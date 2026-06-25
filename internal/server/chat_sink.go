package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flow/internal/flowdb"

	"github.com/google/uuid"
)

// OpenOrContinueChat implements monitor.ChatCommandSink. It routes an authorized
// operator's Slack DM (on the operator↔bot IM channel) into a durable "chat"
// agent session — the same overview-chat model the UI's Ask Flow uses — keyed
// deterministically by the IM channel so one chat is reused across messages.
//
//   - New chat: spawn a fresh free-agent floating session whose launch prompt is
//     the overview brief + a Slack-reply instruction block, then start it detached
//     so it runs whether or not the operator opens the UI window, and record a
//     durable chats row.
//   - Live chat: deliver the new command into the running PTY.
//   - Resumable chat (row exists, PTY gone): rebuild a RESUME launch from the
//     stored session id, start it detached, then deliver the command.
//
// The dispatcher only calls this for authorized operators on the command channel
// (CommandChannelEnabled + IsCommandChannel + AuthorizedOperator), so there is no
// additional authorization here.
func (s *Server) OpenOrContinueChat(ctx context.Context, channel, text string) error {
	_ = ctx
	if s == nil || s.cfg.DB == nil {
		return errors.New("chat sink: no database")
	}
	if s.terminals == nil {
		return errors.New("chat sink: no terminal hub")
	}
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return errors.New("chat sink: empty channel")
	}
	slug := slackChatSlug(channel)
	if err := validateSlug(slug); err != nil {
		return fmt.Errorf("chat sink: %w", err)
	}

	// No server-side ack: the acknowledgment ("on it") is the agent's own first
	// action, per slackReplyInstructions, so the operator hears from the agent
	// that's actually doing the work — not a canned server reply.
	chat, err := flowdb.GetChat(s.cfg.DB, slug)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s.openNewSlackChat(slug, channel, text)
	case err != nil:
		return fmt.Errorf("chat sink: lookup chat %q: %w", slug, err)
	}

	// Existing chat: live → deliver into the running PTY; otherwise resume.
	if s.terminals.running(slug) {
		if err := s.terminals.wakeTask(slug, text); err != nil {
			return fmt.Errorf("chat sink: deliver to live chat %q: %w", slug, err)
		}
		if err := flowdb.TouchChat(s.cfg.DB, slug, flowdb.NowISO()); err != nil {
			return fmt.Errorf("chat sink: touch chat %q: %w", slug, err)
		}
		return nil
	}
	return s.resumeSlackChat(chat, channel, text)
}

// openNewSlackChat builds and starts a fresh free-agent floating session for a
// Slack command-channel chat and records its durable row. The initial command
// text is baked into the launch prompt (overview brief + reply instructions), so
// no separate nudge is needed for the first message.
func (s *Server) openNewSlackChat(slug, channel, text string) error {
	absRoot, err := s.absFlowRoot()
	if err != nil {
		return fmt.Errorf("chat sink: %w", err)
	}
	provider := slackCommandProvider()
	permissionMode, _ := flowdb.NormalizePermissionMode(slackChatPermissionMode)
	sessionID := uuid.NewString()
	brief := overviewBrief(text) + slackReplyInstructions(channel)
	args := agentTerminalArgs(provider, true /*fresh*/, sessionID, absRoot, absRoot, brief, permissionMode, "", "")
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
	title := deriveChatTitle(text)
	ft := s.terminals.registerFloatingLaunch(launch, "Slack: "+title)
	// Start detached so the agent runs (and can reply over Slack) whether or not
	// the operator opens the floating window in the UI.
	if err := s.terminals.startFloatingDetached(ft.ID); err != nil {
		s.terminals.stopFloating(ft.ID)
		return fmt.Errorf("chat sink: start chat session %q: %w", slug, err)
	}
	now := flowdb.NowISO()
	// UpsertChat (not InsertChat): the slug is deterministic per IM channel, so a
	// previously deleted chat for this channel leaves a soft-deleted tombstone
	// under the same slug. Resurrect it into this fresh chat (new session id,
	// deleted_at cleared) instead of failing on the primary-key conflict.
	if err := flowdb.UpsertChat(s.cfg.DB, flowdb.Chat{
		Slug:           slug,
		Title:          title,
		Provider:       provider,
		Origin:         "slack",
		SessionID:      sql.NullString{String: sessionID, Valid: true},
		CreatedAt:      now,
		LastActivityAt: now,
	}); err != nil {
		// The session is live; a DB hiccup must not orphan it. Tear it down so a
		// retry re-opens cleanly rather than leaking a session with no row.
		s.terminals.stopFloating(ft.ID)
		return fmt.Errorf("chat sink: record chat %q: %w", slug, err)
	}
	return nil
}

// resumeSlackChat rebuilds a RESUME launch from a durable chat row, starts it
// detached, then delivers the new command into the resumed session. Mirrors the
// resume path in reopenChat (agentTerminalArgs fresh=false → `--resume <sid>`).
func (s *Server) resumeSlackChat(chat *flowdb.Chat, channel, text string) error {
	_ = channel
	slug := chat.Slug
	sessionID := strings.TrimSpace(chat.SessionID.String)
	if !chat.SessionID.Valid || sessionID == "" {
		return fmt.Errorf("chat sink: chat %q has no session to resume", slug)
	}
	provider, err := flowdb.NormalizeSessionProvider(chat.Provider)
	if err != nil {
		return fmt.Errorf("chat sink: %w", err)
	}
	absRoot, err := s.absFlowRoot()
	if err != nil {
		return fmt.Errorf("chat sink: %w", err)
	}
	permissionMode, _ := flowdb.NormalizePermissionMode(slackChatPermissionMode)
	// fresh=false → RESUME args; empty prompt (resume carries none — the command
	// is delivered as a separate nudge once the session is live).
	args := agentTerminalArgs(provider, false, sessionID, absRoot, absRoot, "", permissionMode, "", "")
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
	}
	ft := s.terminals.registerFloatingLaunch(launch, chat.Title)
	if err := s.terminals.startFloatingDetached(ft.ID); err != nil {
		return fmt.Errorf("chat sink: resume chat session %q: %w", slug, err)
	}
	if err := s.terminals.wakeTask(slug, text); err != nil {
		return fmt.Errorf("chat sink: deliver to resumed chat %q: %w", slug, err)
	}
	if err := flowdb.TouchChat(s.cfg.DB, slug, flowdb.NowISO()); err != nil {
		return fmt.Errorf("chat sink: touch chat %q: %w", slug, err)
	}
	return nil
}

