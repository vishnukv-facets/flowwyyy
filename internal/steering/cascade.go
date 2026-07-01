// internal/steering/cascade.go
package steering

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// Cascade is the triage brain: Stage 0 (free) -> Stage 1 (cheap relevance) ->
// Stage 2 (cheap score) -> Stage 3 (deep), gated by a verdict cache and an
// hourly deep-triage budget, surfacing survivors to the Attention feed.
//
// P1.2a is SURFACE-ONLY: it never acts on a verdict, only writes a feed row.
type Cascade struct {
	DB     *sql.DB
	Config WatchConfig
	// ConfigFn, when set, is called per Observe to read the watch-config live
	// (so Mission Control settings changes take effect without a restart). When
	// nil, the static Config is used. NewCascade leaves it nil; serve wiring
	// sets it to WatchConfigFromEnv.
	ConfigFn func() WatchConfig

	// TextClean, when set, rewrites connector markup (e.g. Slack <@U…> mentions)
	// to human names BEFORE the text reaches the classifier/LLM and the trace,
	// so summaries/replies never surface raw IDs. nil = identity. Connector-blind:
	// a Slack-aware cleaner is a no-op on GitHub text.
	TextClean func(ctx context.Context, text string) string

	// ResolveUserName, when set, resolves connector user IDs (Slack U… IDs) to
	// display names for deterministic task-impact hints. It must return "" when
	// resolution fails; raw IDs are not useful person names.
	ResolveUserName func(context.Context, string) string

	// Autonomy is the per-action auto-act policy. AutonomyFn, when set, reads it
	// live (so Settings changes take effect without a restart); else the static
	// Autonomy is used. NewCascade seeds Autonomy with DefaultAutonomy (every
	// action OFF — surface-only). After surfacing a verdict the cascade attempts
	// the action through the autonomy gate (manual=false), so it only ever acts
	// on its own when the operator opted that action in above its threshold.
	Autonomy   AutonomyPolicy
	AutonomyFn func() AutonomyPolicy

	// CalibratorFn, when set, supplies the live confidence calibrator (raw model
	// score → empirical P(operator agrees) per action×band, learned from
	// attention_feedback). maybeAutoAct gates on the CALIBRATED score so the
	// operator's thresholds mean "minimum probability I'd agree", not "minimum
	// raw model number". NewCascade leaves it nil; serve wiring sets it to a
	// loader. nil (or a nil calibrator, or a band with too-thin history) ⇒ the
	// raw score is used unchanged, so behavior degrades safely to pre-calibration.
	CalibratorFn func() *ConfidenceCalibrator

	now    func() time.Time
	newID  func() string
	cache  *verdictCache
	budget *budgetGuard
	// classifierBudget caps Stage 1/2 Claude subprocess turns. The stages are
	// "cheap" compared with deep triage, but still CPU-heavy under connector
	// floods because each allowed turn shells out to claude.
	classifierBudget         *budgetGuard
	classifierMu             sync.Mutex
	classifierCooldownUntil  time.Time
	classifierCooldownReason string
	log                      func(string, ...any)
	// trace records one decision-trace row per observed event. NewCascade
	// defaults it to a writer that inserts into the steering_trace table; tests
	// swap it to capture rows in memory.
	trace func(flowdb.SteeringTrace)

	// Progress, when set, receives a StageEvent at each cascade boundary so the
	// server can stream live triage progress to Mission Control. NewCascade
	// leaves it nil (no-op); serve wiring sets it. It is never load-bearing — a
	// nil hook changes nothing about triage behavior.
	Progress func(StageEvent)

	// FetchContext deterministically loads connector context for Stage 3. Nil
	// means context fetching is unavailable; the cascade writes an explicit
	// event-only fallback pack rather than asking the model to fetch context.
	FetchContext func(context.Context, monitor.InboundEvent) (ThreadContext, error)

	// KBDir is the operator's knowledge-base directory (…/kb). When set, the
	// operator-reply learning path distills durable facts out of hand-written
	// replies into it. Empty (NewCascade default; tests) ⇒ KB capture is skipped.
	// Best-effort and live-only — never load-bearing for triage.
	KBDir string

	// SessionSink, when set AND SteererSessionsEnabled(), receives Stage-0
	// survivors for delivery into the channel's live steerer session instead of
	// the stateless deep-triage stages (GAP-1). nil ⇒ the cold path is used. serve
	// wiring sets it to *server.Server; NewCascade leaves it nil. Never
	// load-bearing: any delivery error falls back to DeepTriageIncremental.
	SessionSink SteererSessionSink

	// GitHubCanonicalNum collapses a linked GitHub PR↔issue pair to one canonical
	// number so both reach ONE steerer chat (GAP-4). nil ⇒ identity (each keys on
	// its own number); the dispatcher's ownership gate already routes owned/linked
	// pairs to their work-session before the steerer, so identity is the safe
	// default. A real resolver can be wired later if un-owned linked pairs recur.
	GitHubCanonicalNum CanonicalGitHubNumFunc
}

// NewCascade builds a Cascade with production defaults (real clock, random IDs,
// a 10-minute verdict TTL, and an env-configurable hourly deep-triage budget).
func NewCascade(db *sql.DB, cfg WatchConfig) *Cascade {
	return &Cascade{
		DB:               db,
		Config:           cfg,
		now:              time.Now,
		newID:            randomID,
		cache:            newVerdictCache(10 * time.Minute),
		budget:           newBudgetGuard(deepBudgetPerHour()),
		classifierBudget: newBudgetGuard(classifierBudgetPerHour()),
		log:              monitor.NewStderrLogger("[steering] "),
		trace:            func(t flowdb.SteeringTrace) { _ = flowdb.InsertSteeringTrace(db, t) },
		Autonomy:         DefaultAutonomy(),
	}
}

// autonomy returns the live autonomy policy (AutonomyFn when set, else the
// static Autonomy, else the all-off default).
func (c *Cascade) autonomy() AutonomyPolicy {
	if c.AutonomyFn != nil {
		return c.AutonomyFn()
	}
	if c.Autonomy != nil {
		return c.Autonomy
	}
	return DefaultAutonomy()
}

// calibrator returns the live confidence calibrator (CalibratorFn when set, else
// nil). A nil calibrator is a valid no-op: Calibrate returns the raw score.
func (c *Cascade) calibrator() *ConfidenceCalibrator {
	if c.CalibratorFn != nil {
		return c.CalibratorFn()
	}
	return nil
}

func (c *Cascade) watchConfig() WatchConfig {
	cfg := c.Config
	if c.ConfigFn != nil {
		cfg = c.ConfigFn()
	}
	return cfg
}

// ShouldObserve is the dispatcher's cheap prefilter. It filters only event
// kinds that can never be operator-relevant. Full Stage 0 still owns
// mute/self/bot/drop reasoning and Stage 1+ owns ambiguous relevance.
func (c *Cascade) ShouldObserve(ev monitor.InboundEvent) bool {
	if connectorOf(ev) == "github" {
		return true
	}
	if ev.Kind != "message" && ev.Kind != "app_mention" {
		return false
	}
	return true
}

type autonomyTrace struct {
	action, decision, reason string
}

func (a autonomyTrace) applyTo(t *flowdb.SteeringTrace) {
	t.AutonomyAction = a.action
	t.AutonomyDecision = a.decision
	t.AutonomyReason = a.reason
}

