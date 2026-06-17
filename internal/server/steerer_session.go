package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
	"flow/internal/steering"

	"github.com/google/uuid"
)

// Compile-time guarantee that the server satisfies the steering→server boundary.
var _ steering.SteererSessionSink = (*Server)(nil)

// steererChatPermissionMode: detached, AFK — same rationale as slackChatPermissionMode.
// A detached session has no UI to answer native tool-approval prompts, so it runs
// in "bypass" and relies on the prime's surface-only autonomy boundary.
const steererChatPermissionMode = "bypass"

// steererDeliveryAction is the decision for one delivery: start a fresh session,
// wake the live one, or resume a slept one. Pure (steererDeliveryPlan) so it is
// table-testable without a terminal hub.
type steererDeliveryAction int

const (
	steererActStart steererDeliveryAction = iota
	steererActWake
	steererActResume
)

func steererDeliveryPlan(chatExists, running bool) steererDeliveryAction {
	switch {
	case !chatExists:
		return steererActStart
	case running:
		return steererActWake
	default:
		return steererActResume
	}
}

// steererSlotState is the per-channel slot lifecycle. forking is reserved for the
// Phase 5 Claude→Codex provider fork and is never entered in Phase 2.
// ponytail: the load-bearing part today is the slot's mutex (serialize same-key
// deliveries); the state enum is the seam P5's fork hangs off.
type steererSlotState int

const (
	steererSlotNone steererSlotState = iota
	steererSlotStarting
	steererSlotLive
	steererSlotForking
)

type steererSlot struct {
	mu    sync.Mutex
	state steererSlotState
}

func (s *Server) steererSlot(slug string) *steererSlot {
	s.steererSlotsMu.Lock()
	defer s.steererSlotsMu.Unlock()
	if s.steererSlots == nil {
		s.steererSlots = map[string]*steererSlot{}
	}
	sl := s.steererSlots[slug]
	if sl == nil {
		sl = &steererSlot{}
		s.steererSlots[slug] = sl
	}
	return sl
}

// steererChatSlug derives the deterministic, idempotent slug for a channel's
// steerer session: "chat-steer-<sanitized-key>". Same key → same slug → one chat
// reused across events. The slug keys on the immutable channel id; a Slack rename
// never orphans the session (only the title would track a rename — GAP-13, P5).
func steererChatSlug(key string) string {
	return "chat-steer-" + sanitizeSlugSegment(key)
}

// steererSessionProvider is the provider a NEW steerer chat launches with: the
// configured default FLOW_STEERER_DEFAULT_PROVIDER (GAP-11), claude when unset or
// invalid. It applies only at chat CREATION; once a chat exists, chats.provider on
// the row is authoritative for resume (the manual switch / auto-fork flips it).
// ponytail: chats.provider is the per-key override; no separate per-key store.
func steererSessionProvider() string {
	if p, err := flowdb.NormalizeSessionProvider(os.Getenv("FLOW_STEERER_DEFAULT_PROVIDER")); err == nil {
		return p
	}
	return "claude"
}

// steererIdleTTL bounds how long a steerer PTY stays warm with no transcript
// activity before the idle sweep tears it down (the chat row + session_id survive;
// the next event resumes it). No cap on session COUNT — only on live PTYs.
func steererIdleTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERER_IDLE_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Minute
}

// steererShouldSleep reports whether a session whose transcript last changed at
// mtime should be put to sleep at now. A zero mtime (unknown) keeps it alive — we
// never tear down on missing data. Using transcript mtime (not delivery time)
// guarantees we never kill a session mid-turn while the agent is still working.
func steererShouldSleep(now, mtime time.Time, ttl time.Duration) bool {
	if mtime.IsZero() {
		return false
	}
	return now.Sub(mtime) >= ttl
}

