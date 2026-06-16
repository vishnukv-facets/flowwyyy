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

	switch steererDeliveryPlan(exists, exists && s.terminals.running(slug)) {
	case steererActWake:
		slot.state = steererSlotLive
		if err := s.terminals.wakeTask(slug, turn); err != nil {
			return fmt.Errorf("steerer session: deliver to live session %q: %w", slug, err)
		}
		return flowdb.TouchChat(s.cfg.DB, slug, flowdb.NowISO())
	case steererActResume:
		return s.resumeSteererChat(slot, chat, turn)
	default: // steererActStart
		return s.startNewSteererChat(slot, slug, key, turn)
	}
}

// startNewSteererChat launches a fresh detached steerer session primed with the
// steerer brief + the first turn (mirrors openNewSlackChat), records its durable
// chats row with origin="steerer", and launches with NO --model so it runs the
// operator's default (Opus).
func (s *Server) startNewSteererChat(slot *steererSlot, slug, key, turn string) error {
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
	ft := s.terminals.registerFloatingLaunch(launch, steererChatTitle(key))
	if err := s.terminals.startFloatingDetached(ft.ID); err != nil {
		s.terminals.stopFloating(ft.ID)
		slot.state = steererSlotNone
		return fmt.Errorf("steerer session: start %q: %w", slug, err)
	}
	now := flowdb.NowISO()
	if err := flowdb.UpsertChat(s.cfg.DB, flowdb.Chat{
		Slug:           slug,
		Title:          steererChatTitle(key),
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

// steererChatTitle is the minimal display title for Phase 2 (the channel id). The
// human-readable naming convention (#channel / DM · Name / external-org tag) is
// GAP-13 → Phase 5.
func steererChatTitle(key string) string { return "Steering: " + key }

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

// runSteererIdleSweep runs sweepIdleSteererSessionsOnce on a ticker until ctx is
// done. Started from serve wiring only when sessions are enabled (Task 6).
func (s *Server) runSteererIdleSweep(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepIdleSteererSessionsOnce(time.Now())
		}
	}
}