// maybeAutoAct attempts the surfaced verdict's action through the autonomy gate.
// ApplyAction with manual=false enforces the policy: it acts only when the
// operator enabled that action above its confidence threshold, otherwise returns
// ErrAutonomyDenied (a no-op). Outward sends (reply/afk_reply) stay
// operator-confirmed until the AFK/presence work lands. The feed row is already
// written, so a denied or failed auto-act leaves it surfaced for the operator.
func (c *Cascade) maybeAutoAct(ctx context.Context, feedID string, v Verdict) autonomyTrace {
	audit := autonomyTrace{action: string(v.SuggestedAction)}
	if feedID == "" {
		return audit
	}
	// A correction-triggered re-triage NEVER auto-acts: the operator just supplied
	// context and the corrected verdict always comes back for their review (operator
	// decision). The card is already (re)written; we only skip the outward action.
	if autoActSuppressed(ctx) {
		audit.decision = "suppressed"
		audit.reason = "auto-act suppressed: operator-correction re-triage always re-surfaces for review"
		return audit
	}
	pol := c.autonomy()
	// Gate on the CALIBRATED confidence: the operator's per-action threshold means
	// "minimum probability I'd agree with this action", learned from their past
	// feedback (steerer-confidence-calibration). A nil calibrator or a band with
	// too-thin history returns the raw model score unchanged, so the gate degrades
	// safely to the pre-calibration behavior.
	gateConf, grounded := c.calibrator().Calibrate(v.SuggestedAction, v.Confidence)
	decision := pol.Evaluate(v.SuggestedAction, gateConf)
	audit.decision = decision.Decision
	audit.reason = decision.Reason + confidenceProvenance(grounded, gateConf, v.Confidence)
	if !decision.Allowed {
		return audit
	}
	item, err := flowdb.GetFeedItem(c.DB, feedID)
	if err != nil {
		audit.decision = "failed"
		audit.reason = fmt.Sprintf("auto-act %s could not load feed item %s: %v", v.SuggestedAction, feedID, err)
		return audit
	}
	// capture_kb spawns a `claude -p` subprocess (seconds to minutes) — never block
	// the cascade goroutine on it. Dispatch detached with its own timeout, mirroring
	// the operator capture path (server/attention.go). The card flips to acted only
	// when the agent confirms the write; on failure it stays surfaced for review.
	if v.SuggestedAction == ActionCaptureKB {
		if strings.TrimSpace(c.KBDir) == "" {
			audit.decision = "skipped"
			audit.reason = "capture_kb auto-act enabled but no KB directory configured" + confidenceProvenance(grounded, gateConf, v.Confidence)
			return audit
		}
		go func(it flowdb.FeedItem) {
			bctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := ApplyActionAuto(bctx, c.DB, it, ActionCaptureKB, c.KBDir, pol, gateConf); err != nil {
				c.log("auto-capture-kb for %s failed: %v", it.ID, err)
				return
			}
			c.log("auto-captured KB from %s (calibrated %.2f)", it.ID, gateConf)
		}(item)
		audit.decision = "dispatched"
		audit.reason = "auto-capture_kb dispatched (async); " + decision.Reason + confidenceProvenance(grounded, gateConf, v.Confidence)
		return audit
	}
	if err := ApplyActionAuto(ctx, c.DB, item, v.SuggestedAction, c.KBDir, pol, gateConf); err != nil {
		audit.decision = "failed"
		audit.reason = fmt.Sprintf("auto-act %s for %s failed: %v", v.SuggestedAction, feedID, err)
		c.log("%s", audit.reason)
		return audit
	}
	audit.decision = "acted"
	audit.reason = decision.Reason + confidenceProvenance(grounded, gateConf, v.Confidence)
	c.log("auto-acted %s on %s (calibrated %.2f >= threshold)", v.SuggestedAction, feedID, gateConf)
	return audit
}

// confidenceProvenance annotates an autonomy trace reason with how the gating
// confidence was derived, so `flow attention trace` shows whether a decision rode
// the calibrated agreement rate or fell back to the raw model number.
func confidenceProvenance(grounded bool, calibrated, raw float64) string {
	if grounded {
		return fmt.Sprintf(" [calibrated %.2f from raw %.2f]", calibrated, raw)
	}
	return fmt.Sprintf(" [raw %.2f; calibration ungrounded]", raw)
}

// learnFromOperatorReply is the learning path for a self-authored message Stage 0
// dropped. The operator wrote a reply BY HAND on a watched thread flow already
// triaged; their words are the strongest signal of how the thread resolved and of
// the operator's voice. Stage 0 still drops the event (it is never surfaced as a
// card); this reacts to that drop. Connector-blind: Slack threads/DMs and GitHub
// comments alike.
//
// GATE — only threads flow already deep-triaged learn (reusing priorUnderstanding's
// "has decision" test): recordThreadDecision runs only past the Stage-0 scope gate
// (writeFeed + the deep-triage drop paths), so "has decision" structurally implies
// "was watched". New / unwatched / shallow-dropped threads have no such state and
// fall straight through — preserving the plain self-drop with NO row created (no
// firehose of thread-state rows for the operator's everyday chatter).
//
// DELIVERY DEPENDENCY: this only fires if the operator's own message actually
// reaches the cascade — Slack needs user-token event subs (message.im/mpim and
// watched-channel events on behalf of the user) and/or the backfill poller; GitHub
// needs the comment delivered by the App webhook. AGENT-ECHO LIMITATION: a reply
// the agent itself posted via send_reply rides the operator's user token and is
// indistinguishable from a hand-typed one at the socket (no app_id/footer). The TS
// de-dup below catches exact replays/backfill double-processing; it cannot tell an
// agent-sent reply from a hand-typed one of different text — accepted as a known
// gap, not detectable with current payload data.
func (c *Cascade) learnFromOperatorReply(ctx context.Context, ev monitor.InboundEvent, origin string) {
	key := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	if key == "" {
		return
	}
	prior, hadPrior, err := flowdb.GetThreadState(c.DB, key)
	if err != nil {
		c.log("thread-state: get %s for operator reply: %v", key, err)
		return
	}
	// Gate: only learn on threads flow already triaged (same test as priorUnderstanding).
	if !hadPrior || threadStateEmpty(prior) {
		return
	}
	// De-dup against replay / backfill double-processing of the same message
	// (self-authored events drop before the verdict cache, so they aren't deduped
	// upstream).
	for _, r := range prior.OperatorReplies {
		if ev.TS != "" && r.TS == ev.TS {
			return
		}
	}
	now := c.now().UTC().Format(time.RFC3339)
	text := c.cleanText(ctx, ev.Text)

	// Agent-sent echo: a reply the agent posted on the operator's behalf rides the
	// operator's user token, so the socket re-delivers it as a self-authored event.
	// It was already recorded as an approved send_reply action when posted —
	// re-learning it here would double-count the calibration signal and capture the
	// agent's own words into the KB as if the operator hand-wrote them. Recognize it,
	// stand down any still-open card (idempotent; covers the race where the echo
	// beats `attention sent` bookkeeping), and stop short of operator-learning.
	if c.isAgentSentEcho(key, text) {
		if n, err := flowdb.ResolveOpenFeedItemsByThread(c.DB, key, now); err == nil && n > 0 {
			c.log("agent-sent reply echoed on %s; resolved %d open feed item(s)", key, n)
		}
		c.log("self-authored reply on %s recognized as agent-sent echo; not re-learning", key)
		return
	}

	// 1. Persist the operator's own reply into the running understanding.
	if err := flowdb.AppendThreadOperatorReply(c.DB, key, flowdb.ThreadOperatorReply{
		At: now, TS: ev.TS, Author: ev.UserID, Text: text,
	}); err != nil {
		c.log("thread-state: record operator reply for %s: %v", key, err)
	}

	// 2. The operator handled it outside flow — stand down any open card and
	//    recover the prior card (for the matched task + the calibration signal).
	if n, err := flowdb.ResolveOpenFeedItemsByThread(c.DB, key, now); err == nil && n > 0 {
		c.log("operator handled %s directly; resolved %d open feed item(s)", key, n)
	}
	priorCard, hadCard, cerr := flowdb.LatestFeedItemByThread(c.DB, key)
	if cerr != nil {
		c.log("thread-state: latest card for %s: %v", key, cerr)
	}

	// 3. Record the resolution as an operator action in the running understanding —
	//    the uniform, always-on calibration signal (prior decision lives in the same
	//    thread-state row) and a marker incremental Stage 3 reads next time.
	matched := ""
	if hadCard {
		matched = strings.TrimSpace(priorCard.MatchedTask)
	}
	if err := flowdb.AppendThreadOperatorAction(c.DB, key, flowdb.ThreadOperatorAction{
		At: now, Action: "operator_reply", Outcome: "handled", LinkedTask: matched,
	}); err != nil {
		c.log("thread-state: record operator action for %s: %v", key, err)
	}

	// 4. Calibration signal: agent's prior suggestion vs the operator's hand action.
	//    Needs a feed item for the id; when the thread was deep-dropped without a
	//    card the signal still lives in the operator action recorded above.
	if hadCard {
		fb := flowdb.AttentionFeedbackFromFeed(priorCard, "operator_reply", flowdb.OutcomeOperatorHandled, "", now)
		if err := flowdb.RecordAttentionFeedback(c.DB, fb); err != nil {
			c.log("attention-feedback: record operator-reply calibration for %s: %v", key, err)
		}
	}

	// 5. KB capture — live only (skip on backfill to avoid burst LLM spend) and only
	//    for substantive text. Best-effort: a failure never affects triage.
	if origin == "live" && strings.TrimSpace(c.KBDir) != "" && substantive(text) {
		if err := captureOperatorReplyKB(ctx, key, connectorOf(ev), text, c.KBDir); err != nil {
			c.log("kb-capture: operator reply on %s: %v", key, err)
		}
	}
}