// renderSteererTurn formats one delivery into the text fed to the session. The
// header mirrors what the prime tells the session to expect; the body is the
// cleaned message; the context pack rides along as JSON for the agent to parse.
func renderSteererTurn(p steering.SteererDelivery) string {
	var b strings.Builder
	kind := "channel message"
	switch {
	case p.SelfEcho:
		kind = "your own sent reply echoed back — context_only delivery confirmation; do NOT re-surface"
	case p.ContextOnly:
		kind = "context_only update — absorb into memory; do NOT surface or reply"
	}
	fmt.Fprintf(&b, "## Steerer turn — %s\n", kind)
	fmt.Fprintf(&b, "source=%s channel=%s channel_type=%s ts=%s thread_ts=%s author=%s\n\n",
		p.Source, p.Channel, p.ChannelType, p.TS, p.ThreadTS, p.Author)
	if t := strings.TrimSpace(p.Text); t != "" {
		b.WriteString("Message:\n```\n")
		b.WriteString(t)
		b.WriteString("\n```\n\n")
	}
	if js, err := json.Marshal(p.Context); err == nil && len(js) > len("{}") {
		b.WriteString("Context pack (JSON):\n```json\n")
		b.Write(js)
		b.WriteString("\n```\n")
	}
	return b.String()
}

// DeliverToChannelSession implements steering.SteererSessionSink (GAP-1). It
// ensures the channel's chat-steer-<key> session is live and feeds it one turn:
// running → wakeTask; PTY gone → resume from session_id → wakeTask; no row →
// start a fresh detached session (no --model, origin="steerer") primed with the
// steerer brief + this first turn. Per-slug serialized so concurrent events on the
// same channel never double-launch. Returns an error on any failure so the cascade
// can fall back to the stateless cold path (fail-open invariant).
func (s *Server) DeliverToChannelSession(key string, p steering.SteererDelivery) error {
	if s == nil || s.cfg.DB == nil {
		return errors.New("steerer session: no database")
	}
	if s.terminals == nil {
		return errors.New("steerer session: no terminal hub")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("steerer session: empty key")
	}
	slug := steererChatSlug(key)
	if err := validateSlug(slug); err != nil {
		return fmt.Errorf("steerer session: %w", err)
	}

	slot := s.steererSlot(slug)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	turn := renderSteererTurn(p)
	chat, err := flowdb.GetChat(s.cfg.DB, slug)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("steerer session: lookup chat %q: %w", slug, err)
	}

	// Muted chat: the operator silenced this conversation — consume the event
	// without forwarding it to the session, and do NOT fall back to cold triage
	// (returning nil marks it delivered). Self-echo/context_only included: a muted
	// chat receives nothing until unmuted.
	if exists && chat != nil && chat.MutedAt.Valid {
		return nil
	}

	switch steererDeliveryPlan(exists, exists && s.terminals.running(slug)) {
	case steererActWake:
		slot.state = steererSlotLive
		if err := s.terminals.wakeTask(slug, turn); err != nil {
			return fmt.Errorf("steerer session: deliver to live session %q: %w", slug, err)
		}
		s.maybeUpgradeSteererTitle(chat, p, key)
		return flowdb.TouchChat(s.cfg.DB, slug, flowdb.NowISO())
	case steererActResume:
		s.maybeUpgradeSteererTitle(chat, p, key)
		return s.resumeSteererChat(slot, chat, turn)
	default: // steererActStart
		title := s.resolveSteererChatTitle(context.Background(), p, key)
		return s.startNewSteererChat(slot, slug, turn, title)
	}
}

// steererSendReplyPrompt builds the one-shot, operator-authorized SEND instruction
// handed to an existing per-channel chat so IT posts an approved reply — it already
// holds the thread's memory and the Slack MCP, so it posts in the right context
// instead of a context-blind ephemeral session. It explicitly overrides the chat's
// surface-only default for this one approved turn and tells it to mark the card sent
// on a confirmed post (mirrors the ephemeral send session's doneCmd).
func steererSendReplyPrompt(item flowdb.FeedItem, channel, threadTS, text, instructions string) string {
	var b strings.Builder
	b.WriteString("[operator-approved reply — SEND IT NOW]\n")
	b.WriteString("The operator reviewed and APPROVED a reply for the thread you watch. This overrides your usual surface-only stance for THIS message only — you are authorized to post it.\n\n")
	fmt.Fprintf(&b, "Post it in-thread by CALLING the Slack MCP tool mcp__claude_ai_Slack__slack_send_message DIRECTLY with channel=%s, thread_ts=%s, and the approved text below (a real tool call — never Bash/echo/print). ", channel, threadTS)
	if strings.TrimSpace(instructions) != "" {
		fmt.Fprintf(&b, "Revise the text per these instructions before posting: %s. ", strings.TrimSpace(instructions))
	} else {
		b.WriteString("Post it as-is. ")
	}
	b.WriteString("Never sign it or add any attribution footer.\n")
	fmt.Fprintf(&b, "On a CONFIRMED post ONLY, run the shell command: flow attention sent %s\n\n", item.ID)
	b.WriteString("Approved text:\n")
	b.WriteString(text)
	return b.String()
}