// absFlowRoot resolves the configured flow root to an absolute path, mirroring
// the guard used by prepareOverviewFloatingLaunch / reopenChat.
func (s *Server) absFlowRoot() (string, error) {
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return "", errors.New("flow root is not configured")
	}
	return filepath.Abs(root)
}

// slackChatPermissionMode is the permission mode for Slack command-channel chat
// sessions. They run DETACHED for an AFK operator (no UI attached), so native
// tool-approval prompts have no one to answer and would hang the session
// forever. We therefore run in "bypass" (no native prompts), matching the
// detached send-reply floating session, and rely on the brief's
// "ask before risky actions" guidance for the human-in-the-loop: the agent
// itself decides what needs sign-off and asks the operator over the Slack DM.
const slackChatPermissionMode = "bypass"

// slackChatSlug derives a deterministic, idempotent slug for the chat bound to a
// Slack IM channel: "chat-slack-<sanitized-channel>". Same channel → same slug,
// so the chat is reused across messages. The sanitized form keeps only [a-z0-9-]
// (channel lowercased, everything else collapsed to '-'); since the slug is
// prefixed with the literal "chat-slack-" it always starts with a letter and
// matches validateSlug's grammar.
func slackChatSlug(channel string) string {
	return "chat-slack-" + sanitizeSlugSegment(channel)
}

// sanitizeSlugSegment lowercases s and replaces every rune outside [a-z0-9-]
// with '-', collapsing consecutive dashes and trimming leading/trailing ones.
func sanitizeSlugSegment(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// slackReplyInstructions is appended to the launch brief so the chat agent knows
// it reached the operator over a Slack DM and how to reply (the operator is AFK,
// not watching the UI). channel is interpolated so the agent posts back to the
// same IM.
func slackReplyInstructions(channel string) string {
	return "\n\n## You are driving this machine for an AFK operator\n" +
		"This request arrived as a Slack DM command. The operator is AFK, so **act on it directly** — " +
		"do the work yourself with your tools (Bash, the flow CLI, code edits). You have the same access " +
		"to this machine as the operator has at the keyboard; do whatever they ask, don't just advise.\n\n" +
		"## Acknowledge FIRST, before you start\n" +
		"The operator is AFK and there is no automatic receipt — they only know you got the message if you " +
		"tell them. So for EVERY request you receive, your very first action — before any investigation, tool " +
		"use, or work — is to send ONE short acknowledgment line over the Slack DM with the reply command " +
		"below (e.g. `On it — checking now.` or `Got it, looking into this.`). Keep it to a single line, then " +
		"go do the work and send your real answer when it's ready. Don't acknowledge more than once per request.\n\n" +
		"## Coordinating the operator's other flow work\n" +
		"You are not limited to this conversation — you can inspect and drive their other in-flight work:\n" +
		"  - `flow list tasks` / `flow show task <slug>` — what's running, plus a task's status, waiting note, and recent updates.\n" +
		"  - `flow transcript <slug> --compact` — read another session's recent progress to report it back.\n" +
		"  - `flow tell <slug> \"<message>\"` — send an instruction into a running session (wakes it).\n" +
		"  - `flow spawn <name> --prompt \"...\"` or `flow do <task> --auto` — start or run a task headlessly.\n" +
		"  - `flow list playbooks` / `flow run playbook <slug> --auto` — list and run playbooks headlessly.\n" +
		"  - `flow wait <slug> --until done` — block until a task finishes, then report the outcome.\n\n" +
		"## Ask before risky or irreversible actions\n" +
		"You judge when something needs the operator's sign-off. Before anything destructive, irreversible, " +
		"or clearly outside the scope of the request — deleting data, force-pushing, merging or deploying to " +
		"production, spending money, mass changes — **do NOT just do it**. Send the operator a concise question " +
		"over the Slack DM (the reply tool below) stating exactly what you intend to do, and WAIT for their " +
		"answer (their reply arrives as your next message) before proceeding. When in doubt, ask. For routine, " +
		"safe, reversible work, just proceed and report what you did.\n\n" +
		"## Replying\n" +
		"Reply to the operator on the same Slack DM by running:\n" +
		"  flow slack send --channel " + channel + " --text \"<your reply>\"\n" +
		"Replies are read on a phone — keep them concise; on error, say what failed. " +
		"Do not call flow done — this chat is long-lived and continues on the next message."
}

// slackCommandProvider resolves the agent provider for new Slack command-channel
// chats from FLOW_SLACK_COMMAND_PROVIDER (claude|codex), defaulting to claude.
// An unrecognized value falls back to claude rather than failing the DM.
func slackCommandProvider() string {
	provider, err := flowdb.NormalizeSessionProvider(os.Getenv("FLOW_SLACK_COMMAND_PROVIDER"))
	if err != nil {
		return "claude"
	}
	return provider
}
