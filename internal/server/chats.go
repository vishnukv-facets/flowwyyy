package server

import (
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"

	"flow/internal/flowdb"
)

// chatTitleMaxRunes bounds a derived chat title so the sidebar list stays tidy.
const chatTitleMaxRunes = 60

// deriveChatTitle turns a launch prompt into a short, single-line title for the
// Chats list. It takes the first non-empty line, collapses internal whitespace,
// and truncates to chatTitleMaxRunes (appending an ellipsis when it had to cut).
// An empty/whitespace-only prompt yields "New chat".
func deriveChatTitle(prompt string) string {
	var line string
	for _, raw := range strings.Split(prompt, "\n") {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			line = trimmed
			break
		}
	}
	if line == "" {
		return "New chat"
	}
	// Collapse any run of whitespace (tabs, multiple spaces) to a single space.
	line = strings.Join(strings.FieldsFunc(line, unicode.IsSpace), " ")
	if line == "" {
		return "New chat"
	}
	runes := []rune(line)
	if len(runes) > chatTitleMaxRunes {
		return strings.TrimSpace(string(runes[:chatTitleMaxRunes])) + "…"
	}
	return line
}

// chatView is the JSON shape the UI consumes for a single chat in the Chats
// list. It carries the durable chat fields plus a Live flag computed from the
// floating-terminal registry (true when the chat's adhoc session still has a
// PTY attached).
type chatView struct {
	Slug           string `json:"slug"`
	Title          string `json:"title"`
	Provider       string `json:"provider"`
	Origin         string `json:"origin"`
	CreatedAt      string `json:"created_at"`
	LastActivityAt string `json:"last_activity_at"`
	Archived       bool   `json:"archived"`
	Live           bool   `json:"live"`
	// LastReply is a one-line preview of the agent's most recent response in
	// this chat (last assistant text from the session transcript), so the list
	// shows what the agent is saying / working on without opening the session.
	LastReply string `json:"last_reply,omitempty"`
}

// listChats returns the chats for the Chats sidebar, newest activity first.
// Archived chats are excluded unless includeArchived is true; deleted chats are
// always hidden (handled by flowdb.ListChats). The returned slice is never nil
// so JSON encodes an empty list as [] rather than null.
func (s *Server) listChats(includeArchived bool) ([]chatView, error) {
	out := []chatView{}
	if s.cfg.DB == nil {
		return out, nil
	}
	chats, err := flowdb.ListChats(s.cfg.DB, flowdb.ChatFilter{IncludeArchived: includeArchived})
	if err != nil {
		return nil, err
	}
	for _, c := range chats {
		out = append(out, chatView{
			Slug:           c.Slug,
			Title:          c.Title,
			Provider:       c.Provider,
			Origin:         c.Origin,
			CreatedAt:      c.CreatedAt,
			LastActivityAt: c.LastActivityAt,
			Archived:       c.ArchivedAt.Valid,
			Live:           s.terminals != nil && s.terminals.running(c.Slug),
			LastReply:      s.chatLastReply(c),
		})
	}
	return out, nil
}

// chatLastReply returns a one-line preview of the agent's most recent response
// in a chat's session — the last "assistant" text in its transcript, collapsed
// to a single line and truncated. Empty when the chat has no session yet, the
// transcript can't be resolved/read, or the agent hasn't spoken yet. Best
// effort: any error yields "" (the row simply shows no preview). The transcript
// parse is memoized by transcriptCache, so repeated list calls are cheap.
func (s *Server) chatLastReply(c *flowdb.Chat) string {
	if c == nil || !c.SessionID.Valid || strings.TrimSpace(c.SessionID.String) == "" {
		return ""
	}
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return ""
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	provider, err := flowdb.NormalizeSessionProvider(c.Provider)
	if err != nil {
		return ""
	}
	// Synthetic task carrying just what resolveSessionJSONLPath needs; the slug
	// is not in the tasks table, so we resolve the path directly (no DB
	// self-heal) rather than via sessionJSONLPath.
	task := &flowdb.Task{
		Slug:            c.Slug,
		WorkDir:         absRoot,
		SessionProvider: provider,
		SessionID:       sql.NullString{String: strings.TrimSpace(c.SessionID.String), Valid: true},
	}
	path, err := resolveSessionJSONLPath(task)
	if err != nil || path == "" {
		return ""
	}
	entry, err := s.transcripts.get(path)
	if err != nil {
		return ""
	}
	for i := len(entry.entries) - 1; i >= 0; i-- {
		if entry.entries[i].Type != "assistant" {
			continue
		}
		if line := firstLine(entry.entries[i].Text); line != "" {
			return truncateText(line, 120)
		}
	}
	return ""
}