// postApprovedReplyViaChat routes an operator-approved send-reply through the
// channel's existing per-channel steerer chat instead of an ephemeral send session.
// Returns handled=false (caller falls back to the ephemeral floating session) when
// sessions are off, the source isn't Slack, or no chat exists for the channel yet.
// Per-slug serialized like DeliverToChannelSession so it never races a live turn.
func (s *Server) postApprovedReplyViaChat(item flowdb.FeedItem, text, instructions string) (bool, error) {
	if !steering.SteererSessionsEnabled() || s == nil || s.cfg.DB == nil || s.terminals == nil {
		return false, nil
	}
	if item.Source != "slack" {
		return false, nil // GitHub posts via the gh agent; only Slack posts via the chat
	}
	channel, threadTS := splitThreadKey(item.ThreadKey)
	if strings.TrimSpace(channel) == "" {
		return false, nil
	}
	slug := steererChatSlug(channel)
	chat, err := flowdb.GetChat(s.cfg.DB, slug)
	if err != nil || chat == nil {
		return false, nil // no live chat for this channel → fall back to ephemeral
	}
	slot := s.steererSlot(slug)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	prompt := steererSendReplyPrompt(item, channel, threadTS, text, instructions)
	if s.terminals.running(slug) {
		slot.state = steererSlotLive
		if err := s.terminals.wakeTask(slug, prompt); err != nil {
			return false, fmt.Errorf("steerer send-reply: wake %q: %w", slug, err)
		}
		return true, nil
	}
	if err := s.resumeSteererChat(slot, chat, prompt); err != nil {
		return false, fmt.Errorf("steerer send-reply: resume %q: %w", slug, err)
	}
	return true, nil
}

// steererCorrectionPrompt builds a context_only teach turn: the operator corrected
// the chat's read of this thread, so it must update its running understanding
// without replying or surfacing.
func steererCorrectionPrompt(text string) string {
	return "[operator correction — context_only, update your understanding]\n" +
		"The operator corrected your read of this conversation. Absorb this into your running memory so it adjusts your future decisions on this thread. Do NOT reply and do NOT surface a card for this turn — this exists solely to keep your memory correct.\n\n" +
		"Correction:\n" + text
}

// postCorrectionToChat teaches an operator correction to the channel's existing
// per-channel chat (context_only) instead of running a stateless re-triage — under
// the session model the chat owns this thread's understanding. Returns handled=false
// (caller keeps the cold correction-retriage path) when sessions are off, the source
// isn't Slack, the chat is muted, or no chat exists. Per-slug serialized.
func (s *Server) postCorrectionToChat(item flowdb.FeedItem, text string) (bool, error) {
	if !steering.SteererSessionsEnabled() || s == nil || s.cfg.DB == nil || s.terminals == nil {
		return false, nil
	}
	if item.Source != "slack" {
		return false, nil
	}
	channel, _ := splitThreadKey(item.ThreadKey)
	if strings.TrimSpace(channel) == "" {
		return false, nil
	}
	slug := steererChatSlug(channel)
	chat, err := flowdb.GetChat(s.cfg.DB, slug)
	if err != nil || chat == nil || chat.MutedAt.Valid {
		return false, nil // no chat (or muted) → fall back to stateless correction-retriage
	}
	slot := s.steererSlot(slug)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	prompt := steererCorrectionPrompt(text)
	if s.terminals.running(slug) {
		slot.state = steererSlotLive
		if err := s.terminals.wakeTask(slug, prompt); err != nil {
			return false, fmt.Errorf("steerer correction: wake %q: %w", slug, err)
		}
		return true, nil
	}
	if err := s.resumeSteererChat(slot, chat, prompt); err != nil {
		return false, fmt.Errorf("steerer correction: resume %q: %w", slug, err)
	}
	return true, nil
}

