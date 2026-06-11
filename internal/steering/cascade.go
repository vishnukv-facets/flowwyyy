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
		log:              func(f string, a ...any) { fmt.Fprintf(os.Stderr, "[steering] "+f+"\n", a...) },
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
	pol := c.autonomy()
	decision := pol.Evaluate(v.SuggestedAction, v.Confidence)
	audit.decision = decision.Decision
	audit.reason = decision.Reason
	if !decision.Allowed {
		return audit
	}
	item, err := flowdb.GetFeedItem(c.DB, feedID)
	if err != nil {
		audit.decision = "failed"
		audit.reason = fmt.Sprintf("auto-act %s could not load feed item %s: %v", v.SuggestedAction, feedID, err)
		return audit
	}
	if err := ApplyAction(ctx, c.DB, item, v.SuggestedAction, pol, false); err != nil {
		audit.decision = "failed"
		audit.reason = fmt.Sprintf("auto-act %s for %s failed: %v", v.SuggestedAction, feedID, err)
		c.log("%s", audit.reason)
		return audit
	}
	audit.decision = "acted"
	audit.reason = decision.Reason
	c.log("auto-acted %s on %s (confidence %.2f >= threshold)", v.SuggestedAction, feedID, v.Confidence)
	return audit
}

// resolveOnOperatorReply stands down any open feed item for a thread when the
// operator posts there themselves (a self-authored event). They handled it
// directly — outside flow — so the surfaced "needs your reply" card is now
// stale. Fires on the live event AND on backfill replay of the operator's own
// message, so it covers new and recently-missed replies. Connector-blind:
// works for Slack threads/DMs and GitHub comments alike.
func (c *Cascade) resolveOnOperatorReply(ev monitor.InboundEvent) {
	key := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	if key == "" {
		return
	}
	n, err := flowdb.ResolveOpenFeedItemsByThread(c.DB, key, c.now().UTC().Format(time.RFC3339))
	if err == nil && n > 0 {
		c.log("operator handled %s directly; resolved %d open feed item(s)", key, n)
	}
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
			c.resolveOnOperatorReply(ev)
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
			c.emitTrace(tr, start)
			return nil
		}
		det2 := c.applyExistingTaskMatch(&v2, ev)
		if note := gateWeakSemanticForward(&v2, det2); note != "" {
			tr.Error = appendCascadeError(tr.Error, note)
		}
		id, surfaced, werr := c.writeFeed(v2, ev, pack)
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
	v3, err := DeepTriageWithContextAndHints(c.deepStreamCtx(ctx, tr, start), in, taskIndex, pack, hints)
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
		c.emitTrace(tr, start)
		return nil
	}
	id, surfaced, werr := c.writeFeed(v3, ev, pack)
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
				c.resolveOnOperatorReply(ev)
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
	case "surfaced", "dropped", "error":
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
func (c *Cascade) writeFeed(v Verdict, ev monitor.InboundEvent, pack ThreadContext) (string, bool, error) {
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
	return id, surfaced, nil
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

func deepBudgetPerHour() int {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_DEEP_BUDGET_PER_HOUR")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 40
}

func classifierBudgetPerHour() int {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_CLASSIFIER_BUDGET_PER_HOUR")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 30
}

func classifierFailureCooldown() time.Duration {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_CLASSIFIER_FAILURE_COOLDOWN")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Minute
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