// chatStatAgents builds minimal uiAgent records for chats so their token/cost
// burn folds into the Mission Control "flow-managed sessions" panel alongside
// tasks. Chats are flow-launched sessions (Ask Flow / Slack DMs) that the panel
// claims to cover, but they live in the chats table — not as TaskViews — so they
// were silently excluded and the totals undercounted. Archived-but-not-deleted
// chats are included (they still burned tokens). Only the fields buildUIStats'
// tally reads are populated (Slug for dedup, Provider for the split, and the
// per-session token/cost); chats with no resolvable usage yet are skipped so the
// session count isn't inflated by empty sessions.
func (s *Server) chatStatAgents() []uiAgent {
	if s.cfg.DB == nil {
		return nil
	}
	chats, err := flowdb.ListChats(s.cfg.DB, flowdb.ChatFilter{IncludeArchived: true})
	if err != nil {
		return nil
	}
	absRoot, aerr := filepath.Abs(strings.TrimSpace(s.cfg.FlowRoot))
	if aerr != nil {
		return nil
	}
	var out []uiAgent
	for _, c := range chats {
		if c == nil || !c.SessionID.Valid || strings.TrimSpace(c.SessionID.String) == "" {
			continue
		}
		provider, perr := flowdb.NormalizeSessionProvider(c.Provider)
		if perr != nil {
			continue
		}
		task := &flowdb.Task{
			Slug:            c.Slug,
			WorkDir:         absRoot,
			SessionProvider: provider,
			SessionID:       sql.NullString{String: strings.TrimSpace(c.SessionID.String), Valid: true},
		}
		tokens, cost := s.chatSessionUsage(task)
		if tokens == 0 && cost == 0 {
			continue // nothing burned yet / transcript unresolved — don't inflate the count
		}
		out = append(out, uiAgent{Slug: c.Slug, Provider: provider, TokensSession: tokens, CostSession: cost})
	}
	return out
}

// chatSessionUsage resolves a chat's session transcript and returns its
// cumulative session tokens (cache-excluded, the CLI's Σ) and full billed cost
// (cache included) — the same two figures sessionInsightsForTask derives for
// tasks. The transcript parse is memoized by transcriptCache, so this stays cheap
// on the buildUIData hot path. Any resolution/read error yields (0, 0).
func (s *Server) chatSessionUsage(task *flowdb.Task) (int, float64) {
	path, err := resolveSessionJSONLPath(task)
	if err != nil || path == "" {
		return 0, 0
	}
	entry, err := s.transcripts.get(path)
	if err != nil {
		return 0, 0
	}
	cost := 0.0
	for _, c := range entry.usage.CostByDay {
		cost += c
	}
	return entry.usage.TokensSession, cost
}

// handleChats serves GET /api/chats — the Chats sidebar list. The
// include_archived query param (true/1/yes/on) surfaces archived chats too;
// deleted chats are always hidden. The body is always a JSON array (never
// null) so the UI can iterate it unconditionally.
func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	includeArchived := boolQuery(r.URL.Query(), "include_archived")
	result, err := s.listChats(includeArchived)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