// startNewSteererChat launches a fresh detached steerer session primed with the
// steerer brief + the first turn (mirrors openNewSlackChat), records its durable
// chats row with origin="steerer", and launches with NO --model so it runs the
// operator's default (Opus).
func (s *Server) startNewSteererChat(slot *steererSlot, slug, turn, title string) error {
	absRoot, err := s.absFlowRoot()
	if err != nil {
		return fmt.Errorf("steerer session: %w", err)
	}
	provider := steererSessionProvider()
	permissionMode, _ := flowdb.NormalizePermissionMode(steererChatPermissionMode)
	sessionID := uuid.NewString()
	prompt := steererSessionBrief() + "\n\n---\n\n" + turn
	args := agentTerminalArgs(provider, true /*fresh*/, sessionID, absRoot, absRoot, prompt, permissionMode, "" /*no --model*/)
	launch := terminalLaunch{
		Slug:           slug,
		SessionID:      sessionID,
		Provider:       provider,
		PermissionMode: permissionMode,
		WorkDir:        absRoot,
		Args:           args,
		FreeAgent:      true,
		Created:        true,
		NeedsCapture:   provider == "codex",
		StartedAt:      time.Now().Add(-2 * time.Second),
	}
	slot.state = steererSlotStarting
	ft := s.terminals.registerFloatingLaunch(launch, title)
	if err := s.terminals.startFloatingDetached(ft.ID); err != nil {
		s.terminals.stopFloating(ft.ID)
		slot.state = steererSlotNone
		return fmt.Errorf("steerer session: start %q: %w", slug, err)
	}
	now := flowdb.NowISO()
	if err := flowdb.UpsertChat(s.cfg.DB, flowdb.Chat{
		Slug:           slug,
		Title:          title,
		Provider:       provider,
		Origin:         "steerer",
		SessionID:      sql.NullString{String: sessionID, Valid: true},
		CreatedAt:      now,
		LastActivityAt: now,
	}); err != nil {
		s.terminals.stopFloating(ft.ID)
		slot.state = steererSlotNone
		return fmt.Errorf("steerer session: record chat %q: %w", slug, err)
	}
	slot.state = steererSlotLive
	return nil
}

// resumeSteererChat rebuilds a RESUME launch from the durable row, starts it
// detached, then delivers the turn (mirrors resumeSlackChat).
func (s *Server) resumeSteererChat(slot *steererSlot, chat *flowdb.Chat, turn string) error {
	slug := chat.Slug
	sessionID := strings.TrimSpace(chat.SessionID.String)
	if !chat.SessionID.Valid || sessionID == "" {
		return fmt.Errorf("steerer session: chat %q has no session to resume", slug)
	}
	provider, err := flowdb.NormalizeSessionProvider(chat.Provider)
	if err != nil {
		return fmt.Errorf("steerer session: %w", err)
	}
	absRoot, err := s.absFlowRoot()
	if err != nil {
		return fmt.Errorf("steerer session: %w", err)
	}
	permissionMode, _ := flowdb.NormalizePermissionMode(steererChatPermissionMode)
	args := agentTerminalArgs(provider, false /*resume*/, sessionID, absRoot, absRoot, "", permissionMode, "")
	launch := terminalLaunch{
		Slug: slug, SessionID: sessionID, Provider: provider, PermissionMode: permissionMode,
		WorkDir: absRoot, Args: args, FreeAgent: true, Created: true, NeedsCapture: provider == "codex",
	}
	slot.state = steererSlotStarting
	ft := s.terminals.registerFloatingLaunch(launch, chat.Title)
	if err := s.terminals.startFloatingDetached(ft.ID); err != nil {
		slot.state = steererSlotNone
		return fmt.Errorf("steerer session: resume %q: %w", slug, err)
	}
	if err := s.terminals.wakeTask(slug, turn); err != nil {
		return fmt.Errorf("steerer session: deliver to resumed %q: %w", slug, err)
	}
	slot.state = steererSlotLive
	return flowdb.TouchChat(s.cfg.DB, slug, flowdb.NowISO())
}

// steererChatTitleFallback is the placeholder title when names can't be resolved
// (Slack API unavailable at creation). The sticky-upgrade path replaces it with a
// human title once resolution succeeds; a custom/operator-renamed title is never
// equal to this, so it is never clobbered.
func steererChatTitleFallback(key string) string { return "Steering: " + key }