// isAgentSentEcho reports whether a self-authored reply is the echo of a reply the
// agent itself posted on the operator's behalf (send_reply rides the operator's
// user token, so the socket re-delivers it as self-authored). Detected by matching
// the normalized text against drafts the agent sent on this thread in the last hour
// — the only signal available, since a user-token post carries no app/bot marker
// when it echoes back. A user-token post echoes within seconds, so an hour is
// generous slack while staying short enough not to match an unrelated old draft.
// ponytail: exact normalized text+time match; mention-render drift could miss an
// echo (worst case: one agent reply mis-recorded as operator) — tighten only if
// that shows up.
func (c *Cascade) isAgentSentEcho(threadKey, text string) bool {
	want := normalizeReplyText(text)
	if want == "" {
		return false
	}
	since := c.now().UTC().Add(-time.Hour).Format(time.RFC3339)
	drafts, err := flowdb.RecentAgentReplyDrafts(c.DB, threadKey, since)
	if err != nil {
		c.log("attention-feedback: recent agent drafts for %s: %v", threadKey, err)
		return false
	}
	for _, d := range drafts {
		if normalizeReplyText(d) == want {
			return true
		}
	}
	return false
}

// normalizeReplyText lowercases and collapses whitespace so an agent draft and its
// socket echo compare equal despite formatting noise.
func normalizeReplyText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Observe runs the cascade for one live inbound event. Errors from a stage
// abort this event's processing but are returned for logging; a dropped event
// (by any stage) returns nil. Every observed event emits exactly one
// decision-trace row.
func (c *Cascade) Observe(ctx context.Context, ev monitor.InboundEvent) error {
	return c.observe(ctx, ev, "live")
}

// ObserveBackfill is identical to Observe but tags traces with origin
// "backfill" (used by the steerer's catch-up replay).
func (c *Cascade) ObserveBackfill(ctx context.Context, ev monitor.InboundEvent) error {
	return c.observe(ctx, ev, "backfill")
}

// buildSteererDelivery assembles the lean per-event payload for a steerer session:
// cleaned text + the deterministic context pack that anchors this specific message.
func (c *Cascade) buildSteererDelivery(ctx context.Context, ev monitor.InboundEvent, contextOnly bool) SteererDelivery {
	return SteererDelivery{
		Source:      connectorOf(ev),
		Channel:     ev.Channel,
		ChannelType: ev.ChannelType,
		TS:          ev.TS,
		ThreadTS:    ev.ThreadTS,
		Author:      ev.UserID,
		Text:        c.cleanText(ctx, ev.Text),
		Context:     c.contextPack(ctx, ev),
		ContextOnly: contextOnly,
	}
}

// ObserveSelfAuthored feeds a self-authored bot-echo event (dropped at the top of
// Dispatch today) into the channel's steerer session as a context_only delivery
// confirmation (GAP-10) so the session knows its reply landed and stops re-nagging.
// No-op (returns nil) when sessions are off, no sink is wired, or the event has no
// session key — never load-bearing.
func (c *Cascade) ObserveSelfAuthored(ctx context.Context, ev monitor.InboundEvent) error {
	if !SteererSessionsEnabled() || c.SessionSink == nil {
		return nil
	}
	key, ok := sessionKeyForEvent(ev, c.GitHubCanonicalNum)
	if !ok {
		return nil
	}
	start := c.now()
	tr := c.newTrace(ev, "self_echo", c.cleanText(ctx, ev.Text))
	if c.skipDuplicateSessionDelivery(ev, tr, start) {
		return nil
	}
	p := c.buildSteererDelivery(ctx, ev, true)
	p.SelfEcho = true
	if err := c.SessionSink.DeliverToChannelSession(key, p); err != nil {
		c.log("steerer session self-echo delivery failed for %s: %v", key, err)
		tr.Disposition, tr.StageReached, tr.Error = "error", "session", err.Error()
		c.emitTrace(tr, start)
		return nil
	}
	tr.Disposition, tr.StageReached, tr.DropReason = "delivered", "session", "self-echo → context_only"
	c.emitTrace(tr, start)
	return nil
}

// deliverSurvivorToSession hands a Stage-0 survivor to its channel's steerer
// session (GAP-8) when the session model is on. Returns true if the event was
// delivered — the caller MUST stop (no stage1/2/3). On a sink error it records
// the error on tr and returns false (fail-open: fall through to cold triage).
// Shared by the single-event observe() and the batched ObserveBatch() so the
// two paths can't drift — they did once, and backfill kept surfacing digest_only
// FYI cards after the live path moved to sessions.
func (c *Cascade) deliverSurvivorToSession(ctx context.Context, ev monitor.InboundEvent, tr *flowdb.SteeringTrace, start time.Time, cacheKey string) bool {
	if !SteererSessionsEnabled() || c.SessionSink == nil {
		return false
	}
	key, ok := sessionKeyForEvent(ev, c.GitHubCanonicalNum)
	if !ok {
		return false
	}
	if c.skipDuplicateSessionDelivery(ev, tr, start) {
		return true
	}
	if err := c.SessionSink.DeliverToChannelSession(key, c.buildSteererDelivery(ctx, ev, false)); err != nil {
		c.log("steerer session delivery failed for %s: %v; falling back to cold triage", key, err)
		tr.Error = appendCascadeError(tr.Error, "session delivery failed: "+err.Error())
		return false
	}
	c.cache.mark(cacheKey, c.now())
	tr.Disposition, tr.StageReached = "delivered", "session"
	c.emitTrace(tr, start)
	return true
}

// deliverSelfAuthoredToSession feeds a dropped self-authored event into its
// channel session as context_only memory (GAP-10) so the session reasons about
// follow-ups. Returns true if delivered (caller stops); on any miss/error
// returns false so the caller emits the normal Stage-0 drop trace (fail-open).
// Mirror of the inline block observe() used to carry; shared so ObserveBatch
// gets the same behavior.
func (c *Cascade) deliverSelfAuthoredToSession(ctx context.Context, ev monitor.InboundEvent, tr *flowdb.SteeringTrace, start time.Time, cfg WatchConfig) bool {
	if !SteererSessionsEnabled() || c.SessionSink == nil {
		return false
	}
	if !selfAuthoredSessionInScope(ev, cfg) {
		return false
	}
	key, ok := sessionKeyForEvent(ev, c.GitHubCanonicalNum)
	if !ok {
		return false
	}
	if c.skipDuplicateSessionDelivery(ev, tr, start) {
		return true
	}
	if err := c.SessionSink.DeliverToChannelSession(key, c.buildSteererDelivery(ctx, ev, true)); err != nil {
		c.log("steerer session context-only delivery failed for %s: %v", key, err)
		return false
	}
	tr.Disposition, tr.StageReached, tr.DropReason = "delivered", "session", "self-authored → context_only"
	c.emitTrace(tr, start)
	return true
}

func (c *Cascade) skipDuplicateSessionDelivery(ev monitor.InboundEvent, tr *flowdb.SteeringTrace, start time.Time) bool {
	seen, err := flowdb.HasDeliveredSteeringSessionEvent(c.DB, connectorOf(ev), ev.Channel, ev.TS)
	if err != nil {
		c.log("steerer session delivery dedupe check failed for %s/%s: %v", ev.Channel, ev.TS, err)
		return false
	}
	if !seen {
		return false
	}
	tr.Disposition, tr.StageReached, tr.DropReason = "dropped", "cache", "duplicate session delivery"
	c.emitTrace(tr, start)
	return true
}