// chatAction handles the chat management action kinds dispatched from
// runAction: chat-archive, chat-unarchive, chat-delete, chat-reopen. The chat
// slug arrives as Target (falling back to Slug). All paths publish a "chats" UI
// change on success so the sidebar refreshes.
func (s *Server) chatAction(req actionRequest) (actionResponse, int) {
	slug := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(slug); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	if s.cfg.DB == nil {
		return actionResponse{OK: false, Message: "no database"}, http.StatusInternalServerError
	}
	switch req.Kind {
	case "chat-archive":
		if err := flowdb.ArchiveChat(s.cfg.DB, slug, flowdb.NowISO()); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		s.publishUIChange("chats")
		return actionResponse{OK: true, Message: "archived chat"}, http.StatusOK
	case "chat-unarchive":
		if err := flowdb.UnarchiveChat(s.cfg.DB, slug); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		s.publishUIChange("chats")
		return actionResponse{OK: true, Message: "unarchived chat"}, http.StatusOK
	case "chat-delete":
		// Tear down any live floating session first so its tray chip and PTY
		// vanish, then soft-delete the row. stopFloating is idempotent, so a
		// chat with no live session deletes cleanly too.
		if s.terminals != nil {
			s.terminals.stopFloating(slug)
		}
		if err := flowdb.DeleteChat(s.cfg.DB, slug, flowdb.NowISO()); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		s.publishUIChange("chats")
		return actionResponse{OK: true, Message: "deleted chat"}, http.StatusOK
	case "chat-reopen":
		return s.reopenChat(slug)
	case "chat-set-provider":
		// Manual provider switch on a steerer chat (GAP-11) — same switch path as
		// the auto-fork (GAP-9), either direction (claude↔codex). Re-primes the new
		// session from a rendered transcript of the old one.
		if err := s.switchSteererProvider(slug, req.Provider); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		return actionResponse{OK: true, Message: "switched chat provider to " + req.Provider}, http.StatusOK
	default:
		return actionResponse{OK: false, Message: "unknown chat action " + req.Kind}, http.StatusBadRequest
	}
}

// reopenChat brings a chat's adhoc session back into a watchable floating
// terminal so the UI can reattach. When the session is still alive (or merely
// detached but still registered in the floating hub), it returns the existing
// floating-terminal handle. When the session has fully ended and is no longer
// registered, it rebuilds a RESUME launch from the durable chat row and
// registers a fresh floating session that resumes the original agent
// conversation by session id.
func (s *Server) reopenChat(slug string) (actionResponse, int) {
	chat, err := flowdb.GetChat(s.cfg.DB, slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "chat not found"}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if s.terminals == nil {
		return actionResponse{OK: false, Message: "no terminal hub"}, http.StatusInternalServerError
	}

	// Live or still-registered: hand back the existing handle so the UI
	// reattaches to the running (or replay-from-scrollback) session rather than
	// spawning a duplicate.
	if existing, ok := s.terminals.floatingResponse(slug); ok {
		return actionResponse{OK: true, Message: "reattached chat", FloatingTerminal: &existing}, http.StatusOK
	}

	// Dead and forgotten: rebuild a RESUME launch from the chat row. agentTerminalArgs
	// with fresh=false produces `--resume <sid>` (Claude) / `resume ... <sid>` (Codex),
	// reattaching the original conversation. Without a captured session id there is
	// nothing to resume.
	sessionID := strings.TrimSpace(chat.SessionID.String)
	if !chat.SessionID.Valid || sessionID == "" {
		return actionResponse{OK: false, Message: "chat has no session to resume"}, http.StatusConflict
	}
	provider, err := flowdb.NormalizeSessionProvider(chat.Provider)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return actionResponse{OK: false, Message: "flow root is not configured"}, http.StatusInternalServerError
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	// Adhoc overview/chat sessions launch in default permission mode (overviewChat
	// derives it from the request, defaulting to DefaultPermissionMode); resume
	// mirrors that. The chat row does not persist a permission mode, so use the
	// normalized default. fresh=false → RESUME args; empty prompt (resume carries none).
	permissionMode, _ := flowdb.NormalizePermissionMode("")
	args := agentTerminalArgs(provider, false, sessionID, absRoot, absRoot, "", permissionMode, "")
	launch := terminalLaunch{
		Slug:           chat.Slug,
		SessionID:      sessionID,
		Provider:       provider,
		PermissionMode: permissionMode,
		WorkDir:        absRoot,
		Args:           args,
		FreeAgent:      true,
		Created:        true,
		NeedsCapture:   provider == "codex",
	}
	terminal := s.terminals.registerFloatingLaunch(launch, chat.Title)
	if err := flowdb.TouchChat(s.cfg.DB, slug, flowdb.NowISO()); err != nil {
		// Best-effort: the session is registered; a touch hiccup must not fail reopen.
		return actionResponse{OK: true, Message: "reopened chat", FloatingTerminal: &terminal}, http.StatusOK
	}
	s.publishUIChange("chats")
	return actionResponse{OK: true, Message: "reopened chat", FloatingTerminal: &terminal}, http.StatusOK
}