// steererTitleFor formats the human display title from already-resolved names
// (GAP-13, pure/testable). channelName is the resolver's "#channel" output (empty
// for DMs/MPDMs); authorName is the message author's display name. Returns "" when
// it can't form a good title, so the caller falls back to the placeholder.
// ponytail: external-org "(Org)" suffix for Slack-Connect partners is deferred —
// it needs Slack team_id extraction + an operator-team source not wired yet.
func steererTitleFor(p steering.SteererDelivery, channelName, authorName string) string {
	switch {
	case p.Source == "github" || p.ChannelType == "github":
		return githubChatTitle(p.Channel, p.ThreadTS)
	case p.ChannelType == "channel":
		return channelName // "" when unresolved → placeholder fallback
	case p.ChannelType == "im":
		if authorName != "" {
			return "DM · " + authorName
		}
	case p.ChannelType == "mpim":
		if authorName != "" {
			return "Group · " + authorName
		}
	}
	return ""
}

// githubChatTitle builds "owner/repo#N" from the repo + the link tag
// ("gh-pr:owner/repo#N"). repo alone when the number can't be parsed.
func githubChatTitle(repo, linkTag string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	if i := strings.LastIndex(linkTag, "#"); i >= 0 {
		if n := strings.TrimSpace(linkTag[i+1:]); n != "" {
			return repo + "#" + n
		}
	}
	return repo
}

// resolveSteererChatTitle resolves the best human title for a steerer chat at
// creation (GAP-13): Slack #channel / DM · Name / Group · Name via the name
// resolver, GitHub owner/repo#N from the delivery, placeholder otherwise.
func (s *Server) resolveSteererChatTitle(ctx context.Context, p steering.SteererDelivery, key string) string {
	var channelName, authorName string
	if s.nameResolver != nil {
		switch p.ChannelType {
		case "channel":
			channelName = s.nameResolver.ChannelName(ctx, p.Channel)
		case "im", "mpim":
			authorName = s.nameResolver.UserName(ctx, p.Author)
		}
	}
	if authorName == "" {
		authorName = strings.TrimSpace(p.Author)
	}
	if t := steererTitleFor(p, channelName, authorName); t != "" {
		return t
	}
	return steererChatTitleFallback(key)
}

// maybeUpgradeSteererTitle upgrades a chat still showing the placeholder
// "Steering: <key>" to a resolved human title once names become resolvable. Never
// touches a custom (operator-renamed) or already-resolved title. Best-effort.
func (s *Server) maybeUpgradeSteererTitle(chat *flowdb.Chat, p steering.SteererDelivery, key string) {
	if chat == nil || s.cfg.DB == nil || chat.Title != steererChatTitleFallback(key) {
		return
	}
	title := s.resolveSteererChatTitle(context.Background(), p, key)
	if title == "" || title == chat.Title {
		return
	}
	if err := flowdb.SetChatTitle(s.cfg.DB, chat.Slug, title, flowdb.NowISO()); err == nil {
		s.publishUIChange("chats")
	}
}