func selfAuthoredSessionInScope(ev monitor.InboundEvent, cfg WatchConfig) bool {
	probe := ev
	// Re-run Stage 0 as a normal human message so context_only delivery keeps
	// the same DM/mention/watched scope instead of creating sessions for noise.
	probe.UserID = "__flow_non_self_stage0_probe__"
	return Stage0(probe, cfg).Pass
}

// observe is the single-event triage path: Stage 0 → verdict cache →
// single-event Stage 1 relevance, then the shared finishItem tail. It emits a
// trace at every exit.
func (c *Cascade) observe(ctx context.Context, ev monitor.InboundEvent, origin string) error {
	start := c.now()
	cleaned := c.cleanText(ctx, ev.Text)
	tr := c.newTrace(ev, origin, cleaned)
	c.stage(tr, start, "received", "running", connectorOf(ev))
	cfg := c.watchConfig()

	s0 := Stage0(ev, cfg)
	if !s0.Pass {
		if s0.DropReason == "self-authored" {
			c.learnFromOperatorReply(ctx, ev, origin)
			// Per-channel session model (GAP-10): feed the operator's own message to
			// the channel session as context_only memory (never surfaced) so the
			// session reasons correctly about follow-ups.
			if c.deliverSelfAuthoredToSession(ctx, ev, tr, start, cfg) {
				return nil
			}
		}
		tr.Disposition, tr.StageReached, tr.DropReason = "dropped", "stage0", s0.DropReason
		c.emitTrace(tr, start)
		return nil
	}
	tr.ThreadKey = s0.ThreadKey
	c.stage(tr, start, "stage0", "passed", "scope gate passed")
	cacheKey := verdictCacheKey(ev, s0.ThreadKey)
	if c.cache.seenFn(cacheKey, c.now()) {
		tr.Disposition, tr.StageReached, tr.DropReason = "dropped", "cache", "duplicate within verdict TTL"
		c.emitTrace(tr, start)
		return nil
	}

	// Per-channel session model (GAP-8): when enabled, hand the survivor to the
	// channel's live steerer session instead of the stateless deep-triage stages.
	// Placed BEFORE the classifier gate so the session path never shells out. Any
	// sink error falls through to DeepTriageIncremental below (fail-open invariant).
	if c.deliverSurvivorToSession(ctx, ev, tr, start, cacheKey) {
		return nil
	}

	in := ClassifyInput{ThreadKey: s0.ThreadKey, Source: connectorOf(ev), Author: ev.UserID, Text: cleaned}
	if githubActionableSignal(ev, cleaned) {
		t := true
		tr.Stage1Relevant = &t
		tr.Stage1Reason = "deterministic GitHub review signal marked actionable"
		return c.finishItem(ctx, in, tr, start, ev, cacheKey)
	}

	if reason, ok := c.classifierUnavailable(c.now()); ok {
		c.dropForClassifierUnavailable(tr, start, cacheKey, "stage1", reason)
		return nil
	}
	if !c.allowClassifier(c.now()) {
		c.dropForClassifierBudget(tr, start, cacheKey, "stage1")
		return nil
	}
	stage1In := in
	stage1In.ThreadKey = cacheKey
	c.stage(tr, start, "stage1", "running", "relevance check")
	rel, err := Stage1Relevance(ctx, []ClassifyInput{stage1In})
	if err != nil {
		tr.Error = "stage1 advisory failed: " + err.Error()
		c.noteClassifierError(err, c.now())
		tr.Disposition, tr.StageReached = "error", "stage1"
		if c.cache != nil {
			c.cache.mark(cacheKey, c.now())
		}
		c.emitTrace(tr, start)
		return nil
	} else if len(rel) > 0 {
		r := rel[0]
		tr.Stage1Relevant = &r.Relevant
		tr.Stage1Reason = stage1Reason(r)
	}
	return c.finishItem(ctx, in, tr, start, ev, cacheKey)
}

// finishItem runs the per-item tail of the cascade — task index → Stage 2 →
// budget gate → Stage 3 deep triage → feed write — and emits a trace at every
// exit. It assumes Stage 0/cache/Stage 1 have already passed and tr.ThreadKey
// + tr.Stage1Relevant are set.
func (c *Cascade) finishItem(ctx context.Context, in ClassifyInput, tr *flowdb.SteeringTrace, start time.Time, ev monitor.InboundEvent, cacheKey string) error {
	if reason, ok := c.classifierUnavailable(c.now()); ok {
		c.dropForClassifierUnavailable(tr, start, cacheKey, "stage2", reason)
		return nil
	}
	if !c.allowClassifier(c.now()) {
		c.dropForClassifierBudget(tr, start, cacheKey, "stage2")
		return nil
	}
	taskIndex, err := BuildTaskIndex(c.DB)
	if err != nil {
		tr.Disposition, tr.StageReached, tr.Error = "error", "stage1", err.Error()
		c.emitTrace(tr, start)
		return fmt.Errorf("steering: task index: %w", err)
	}

	// Read back the thread's running understanding before triaging so triage is
	// no longer stateless across events — the prior decision plus any operator
	// actions/replies feed the incremental deep-triage prompt below
	// ([[steerer-context-assembly]] layer 2).
	prior, hadPrior, perr := flowdb.GetThreadState(c.DB, in.ThreadKey)
	if perr != nil {
		c.log("thread-state: load %s: %v", in.ThreadKey, perr)
	} else if hadPrior {
		c.log("thread-state: %s has prior decision %q (conf %.2f, %d events) — continuing",
			in.ThreadKey, prior.CurrentAction, prior.CurrentConfidence, prior.EventCount)
	}

	c.stage(tr, start, "stage2", "running", "scoring against tasks")
	v2, err := Stage2Score(ctx, in, taskIndex)
	if err != nil {
		c.noteClassifierError(err, c.now())
		tr.Disposition, tr.StageReached, tr.Error = "error", "stage2", err.Error()
		if c.cache != nil {
			c.cache.mark(cacheKey, c.now())
		}
		c.emitTrace(tr, start)
		return nil
	}
	tr.Stage2Action = string(v2.SuggestedAction)
	tr.Stage2Confidence = v2.Confidence
	pack := c.contextPack(ctx, ev)
	hints, hintErr := BuildTaskImpactHints(c.DB, TaskImpactInput{
		Source: in.Source,
		People: c.taskImpactPeople(ctx, in, ev, pack),
		Text:   in.Text,
	})
	if hintErr != nil {
		tr.Error = appendCascadeError(tr.Error, "task impact hints failed: "+hintErr.Error())
		hints = nil
	}

	// Backpressure: when the deep-triage budget is exhausted, surface the cheap
	// Stage-2 verdict rather than silently deferring. Nothing is lost.
	if !c.budget.allow(c.now()) {
		c.log("deep-triage budget exhausted; surfacing stage2 verdict for %s", in.ThreadKey)
		c.cache.mark(cacheKey, c.now())
		if v2.SuggestedAction == ActionDrop {
			tr.Disposition, tr.StageReached = "dropped", "stage2"
			tr.DropReason = dropReasonFromVerdict("deep budget exhausted; stage2 action=drop", v2)
			tr.FinalAction, tr.FinalConfidence = string(v2.SuggestedAction), v2.Confidence
			c.recordThreadDecision(in.ThreadKey, v2.Summary, v2, ev.TS)
			c.emitTrace(tr, start)
			return nil
		}
		det2 := c.applyExistingTaskMatch(&v2, ev)
		if note := gateWeakSemanticForward(&v2, det2); note != "" {
			tr.Error = appendCascadeError(tr.Error, note)
		}
		id, surfaced, werr := c.writeFeed(ctx, v2, ev, pack)
		if werr == nil && !surfaced {
			tr.Disposition, tr.StageReached = "dropped", "stage2"
			tr.DropReason = "operator dismissed this thread"
			tr.FinalAction, tr.FinalConfidence, tr.FeedItemID = string(v2.SuggestedAction), v2.Confidence, id
			c.emitTrace(tr, start)
			return nil
		}
		tr.Disposition, tr.StageReached = "surfaced", "stage2"
		tr.DropReason = "deep budget exhausted; surfaced stage2 verdict"
		tr.FinalAction, tr.FinalConfidence, tr.FeedItemID = string(v2.SuggestedAction), v2.Confidence, id
		c.maybeAutoAct(ctx, id, v2).applyTo(tr)
		c.emitTrace(tr, start)
		return werr
	}

	c.stage(tr, start, "stage3", "running", "deep triage")
	// Assemble the full context for incremental deep-triage: layer 2 (the thread's
	// prior running understanding) + layer 3 (retrieved KB / past-task history).
	// Both degrade to empty, in which case DeepTriageIncremental cold-classifies.
	inc := IncrementalContext{
		Prior:     priorUnderstanding(prior, hadPrior),
		Retrieved: c.retrieveHistory(in, pack),
	}
	v3, err := DeepTriageIncremental(c.deepStreamCtx(ctx, tr, start), in, taskIndex, pack, hints, inc)
	if err != nil {
		c.log("deep triage failed for %s: %v; falling back to stage2 verdict", in.ThreadKey, err)
		tr.Error = appendCascadeError(tr.Error, "deep triage failed: "+err.Error()+"; fell back to stage2")
		v3 = v2
		tr.StageReached = "stage2"
	} else {
		tr.Stage3Action = string(v3.SuggestedAction)
		tr.Stage3Confidence = v3.Confidence
		tr.StageReached = "stage3"
	}
	c.cache.mark(cacheKey, c.now())
	det3 := c.applyExistingTaskMatch(&v3, ev)
	if note := gateWeakSemanticForward(&v3, det3); note != "" {
		tr.Error = appendCascadeError(tr.Error, note)
	}
	// A deep-triage 'drop' verdict is noise the cascade itself rejected — it
	// belongs in the trace (for transparency), never as a feed card nagging the
	// operator. Stage 2 is advisory while budget is available; it only becomes
	// final on the budget-exhausted fallback path.
	if v3.SuggestedAction == ActionDrop {
		tr.Disposition = "dropped"
		tr.DropReason = dropReasonFromVerdict("deep-triage verdict: drop", v3)
		tr.FinalAction, tr.FinalConfidence = string(v3.SuggestedAction), v3.Confidence
		c.recordThreadDecision(in.ThreadKey, v3.Summary, v3, ev.TS)
		c.emitTrace(tr, start)
		return nil
	}
	id, surfaced, werr := c.writeFeed(ctx, v3, ev, pack)
	if werr == nil && !surfaced {
		// Operator already dismissed this thread/message; re-observation must not
		// resurrect the card or auto-act on it. Record an honest trace and stop.
		tr.Disposition = "dropped"
		tr.DropReason = "operator dismissed this thread"
		tr.FinalAction, tr.FinalConfidence, tr.FeedItemID = string(v3.SuggestedAction), v3.Confidence, id
		c.emitTrace(tr, start)
		return nil
	}
	tr.Disposition = "surfaced"
	tr.FinalAction, tr.FinalConfidence, tr.FeedItemID = string(v3.SuggestedAction), v3.Confidence, id
	c.maybeAutoAct(ctx, id, v3).applyTo(tr)
	c.emitTrace(tr, start)
	return werr
}