// switchSteererProvider switches a live steerer chat to a new provider
// (claude↔codex), shared by the manual settings/Chats switch (GAP-11) and the auto
// provider-fork on token exhaustion (GAP-9) — one mechanism, two triggers, either
// direction. It hands the new session a rendered transcript of the old one so it
// continues with context. Serialized on the slot, so an in-flight delivery (which
// also takes slot.mu) can't race the swap — it waits, then feeds the new session.
// MUST be called WITHOUT holding slot.mu (it acquires it); callers are the manual
// action handler and the occupancy worker, never DeliverToChannelSession.
func (s *Server) switchSteererProvider(slug, target string) error {
	if s == nil || s.cfg.DB == nil || s.terminals == nil {
		return errors.New("steerer switch: server not ready")
	}
	target, err := flowdb.NormalizeSessionProvider(target)
	if err != nil {
		return fmt.Errorf("steerer switch: %w", err)
	}
	slot := s.steererSlot(slug)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	chat, err := flowdb.GetChat(s.cfg.DB, slug)
	if err != nil {
		return fmt.Errorf("steerer switch: lookup chat %q: %w", slug, err)
	}
	if chat.Origin != "steerer" {
		return fmt.Errorf("steerer switch: chat %q is not a steerer chat", slug)
	}
	if current, _ := flowdb.NormalizeSessionProvider(chat.Provider); current == target {
		return nil // already on target — idempotent
	}

	slot.state = steererSlotForking
	// Render the dying session BEFORE teardown so the new provider gets context.
	// Deterministic (no model call) — the old session may be the one out of budget.
	handoff := s.steererForkHandoffPrime(chat)
	s.terminals.stopFloating(slug)

	absRoot, err := s.absFlowRoot()
	if err != nil {
		slot.state = steererSlotNone
		return fmt.Errorf("steerer switch: %w", err)
	}
	permissionMode, _ := flowdb.NormalizePermissionMode(steererChatPermissionMode)
	// Codex assigns its own id on launch (NeedsCapture fills it); Claude pre-generates.
	sessionID := ""
	if target == "claude" {
		sessionID = uuid.NewString()
	}
	prompt := steererSessionBrief() + handoff
	args := agentTerminalArgs(target, true /*fresh*/, sessionID, absRoot, absRoot, prompt, permissionMode, "")
	launch := terminalLaunch{
		Slug: slug, SessionID: sessionID, Provider: target, PermissionMode: permissionMode,
		WorkDir: absRoot, Args: args, FreeAgent: true, Created: true,
		NeedsCapture: target == "codex", StartedAt: time.Now().Add(-2 * time.Second),
	}
	ft := s.terminals.registerFloatingLaunch(launch, chat.Title)
	if err := s.terminals.startFloatingDetached(ft.ID); err != nil {
		s.terminals.stopFloating(ft.ID)
		slot.state = steererSlotNone
		return fmt.Errorf("steerer switch: relaunch %q: %w", slug, err)
	}
	now := flowdb.NowISO()
	if err := flowdb.SetChatProvider(s.cfg.DB, slug, target, now); err != nil {
		slot.state = steererSlotNone
		return fmt.Errorf("steerer switch: %w", err)
	}
	// Point the row at the new session id (claude: the new uuid; codex: cleared,
	// capture fills it post-launch).
	if err := flowdb.SetChatSession(s.cfg.DB, slug, sessionID, now); err != nil {
		slot.state = steererSlotNone
		return fmt.Errorf("steerer switch: %w", err)
	}
	slot.state = steererSlotLive
	s.publishUIChange("chats")
	return nil
}

// steererForkEnabled gates the Claude→Codex provider fork (GAP-9). Off by default;
// the manual switch (chat-set-provider) works regardless of this flag.
func steererForkEnabled() bool { return envBoolDefaultServer("FLOW_STEERER_FORK_PROVIDER", false) }

// steererForkRecoveryEnabled gates the optional time-based Codex→Claude come-back.
// Off by default — coming back to Claude is otherwise an operator (manual) action.
func steererForkRecoveryEnabled() bool { return envBoolDefaultServer("FLOW_STEERER_FORK_RECOVERY", false) }

// steererForkRecoveryAfter is how long a chat stays on Codex after an auto-fork
// before recovery retries Claude. A re-exhaustion re-forks and resets the timer,
// so repeated failures naturally push the next attempt out (anti-thrash, no
// explicit backoff needed).
func steererForkRecoveryAfter() time.Duration {
	return envDurationDefault("FLOW_STEERER_FORK_RECOVERY_AFTER", 2*time.Hour)
}