// matchExistingTask returns the slug of a flow task already tracking this
// event's thread — via the GitHub link tag (gh-pr:/gh-issue:owner/repo#N, which
// gitHubEventToInboxEvent stashes in ThreadTS) for GitHub, or the
// slack-thread:<channel>:<thread_ts> tag for Slack — preferring a non-done task.
// Package var so tests can stub it.
var matchExistingTask = func(db *sql.DB, ev monitor.InboundEvent) (string, bool) {
	if db == nil {
		return "", false
	}
	var tags []string
	if connectorOf(ev) == "github" {
		tags = append(tags, strings.TrimSpace(ev.ThreadTS)) // the LinkTag, e.g. gh-pr:owner/repo#550
	} else {
		for _, key := range slackTaskKeys(ev) {
			tags = append(tags, monitor.SlackThreadTagPrefix+key)
		}
	}
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			continue
		}
		// IncludeArchived: an archived task is still the canonical tracker for its
		// thread/PR — archiving only declutters the active list, it doesn't stop
		// tracking. Without this, a new comment on an archived-but-open PR matches
		// nothing and the cascade suggests make_task instead of forwarding.
		tasks, err := flowdb.ListTasks(db, flowdb.TaskFilter{Tag: flowdb.NormalizeTag(tag), IncludeArchived: true})
		if err != nil || len(tasks) == 0 {
			continue
		}
		for _, t := range tasks {
			if t != nil && t.Status != "done" {
				return t.Slug, true
			}
		}
		return tasks[0].Slug, true
	}
	return "", false
}

func slackTaskKeys(ev monitor.InboundEvent) []string {
	keys := []string{}
	if key := monitor.ThreadKey(ev.Channel, ev.ThreadTS); key != "" {
		keys = append(keys, key)
	}
	if ref, ok := ev.SharedRef(); ok {
		keys = append(keys, ref.ThreadKeys()...)
	}
	return keys
}

// applyExistingTaskMatch sets MatchedTask when a task already tracks this thread
// (a deterministic thread-tag / PR-link match), and rewrites a would-be
// duplicate make_task into a forward. Returns true when a deterministic match
// was applied — callers use this to distinguish a trusted thread link from the
// classifier's *semantic* guess, which is gated separately (see
// gateWeakSemanticForward).
func (c *Cascade) applyExistingTaskMatch(v *Verdict, ev monitor.InboundEvent) bool {
	if slug, ok := matchExistingTask(c.DB, ev); ok {
		v.MatchedTask = slug
		if v.SuggestedAction == ActionMakeTask {
			v.SuggestedAction = ActionForward
		}
		return true
	}
	return false
}

// forwardMatchFloor is the minimum confidence for surfacing a *semantic* forward
// into an existing task — a match the classifier inferred from topic, with no
// deterministic thread/PR link. Below it, a thematic-only match ("both are
// migrations") retrofits unrelated threads onto the wrong task, so the cascade
// downgrades it to a digest_only FYI. Tunable via
// FLOW_STEERING_FORWARD_MIN_CONFIDENCE (default 0.6).
func forwardMatchFloor() float64 {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_FORWARD_MIN_CONFIDENCE")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return 0.6
}

// gateWeakSemanticForward stops the steerer from retrofitting loosely-related
// threads onto existing tasks. A forward whose match is the classifier's
// semantic guess (no deterministic thread/PR link) and whose confidence is below
// forwardMatchFloor is downgraded to a digest_only FYI with the match cleared —
// it still surfaces, but as "noteworthy", not as "forward to <wrong task>".
// Deterministic thread matches are always trusted and never gated. Returns a
// short audit note when it fired, else "".
func gateWeakSemanticForward(v *Verdict, deterministic bool) string {
	if deterministic || v.SuggestedAction != ActionForward {
		return ""
	}
	if strings.TrimSpace(v.MatchedTask) == "" || v.Confidence >= forwardMatchFloor() {
		return ""
	}
	prev := v.MatchedTask
	v.SuggestedAction = ActionDigestOnly
	v.MatchedTask = ""
	return fmt.Sprintf("weak semantic forward to %q (confidence %.2f < %.2f floor) downgraded to digest_only", prev, v.Confidence, forwardMatchFloor())
}