// forkTriggerMatches reports whether transcript text shows Claude usage/quota
// exhaustion the fork should escalate on (GAP-9, best-effort marker scan — the
// dependable trigger is the manual switch). Transient blips (overloaded / 5xx /
// timeout) are NOT exhaustion: they retry, they don't justify a provider fork.
func forkTriggerMatches(text string) bool {
	t := strings.ToLower(text)
	for _, m := range []string{
		"usage limit", "rate limit", "rate_limit", "quota",
		"insufficient_quota", "exhausted", "credit balance", "billing",
	} {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// recentSteererExhaustion scans a bounded transcript tail for an exhaustion marker.
func recentSteererExhaustion(entries []TranscriptEntry) bool {
	const tail = 8
	start := max(len(entries)-tail, 0)
	for _, e := range entries[start:] {
		if forkTriggerMatches(e.Text) || forkTriggerMatches(e.ToolResultText) {
			return true
		}
	}
	return false
}

// shouldRecoverToClaude reports whether an auto-forked Codex steerer chat should
// retry Claude. forkedAt is when this chat was auto-forked; a re-exhaustion resets
// it, so failures back off on their own.
func shouldRecoverToClaude(now, forkedAt time.Time, recoveryAfter time.Duration, flagOn bool) bool {
	if !flagOn || forkedAt.IsZero() || recoveryAfter <= 0 {
		return false
	}
	return now.Sub(forkedAt) >= recoveryAfter
}

// steererForkHandoffPrime renders the chat's current session transcript as priming
// for the new provider (GAP-9 hand-off). Reuses the existing fork renderer; empty
// when there's nothing to hand off.
func (s *Server) steererForkHandoffPrime(chat *flowdb.Chat) string {
	absRoot, err := s.absFlowRoot()
	if err != nil {
		return ""
	}
	provider, perr := flowdb.NormalizeSessionProvider(chat.Provider)
	if perr != nil {
		return ""
	}
	synth := &flowdb.Task{
		Slug: chat.Slug, WorkDir: absRoot, SessionProvider: provider,
		SessionID: chat.SessionID,
	}
	render, ok, rerr := s.renderForkTranscript(synth)
	if rerr != nil || !ok || strings.TrimSpace(render) == "" {
		return ""
	}
	return "\n\n---\n\n## Provider hand-off — prior session transcript\n" +
		"You are continuing a steerer session that switched providers. Reconstruct the channel's state from this log and keep going:\n\n" + render
}

// sweepIdleSteererSessionsOnce tears down the PTY of any live steerer session whose
// transcript has been quiet past the idle TTL, keeping the chat row + session_id so
// the next event resumes it. Reuses the kbDistiller transcript-mtime mechanism so
// "idle" means transcript-quiet (never mid-turn). No-op when sessions are disabled.
func (s *Server) sweepIdleSteererSessionsOnce(now time.Time) {
	if !steering.SteererSessionsEnabled() || s == nil || s.cfg.DB == nil || s.terminals == nil {
		return
	}
	chats, err := flowdb.ListChats(s.cfg.DB, flowdb.ChatFilter{})
	if err != nil {
		return
	}
	absRoot, aerr := s.absFlowRoot()
	if aerr != nil {
		return
	}
	ttl := steererIdleTTL()
	for _, ch := range chats {
		if ch == nil || ch.Origin != "steerer" || !s.terminals.running(ch.Slug) {
			continue
		}
		if !ch.SessionID.Valid || strings.TrimSpace(ch.SessionID.String) == "" {
			continue
		}
		provider, perr := flowdb.NormalizeSessionProvider(ch.Provider)
		if perr != nil {
			continue
		}
		path, perr := resolveSessionJSONLPath(&flowdb.Task{
			Slug: ch.Slug, WorkDir: absRoot, SessionProvider: provider,
			SessionID: ch.SessionID,
		})
		if perr != nil || path == "" {
			continue
		}
		entry, terr := s.transcripts.get(path)
		if terr != nil {
			continue
		}
		if steererShouldSleep(now, entry.mtime, ttl) {
			s.terminals.stopFloating(ch.Slug)
			sl := s.steererSlot(ch.Slug)
			sl.mu.Lock()
			sl.state = steererSlotNone
			sl.mu.Unlock()
		}
	}
}

// reconcileDeletedSteererSessions tears down the PTY of any steerer slug whose
// chat row is gone — soft-deleted (GAP-14). chat-delete already stops the PTY
// synchronously; this is the safety-net for any other path that sets deleted_at
// (so a deleted chat never leaves a claude/codex process running). Reset-and-reopen
// itself is structural: GetChat treats a deleted row as absent, so the next event
// on that key starts a FRESH session and UpsertChat reclaims the tombstone.
func (s *Server) reconcileDeletedSteererSessions() {
	if !steering.SteererSessionsEnabled() || s == nil || s.cfg.DB == nil || s.terminals == nil {
		return
	}
	s.steererSlotsMu.Lock()
	slugs := make([]string, 0, len(s.steererSlots))
	for slug := range s.steererSlots {
		slugs = append(slugs, slug)
	}
	s.steererSlotsMu.Unlock()
	for _, slug := range slugs {
		if !s.terminals.running(slug) {
			continue
		}
		if _, err := flowdb.GetChat(s.cfg.DB, slug); errors.Is(err, sql.ErrNoRows) {
			s.terminals.stopFloating(slug)
			sl := s.steererSlot(slug)
			sl.mu.Lock()
			sl.state = steererSlotNone
			sl.mu.Unlock()
		}
	}
}

// runSteererIdleSweep runs the idle sweep + deleted-chat reconciler on a ticker
// until ctx is done. Started from serve wiring only when sessions are enabled.
func (s *Server) runSteererIdleSweep(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepIdleSteererSessionsOnce(time.Now())
			s.reconcileDeletedSteererSessions()
		}
	}
}