// ObserveBatch triages a batch of events with a SINGLE batched Stage 1
// relevance call (the rest is per-item). Used by the steerer backfill, where
// many events arrive at once. Each event still emits exactly one trace.
func (c *Cascade) ObserveBatch(ctx context.Context, evs []monitor.InboundEvent) error {
	cfg := c.Config
	if c.ConfigFn != nil {
		cfg = c.ConfigFn()
	}
	type pending struct {
		in       ClassifyInput
		stage1In ClassifyInput
		cacheKey string
		tr       *flowdb.SteeringTrace
		start    time.Time
		ev       monitor.InboundEvent
	}
	var survivors []pending
	var inputs []ClassifyInput
	var firstErr error
	for _, ev := range evs {
		start := c.now()
		cleaned := c.cleanText(ctx, ev.Text)
		tr := c.newTrace(ev, "backfill", cleaned)
		s0 := Stage0(ev, cfg)
		if !s0.Pass {
			if s0.DropReason == "self-authored" {
				c.learnFromOperatorReply(ctx, ev, "backfill")
				// GAP-10 parity with observe(): feed the operator's own backfilled
				// message into the channel session as context_only memory.
				if c.deliverSelfAuthoredToSession(ctx, ev, tr, start, cfg) {
					continue
				}
			}
			tr.Disposition, tr.StageReached, tr.DropReason = "dropped", "stage0", s0.DropReason
			c.emitTrace(tr, start)
			continue
		}
		tr.ThreadKey = s0.ThreadKey
		cacheKey := verdictCacheKey(ev, s0.ThreadKey)
		if c.cache.seenFn(cacheKey, c.now()) {
			tr.Disposition, tr.StageReached, tr.DropReason = "dropped", "cache", "duplicate within verdict TTL"
			c.emitTrace(tr, start)
			continue
		}
		// GAP-8 parity with observe(): when the session model is on, hand the
		// survivor to its channel session instead of running stage1/2/3 here.
		// Without this, backfilled messages skipped the session and surfaced as
		// digest_only FYI cards — the high-bar fix never reached them.
		if c.deliverSurvivorToSession(ctx, ev, tr, start, cacheKey) {
			continue
		}
		in := ClassifyInput{ThreadKey: s0.ThreadKey, Source: connectorOf(ev), Author: ev.UserID, Text: cleaned}
		if githubActionableSignal(ev, cleaned) {
			t := true
			tr.Stage1Relevant = &t
			tr.Stage1Reason = "deterministic GitHub review signal marked actionable"
			if e := c.finishItem(ctx, in, tr, start, ev, cacheKey); e != nil && firstErr == nil {
				firstErr = e
			}
			continue
		}
		stage1In := in
		stage1In.ThreadKey = cacheKey
		survivors = append(survivors, pending{in: in, stage1In: stage1In, cacheKey: cacheKey, tr: tr, start: start, ev: ev})
		inputs = append(inputs, stage1In)
	}
	if len(inputs) == 0 {
		return firstErr
	}
	if reason, ok := c.classifierUnavailable(c.now()); ok {
		for _, p := range survivors {
			c.dropForClassifierUnavailable(p.tr, p.start, p.cacheKey, "stage1", reason)
		}
		return firstErr
	}
	if !c.allowClassifier(c.now()) {
		for _, p := range survivors {
			c.dropForClassifierBudget(p.tr, p.start, p.cacheKey, "stage1")
		}
		return firstErr
	}
	rel, err := Stage1Relevance(ctx, inputs)
	if err != nil {
		c.noteClassifierError(err, c.now())
		for _, p := range survivors {
			p.tr.Error = "stage1 advisory failed: " + err.Error()
			p.tr.Disposition, p.tr.StageReached = "error", "stage1"
			if c.cache != nil {
				c.cache.mark(p.cacheKey, c.now())
			}
			c.emitTrace(p.tr, p.start)
		}
		return firstErr
	}
	relByKey := make(map[string]RelevanceVerdict, len(rel))
	for _, v := range rel {
		relByKey[v.ThreadKey] = v
	}
	for _, p := range survivors {
		if r, ok := relByKey[p.stage1In.ThreadKey]; ok {
			p.tr.Stage1Relevant = &r.Relevant
			p.tr.Stage1Reason = stage1Reason(r)
		}
		if e := c.finishItem(ctx, p.in, p.tr, p.start, p.ev, p.cacheKey); e != nil && firstErr == nil {
			firstErr = e
		}
	}
	return firstErr
}

func (c *Cascade) allowClassifier(now time.Time) bool {
	if c.classifierBudget == nil {
		return true
	}
	return c.classifierBudget.allow(now)
}

func (c *Cascade) dropForClassifierBudget(tr *flowdb.SteeringTrace, start time.Time, cacheKey, stage string) {
	tr.Disposition = "dropped"
	tr.StageReached = stage
	tr.DropReason = "classifier budget exhausted"
	if c.cache != nil {
		c.cache.mark(cacheKey, c.now())
	}
	c.emitTrace(tr, start)
}

func (c *Cascade) dropForClassifierUnavailable(tr *flowdb.SteeringTrace, start time.Time, cacheKey, stage, reason string) {
	tr.Disposition = "dropped"
	tr.StageReached = stage
	tr.DropReason = "classifier unavailable: " + reason
	if c.cache != nil {
		c.cache.mark(cacheKey, c.now())
	}
	c.emitTrace(tr, start)
}

func (c *Cascade) classifierUnavailable(now time.Time) (string, bool) {
	c.classifierMu.Lock()
	defer c.classifierMu.Unlock()
	if now.Before(c.classifierCooldownUntil) {
		return c.classifierCooldownReason, true
	}
	if !c.classifierCooldownUntil.IsZero() {
		c.classifierCooldownUntil = time.Time{}
		c.classifierCooldownReason = ""
	}
	return "", false
}

func (c *Cascade) noteClassifierError(err error, now time.Time) {
	reason, ok := classifierUnavailableReason(err)
	if !ok {
		return
	}
	c.classifierMu.Lock()
	defer c.classifierMu.Unlock()
	c.classifierCooldownReason = reason
	c.classifierCooldownUntil = now.Add(classifierFailureCooldown())
}

func classifierUnavailableReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "weekly limit"):
		return "weekly limit", true
	case strings.Contains(msg, "rate limit"), strings.Contains(msg, "quota"):
		return "rate/quota limit", true
	case strings.Contains(msg, "auth"), strings.Contains(msg, "login"), strings.Contains(msg, "permission"):
		return "authentication required", true
	default:
		return "", false
	}
}

func verdictCacheKey(ev monitor.InboundEvent, threadKey string) string {
	threadKey = strings.TrimSpace(threadKey)
	if connectorOf(ev) != "github" {
		return eventLevelVerdictCacheKey(ev, threadKey)
	}
	if key := strings.TrimSpace(ev.EventKey); key != "" {
		return threadKey + ":event:" + key
	}
	if !githubEventLevelCacheKind(ev.Kind) {
		return threadKey
	}
	if url := strings.TrimSpace(ev.URL); url != "" {
		return threadKey + ":event-url:" + url
	}
	fingerprint := strings.Join([]string{
		strings.TrimSpace(ev.Kind),
		strings.TrimSpace(ev.TS),
		strings.TrimSpace(ev.UserID),
		strings.TrimSpace(ev.Text),
	}, "\x1f")
	if fingerprint == "\x1f\x1f\x1f" {
		return threadKey
	}
	return threadKey + ":event:" + shortHash(fingerprint)
}

func eventLevelVerdictCacheKey(ev monitor.InboundEvent, threadKey string) string {
	if key := strings.TrimSpace(ev.EventKey); key != "" {
		return threadKey + ":event:" + key
	}
	ts := strings.TrimSpace(ev.TS)
	if ts != "" && ts != strings.TrimSpace(ev.ThreadTS) {
		return threadKey + ":event-ts:" + ts
	}
	return threadKey
}

func githubEventLevelCacheKind(kind string) bool {
	switch kind {
	case "pr_head_updated", "pr_merged", "pr_closed", "pr_review_requested",
		"pr_review_comment", "pr_review_changes_requested", "pr_review_approved",
		"pr_comment", "issue_comment":
		return true
	default:
		return false
	}
}

func githubActionableSignal(ev monitor.InboundEvent, text string) bool {
	if connectorOf(ev) != "github" {
		return false
	}
	switch ev.Kind {
	case "pr_review_comment", "pr_review_changes_requested", "pr_comment", "issue_comment":
	default:
		return false
	}
	author := strings.ToLower(strings.TrimSpace(ev.UserID))
	if !strings.Contains(author, "coderabbit") {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || strings.Contains(lower, "no actionable comments") {
		return false
	}
	for _, marker := range []string{
		"potential issue",
		"actionable comments posted",
		"changes requested",
		"should-fix",
		"must-fix",
		"critical",
		"major",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func stage1Reason(v RelevanceVerdict) string {
	if reason := strings.TrimSpace(v.Reason); reason != "" {
		return reason
	}
	parts := []string{}
	if v.Category != "" {
		parts = append(parts, "category="+strings.TrimSpace(v.Category))
	}
	if v.UrgencyHint != "" {
		parts = append(parts, "urgency="+strings.TrimSpace(v.UrgencyHint))
	}
	return strings.Join(parts, "; ")
}

func dropReasonFromVerdict(prefix string, v Verdict) string {
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		return prefix
	}
	return prefix + ": " + reason
}

func appendCascadeError(existing, msg string) string {
	existing = strings.TrimSpace(existing)
	msg = strings.TrimSpace(msg)
	if existing == "" {
		return msg
	}
	if msg == "" {
		return existing
	}
	return existing + "; " + msg
}

// cleanText rewrites connector markup (Slack <@U…> mentions, etc.) to human
// names before the text reaches the classifier/LLM and the trace. nil = the
// text passes through unchanged.
func (c *Cascade) cleanText(ctx context.Context, text string) string {
	if c.TextClean != nil {
		return c.TextClean(ctx, text)
	}
	return text
}

func (c *Cascade) taskImpactPeople(ctx context.Context, in ClassifyInput, ev monitor.InboundEvent, pack ThreadContext) []string {
	seen := map[string]bool{}
	var people []string
	add := func(raw string) {
		name := c.taskImpactPersonName(ctx, raw)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if seen[key] {
			return
		}
		seen[key] = true
		people = append(people, name)
	}

	add(in.Author)
	add(ev.UserID)
	if pack.Parent != nil {
		add(pack.Parent.Author)
	}
	for _, msg := range pack.Messages {
		add(msg.Author)
	}
	for _, participant := range pack.Participants {
		add(participant)
	}
	return people
}

func (c *Cascade) taskImpactPersonName(ctx context.Context, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if c.ResolveUserName != nil {
		if name := cleanImpactPersonName(c.ResolveUserName(ctx, raw)); usableImpactPersonName(name) {
			return name
		}
	}
	cleaned := raw
	if c.TextClean != nil {
		cleaned = c.TextClean(ctx, raw)
	}
	cleaned = cleanImpactPersonName(cleaned)
	if !usableImpactPersonName(cleaned) {
		return ""
	}
	return cleaned
}

func cleanImpactPersonName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "@")
	return strings.TrimSpace(name)
}

func usableImpactPersonName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || rawSlackPersonID(name) {
		return false
	}
	switch strings.ToLower(name) {
	case "user", "unknown", "the sender", "slack user", "slack participant":
		return false
	default:
		return true
	}
}

func rawSlackPersonID(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if operatorSlackUserIDRE.MatchString(name) && operatorSlackUserIDRE.ReplaceAllString(name, "") == "" {
		return true
	}
	upper := strings.ToUpper(name)
	return strings.HasPrefix(upper, "U_") || strings.HasPrefix(upper, "W_")
}

// newTrace seeds a decision-trace row from the inbound event with the fields
// known before any stage runs. cleaned is the de-ID'd message text (see
// cleanText) used for the stored preview so the trace never surfaces raw IDs.
func (c *Cascade) newTrace(ev monitor.InboundEvent, origin, cleaned string) *flowdb.SteeringTrace {
	return &flowdb.SteeringTrace{
		ID:          c.newID(),
		CreatedAt:   c.now().UTC().Format(time.RFC3339),
		Origin:      origin,
		Source:      connectorOf(ev),
		Channel:     ev.Channel,
		ChannelType: ev.ChannelType,
		Author:      ev.UserID,
		TextPreview: preview(cleaned),
		Model:       classifierModel(),
		TS:          ev.TS,
		TeamID:      ev.TeamID,
		URL:         ev.URL,
	}
}

// emitTrace stamps the latency and hands the finished trace row to the sink.
func (c *Cascade) emitTrace(tr *flowdb.SteeringTrace, start time.Time) {
	tr.LatencyMS = c.now().Sub(start).Milliseconds()
	// Terminal stage event. Every exit path funnels through emitTrace, so this
	// fires exactly once per run with the final disposition — no need to sprinkle
	// terminal emits across the ~10 early-returns.
	c.stage(tr, start, "verdict", verdictStatus(tr.Disposition), verdictDetail(tr))
	c.trace(*tr)
}

// stage emits a live progress signal for one cascade boundary. Nil-safe: with no
// Progress hook (the default) it is a cheap no-op, so triage behavior is
// identical whether or not anyone is watching.
func (c *Cascade) stage(tr *flowdb.SteeringTrace, start time.Time, stage, status, detail string) {
	if c.Progress == nil || tr == nil {
		return
	}
	now := c.now()
	c.Progress(StageEvent{
		RunID:       tr.ID,
		ThreadKey:   tr.ThreadKey,
		Source:      tr.Source,
		Channel:     tr.Channel,
		ChannelType: tr.ChannelType,
		Author:      tr.Author,
		TS:          tr.TS,
		TeamID:      tr.TeamID,
		URL:         tr.URL,
		Stage:       stage,
		Status:      status,
		Detail:      detail,
		At:          now.UTC().Format(time.RFC3339),
		ElapsedMs:   now.Sub(start).Milliseconds(),
	})
}

func verdictStatus(disposition string) string {
	switch disposition {
	case "surfaced", "dropped", "error", "delivered":
		return disposition
	default:
		return "done"
	}
}

func verdictDetail(tr *flowdb.SteeringTrace) string {
	if tr.Error != "" {
		return tr.Error
	}
	if tr.DropReason != "" {
		return tr.DropReason
	}
	if tr.FinalAction != "" {
		if tr.FinalConfidence > 0 {
			return fmt.Sprintf("%s · conf %.2f", tr.FinalAction, tr.FinalConfidence)
		}
		return tr.FinalAction
	}
	// Per-channel session model: a delivered survivor has no final cascade action
	// — the chat owns triage downstream. Say so instead of leaving the verdict blank.
	if tr.Disposition == "delivered" {
		return "routed to the channel's steerer chat"
	}
	return ""
}

// deepStreamCtx returns a context that streams Stage 3's model output into the
// live stage view as it generates. No-op (returns ctx unchanged) when nobody is
// watching or streaming is disabled, so the deep-triage runner takes its
// one-shot path. Coalesces by growth so a token-rate stream emits a bounded
// number of progress updates (each carries the full accumulated text, so dropped
// intermediate events are harmless — the store keeps the latest).
func (c *Cascade) deepStreamCtx(ctx context.Context, tr *flowdb.SteeringTrace, start time.Time) context.Context {
	if c.Progress == nil || !streamingEnabled() {
		return ctx
	}
	var buf strings.Builder
	lastLen := 0
	return withStreamSink(ctx, func(delta string) {
		buf.WriteString(delta)
		if buf.Len()-lastLen < 24 {
			return
		}
		lastLen = buf.Len()
		c.stageStream(tr, start, "stage3", buf.String())
	})
}

// stageStream emits a streaming update for an in-flight stage (Status "running"
// with the accumulated model text). The server folds it into the existing stage
// row in place rather than appending.
func (c *Cascade) stageStream(tr *flowdb.SteeringTrace, start time.Time, stage, text string) {
	if c.Progress == nil || tr == nil {
		return
	}
	now := c.now()
	c.Progress(StageEvent{
		RunID:     tr.ID,
		ThreadKey: tr.ThreadKey,
		Source:    tr.Source,
		Stage:     stage,
		Status:    "running",
		Stream:    text,
		At:        now.UTC().Format(time.RFC3339),
		ElapsedMs: now.Sub(start).Milliseconds(),
	})
}

// preview trims and truncates message text for the trace (operator's own data —
// safe to store; just keep rows small).
func preview(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// writeFeed maps a Verdict to a surface-only ('new') Attention feed row and
// returns the upserted item's id plus whether it actually surfaced a live card.
// surfaced == false means the operator already dismissed this thread/message and
// the upsert left it dismissed — the caller must not re-surface or auto-act.
func (c *Cascade) writeFeed(_ context.Context, v Verdict, ev monitor.InboundEvent, pack ThreadContext) (string, bool, error) {
	item := flowdb.FeedItem{
		ID:                c.newID(),
		Source:            v.Source,
		ThreadKey:         v.ThreadKey,
		Summary:           SanitizeOperatorText(v.Summary),
		SuggestedAction:   string(v.SuggestedAction),
		MatchedTask:       v.MatchedTask,
		SuggestedProject:  v.SuggestedProject,
		SuggestedPriority: v.SuggestedPriority,
		Urgency:           string(v.Urgency),
		IsVIP:             v.IsVIP,
		Confidence:        v.Confidence,
		Draft:             SanitizeOperatorText(v.Draft),
		Reason:            SanitizeOperatorText(v.Reason),
		ContextJSON:       contextJSON(pack),
		Channel:           ev.Channel,
		ChannelType:       ev.ChannelType,
		Author:            ev.UserID,
		TS:                ev.TS,
		TeamID:            ev.TeamID,
		URL:               ev.URL,
		Status:            "new",
		CreatedAt:         c.now().UTC().Format(time.RFC3339),
	}
	if item.SuggestedAction == "" {
		item.SuggestedAction = string(ActionDrop)
	}
	id, surfaced, err := flowdb.UpsertFeedItemSurfaced(c.DB, item)
	if err != nil {
		return "", false, fmt.Errorf("steering: write feed item: %w", err)
	}
	// Accumulate the thread's running understanding under the SAME (post-club)
	// key the card uses. Recorded regardless of `surfaced`: a dismissed thread
	// with fresh activity still advances the understanding, it just doesn't
	// re-surface a card.
	c.recordThreadDecision(item.ThreadKey, item.Summary, v, ev.TS)
	return id, surfaced, nil
}

// recordThreadDecision persists the latest verdict into the thread's running
// understanding (attention_thread_state) keyed by threadKey. Best-effort: a
// failure is logged, never fatal to triage. Callers pass the canonical key the
// card uses — item.ThreadKey (post-club) on the surfaced path, in.ThreadKey on
// the drop paths — never the raw verdict key.
func (c *Cascade) recordThreadDecision(threadKey, summary string, v Verdict, ts string) {
	if err := flowdb.RecordThreadDecision(c.DB, flowdb.ThreadDecision{
		ThreadKey:  threadKey,
		Source:     v.Source,
		Action:     string(v.SuggestedAction),
		Confidence: v.Confidence,
		Reason:     v.Reason,
		Summary:    SanitizeOperatorText(summary),
		LastSeenTS: ts,
		At:         c.now().UTC().Format(time.RFC3339),
	}); err != nil {
		c.log("thread-state: record decision for %s: %v", threadKey, err)
	}
}

// priorUnderstanding projects a persisted thread-state row into the model-facing
// PriorUnderstanding fed to incremental Stage 3. Returns nil when there is no
// prior signal at all (the thread's first triage), which makes the prompt fall
// back to cold framing. A row that carries only operator actions/replies (the
// cascade never carded it, but the operator acted/replied) still counts as prior
// understanding worth feeding.
func priorUnderstanding(s flowdb.ThreadState, had bool) *PriorUnderstanding {
	if !had {
		return nil
	}
	hasDecision := s.EventCount > 0 || strings.TrimSpace(s.CurrentAction) != ""
	if !hasDecision && len(s.OperatorActions) == 0 && len(s.OperatorReplies) == 0 && len(s.OperatorCorrections) == 0 {
		return nil
	}
	p := &PriorUnderstanding{
		Action:     s.CurrentAction,
		Confidence: s.CurrentConfidence,
		Reason:     s.CurrentReason,
		Summary:    s.Summary,
		EventCount: s.EventCount,
	}
	for _, a := range s.OperatorActions {
		p.OperatorActions = append(p.OperatorActions, formatOperatorAction(a))
	}
	for _, r := range s.OperatorReplies {
		if t := strings.TrimSpace(r.Text); t != "" {
			p.OperatorReplies = append(p.OperatorReplies, t)
		}
	}
	for _, corr := range s.OperatorCorrections {
		if t := strings.TrimSpace(corr.Text); t != "" {
			p.Corrections = append(p.Corrections, t)
		}
	}
	return p
}

// threadStateEmpty reports that a thread-state row carries no triage decision yet
// (the inverse of priorUnderstanding's "has decision" test). The operator-reply
// learn path uses it to decide a thread was never deep-triaged here.
func threadStateEmpty(s flowdb.ThreadState) bool {
	return s.EventCount == 0 && strings.TrimSpace(s.CurrentAction) == ""
}

func formatOperatorAction(a flowdb.ThreadOperatorAction) string {
	s := a.Action
	if a.Outcome != "" && a.Outcome != a.Action {
		s += " (" + a.Outcome + ")"
	}
	if a.LinkedTask != "" {
		s += " -> " + a.LinkedTask
	}
	return s
}

func (c *Cascade) contextPack(ctx context.Context, ev monitor.InboundEvent) ThreadContext {
	if c.FetchContext == nil {
		return fallbackThreadContext(ev, "unavailable", "context fetcher unavailable", c.cleanText(ctx, ev.Text))
	}
	pack, err := c.FetchContext(ctx, ev)
	if err != nil {
		return fallbackThreadContext(ev, "error", err.Error(), c.cleanText(ctx, ev.Text))
	}
	return normalizeThreadContext(pack, ev)
}

func contextJSON(pack ThreadContext) string {
	b, err := json.Marshal(pack)
	if err != nil {
		return ""
	}
	return string(b)
}

// ---------- verdict cache ----------

// verdictCache suppresses re-triaging the same thread within a TTL window
// (handles Slack re-deliveries, backfill replays, and bursty threads).
type verdictCache struct {
	ttl  time.Duration
	mu   sync.Mutex
	seen map[string]time.Time
}

func newVerdictCache(ttl time.Duration) *verdictCache {
	return &verdictCache{ttl: ttl, seen: map[string]time.Time{}}
}

// seenFn reports whether key was marked within the TTL of now.
func (vc *verdictCache) seenFn(key string, now time.Time) bool {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	at, ok := vc.seen[key]
	return ok && now.Sub(at) < vc.ttl
}

func (vc *verdictCache) mark(key string, now time.Time) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.seen[key] = now
}

// ---------- budget guard ----------

// budgetGuard caps deep-triage calls per rolling hour (cost backpressure).
type budgetGuard struct {
	max   int
	mu    sync.Mutex
	calls []time.Time
}

func newBudgetGuard(maxPerHour int) *budgetGuard {
	return &budgetGuard{max: maxPerHour}
}

// allow records and permits a deep-triage call if fewer than max occurred in
// the last hour; otherwise returns false without recording.
func (b *budgetGuard) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := now.Add(-time.Hour)
	kept := b.calls[:0]
	for _, t := range b.calls {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.calls = kept
	if len(b.calls) >= b.max {
		return false
	}
	b.calls = append(b.calls, now)
	return true
}

// deepBudgetPerHour caps Stage-3 deep-triage turns (the expensive rung — the
// capable model with a fetched context pack). 60/hr ≈ one per minute sustained,
// which clears normal connector volume; bursts spill to the Stage-2 verdict
// (surfaced, nothing lost) rather than being dropped. Override via
// FLOW_STEERING_DEEP_BUDGET_PER_HOUR (0 disables deep triage entirely).
func deepBudgetPerHour() int {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_DEEP_BUDGET_PER_HOUR")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 60
}

// classifierBudgetPerHour caps Stage 1/2 turns. These are cheap batched Haiku
// calls, so the old 30/hr throttled ordinary inbox volume and silently dropped
// events under any real connector activity. 120/hr (one every 30s sustained)
// keeps the cheap front of the cascade open; deep triage remains the rate-limited
// rung. Override via FLOW_STEERING_CLASSIFIER_BUDGET_PER_HOUR (0 disables).
func classifierBudgetPerHour() int {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_CLASSIFIER_BUDGET_PER_HOUR")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 120
}

// classifierFailureCooldown is how long the cascade stops launching classifier
// subprocesses after a quota/auth failure, to avoid hammering a CLI that's
// returning errors. 10m recovers quickly from a transient rate-limit blip while
// still backing off; the old 30m left the steerer deaf for half an hour after a
// momentary limit. Override via FLOW_STEERING_CLASSIFIER_FAILURE_COOLDOWN.
func classifierFailureCooldown() time.Duration {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_CLASSIFIER_FAILURE_COOLDOWN")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 10 * time.Minute
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
