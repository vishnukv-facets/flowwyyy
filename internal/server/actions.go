package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flow/internal/flowdb"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type actionRequest struct {
	Kind           string `json:"kind"`
	Target         string `json:"target"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Path           string `json:"path"`
	Description    string `json:"description"`
	Project        string `json:"project"`
	WorkDir        string `json:"work_dir"`
	Priority       string `json:"priority"`
	Prompt         string `json:"prompt"`
	SessionID      string `json:"session_id"`
	Branch         string `json:"branch"`
	EntityKind     string `json:"entity_kind"`
	Provider       string `json:"provider"`
	PermissionMode string `json:"permission_mode"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	Mkdir          bool   `json:"mkdir"`

	// Schedule is the recurring-schedule phrase ("every 6 hours",
	// "Wednesday at 1pm", or cron) for the set-playbook-schedule action.
	Schedule string `json:"schedule,omitempty"`
	// ScheduleOp selects the schedule mutation: set | clear | pause | resume.
	// Empty with a non-empty Schedule is treated as "set".
	ScheduleOp string `json:"schedule_op,omitempty"`

	// NoOpen, on create-flow, creates the task in backlog without opening an
	// agent session (no browser-terminal bridge). Defaults false → the legacy
	// "create & open session" behaviour is preserved for callers that omit it.
	NoOpen bool `json:"no_open,omitempty"`

	// AttentionAction is the verb for the attention-act action kind:
	// make-task | forward | dismiss | send-reply. Target carries the feed item id.
	AttentionAction string `json:"attention_action,omitempty"`

	// MergeTarget is the kept attention feed item id for attention_action=merge-into.
	// Target carries the duplicate feed item id.
	MergeTarget string `json:"merge_target,omitempty"`

	// ReplyText is the operator's edited draft for the send-reply attention
	// action. Empty falls back to the feed item's stored Draft.
	ReplyText string `json:"reply_text,omitempty"`

	// ReplyInstructions is optional extra guidance for the sending agent on the
	// send-reply action ("make it shorter", "also ask about the timeline"). When
	// present, the agent revises the draft per these instructions before posting;
	// empty means post the draft as-is.
	ReplyInstructions string `json:"reply_instructions,omitempty"`

	// CorrectionText is the operator's authoritative context for the "correct"
	// attention action ("this thread is actually about X"). It is stored on the
	// thread's running understanding and re-triaged as ground truth.
	CorrectionText string `json:"correction_text,omitempty"`

	// Remember, on the "correct" action, also promotes the correction into the KB
	// as a durable cross-thread fact (default false ⇒ thread-local only).
	Remember bool `json:"remember,omitempty"`

	// Settings carries key→value pairs for the update-settings action.
	Settings map[string]string `json:"settings,omitempty"`

	AttachmentFiles []*multipart.FileHeader `json:"-"`
}

type actionResponse struct {
	OK               bool                      `json:"ok"`
	Message          string                    `json:"message"`
	Output           string                    `json:"output,omitempty"`
	Agent            *uiAgent                  `json:"agent,omitempty"`
	FloatingTerminal *floatingTerminalResponse `json:"floating_terminal,omitempty"`
	Bridge           bool                      `json:"bridge,omitempty"`
	AlreadyLive      bool                      `json:"already_live,omitempty"`
}

type floatingTerminalResponse struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Title    string `json:"title"`
}

var (
	safeSlugRe    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	safeSessionRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	safeBranchRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/\-]*$`)
)

const (
	overviewTaskSlug = "flow-overview"
	overviewTaskName = "Flow overview command center"
)

var nativeCommandStarter = startNativeCommand

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req actionRequest
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		var err error
		req, err = s.multipartActionRequest(w, r)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
	}
	resp, status := s.runAction(req)
	writeJSONStatus(w, resp, status)
	if status < 400 && resp.OK {
		s.publishUIChange(req.Kind)
	}
}

func (s *Server) multipartActionRequest(w http.ResponseWriter, r *http.Request) (actionRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTerminalAttachmentUploadBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		return actionRequest{}, err
	}
	req := actionRequest{
		Kind:           strings.TrimSpace(r.FormValue("kind")),
		Target:         strings.TrimSpace(r.FormValue("target")),
		Slug:           strings.TrimSpace(r.FormValue("slug")),
		Name:           strings.TrimSpace(r.FormValue("name")),
		Path:           strings.TrimSpace(r.FormValue("path")),
		Description:    strings.TrimSpace(r.FormValue("description")),
		Project:        strings.TrimSpace(r.FormValue("project")),
		WorkDir:        strings.TrimSpace(r.FormValue("work_dir")),
		Priority:       strings.TrimSpace(r.FormValue("priority")),
		Prompt:         r.FormValue("prompt"),
		SessionID:      strings.TrimSpace(r.FormValue("session_id")),
		Branch:         strings.TrimSpace(r.FormValue("branch")),
		EntityKind:     strings.TrimSpace(r.FormValue("entity_kind")),
		Provider:       strings.TrimSpace(r.FormValue("provider")),
		PermissionMode: strings.TrimSpace(r.FormValue("permission_mode")),
		Model:          strings.TrimSpace(r.FormValue("model")),
		Effort:         strings.TrimSpace(r.FormValue("effort")),
	}
	if req.Kind == "" {
		return actionRequest{}, errors.New("kind is required")
	}
	if raw := strings.TrimSpace(r.FormValue("mkdir")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return actionRequest{}, fmt.Errorf("mkdir: %w", err)
		}
		req.Mkdir = value
	}
	if raw := strings.TrimSpace(r.FormValue("no_open")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return actionRequest{}, fmt.Errorf("no_open: %w", err)
		}
		req.NoOpen = value
	}
	if r.MultipartForm != nil {
		req.AttachmentFiles = r.MultipartForm.File["images"]
		if len(req.AttachmentFiles) == 0 {
			req.AttachmentFiles = r.MultipartForm.File["files"]
		}
	}
	return req, nil
}

func (s *Server) runAction(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	switch req.Kind {
	case "spawn":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		return s.openBrowserTerminalBridge(target, req.Provider)
	case "resume", "attach":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		return s.openBrowserTerminalBridge(target, "")
	case "iterm", "terminal", "warp", "kitty", "alacritty", "ghostty", "wezterm", "tmux", "vscode":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		if err := s.ensureTerminalAvailable(req.Kind); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		return s.openTaskBridge(target, req.Kind, false)
	case "restart":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		return s.restartBrowserTerminalBridge(target)
	case "restart-fresh":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		return s.restartFreshBrowserTerminalBridge(target)
	case "switch-branch":
		return s.switchBranch(req)
	case "archive":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		out, err := s.runFlowCommand("archive", target)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "archived " + target, Output: out}, http.StatusOK
	case "unarchive":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		out, err := s.runFlowCommand("unarchive", target)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "unarchived " + target, Output: out}, http.StatusOK
	case "done":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		// `flow done` flips status, snapshots git, and runs the headless KB /
		// project-update close-out sweep (the slow part — within runFlowCommand's
		// 2-minute budget). Output is returned so the UI can confirm which phases
		// actually ran.
		out, err := s.runFlowCommand("done", target)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "marked " + target + " done", Output: out}, http.StatusOK
	case "delete", "restore":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		ref, err := qualifiedEntityRef(req.EntityKind, target)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		out, err := s.runFlowCommand(req.Kind, ref)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: req.Kind + "d " + target, Output: out}, http.StatusOK
	case "destroy":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		return s.destroyDeletedEntity(req.EntityKind, target)
	case "empty-trash":
		return s.emptyTrash()
	case "workdir-add", "workdir-rename", "workdir-remove":
		return s.workdirAction(req)
	case "spawn-run":
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		return s.spawnPlaybookRunBridge(target, req)
	case "create-flow":
		return s.createFlow(req)
	case "create-project":
		return s.createProject(req)
	case "create-playbook":
		return s.createPlaybook(req)
	case "create-kb":
		return s.createKB(req)
	case "update-permission-mode":
		return s.updatePermissionMode(req)
	case "update-priority":
		return s.updatePriority(req)
	case "update-provider":
		return s.updateProvider(req)
	case "update-model":
		return s.updateModel(req)
	case "update-effort":
		return s.updateEffort(req)
	case "update-task-name":
		return s.updateTaskName(req)
	case "update-project":
		return s.updateProject(req)
	case "update-playbook":
		return s.updatePlaybook(req)
	case "set-playbook-schedule":
		return s.updatePlaybookSchedule(req)
	case "pause":
		return s.pauseTask(target)
	case "clear-waiting":
		return s.clearWaiting(target)
	case "mark-read":
		return s.markInboxRead(target)
	case "kill":
		return s.killSession(req)
	case "approve", "deny":
		return s.ackApproval(req.Kind, target)
	case "fork":
		return s.forkTask(req)
	case "edit-playbook":
		return s.editPlaybook(target)
	case "nudge":
		return s.nudgeSession(target, req.Prompt)
	case "overview-chat":
		return s.overviewChat(req)
	case "close-floating-terminal":
		return s.closeFloatingTerminal(req)
	case "chat-archive", "chat-unarchive", "chat-delete", "chat-reopen", "chat-rename", "chat-set-provider", "chat-mute", "chat-unmute":
		return s.chatAction(req)
	case "update-settings":
		return s.updateSettings(req)
	case "rotate-webhook-secret":
		return s.rotateWebhookSecret()
	case "rotate-ingress-url":
		return s.rotateZrokShareName()
	case "reveal-webhook-secret":
		return s.revealWebhookSecret()
	case "compact-db":
		return s.compactFlowDB()
	case "recheck-provider-limits":
		return s.recheckProviderLimits()
	case "attention-act":
		return s.attentionAct(req)
	case "attention-autoact":
		return s.attentionAutoAct(req)
	default:
		return actionResponse{OK: false, Message: "unknown action " + req.Kind}, http.StatusBadRequest
	}
}

func (s *Server) compactFlowDB() (actionResponse, int) {
	// uiFlowDBFresh bypasses the hot-path cache: the precheck below trusts
	// before.ReclaimableBytes and before.PageSize, which a stale cache could
	// under-report.
	before := s.uiFlowDBFresh()
	if !before.Exists {
		return actionResponse{OK: false, Message: "flow database is missing"}, http.StatusNotFound
	}
	if before.Error != "" || before.PageSize == 0 {
		return actionResponse{OK: false, Message: "database is busy; try compacting again when flow is idle"}, http.StatusConflict
	}
	quickCheck := sqliteQuickCheck(s.cfg.DB, 2*time.Minute)
	if quickCheck != "ok" {
		return actionResponse{OK: false, Message: "database integrity check failed: " + quickCheck}, http.StatusConflict
	}
	s.rememberFlowDBQuickCheck(before.Path, quickCheck, "compact-precheck", time.Now())
	if before.ReclaimableBytes == 0 {
		return actionResponse{OK: true, Message: "database already compact", Output: "No SQLite free-list pages to reclaim."}, http.StatusOK
	}
	// Best-effort WAL cleanup before VACUUM. Some installs may not use WAL; a
	// checkpoint failure should not hide that VACUUM itself can still reclaim
	// free-list pages from the main database file.
	_, _ = s.cfg.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	if _, err := s.cfg.DB.Exec(`VACUUM`); err != nil {
		return actionResponse{OK: false, Message: "compact failed: " + err.Error()}, http.StatusInternalServerError
	}
	// VACUUM rewrote the file; recompute fresh so after/sidebar see real numbers.
	after := s.uiFlowDBFresh()
	reclaimed := before.Bytes - after.Bytes
	if reclaimed < 0 {
		reclaimed = 0
	}
	return actionResponse{
		OK:      true,
		Message: "compacted database; reclaimed " + humanByteSize(reclaimed),
		Output:  fmt.Sprintf("Before: %s on disk, %s reclaimable\nAfter: %s on disk, %s reclaimable", before.HumanSize, before.ReclaimableHumanSize, after.HumanSize, after.ReclaimableHumanSize),
	}, http.StatusOK
}

// nudgeSession delivers a user-typed instruction into a task's agent session
// without opening the terminal. It reuses the exact path the inbox monitor uses
// to auto-inject on new messages (see deliverInboxEvents): inject into the live
// server-managed PTY if one exists, otherwise resume the session and inject —
// but never duplicate a session the user is running in an external terminal.
func (s *Server) nudgeSession(slug, text string) (actionResponse, int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return actionResponse{OK: false, Message: "instruction is empty"}, http.StatusBadRequest
	}
	if slug == "" {
		return actionResponse{OK: false, Message: "no session specified"}, http.StatusBadRequest
	}
	task, err := flowdb.GetTask(s.cfg.DB, slug)
	if err != nil {
		return actionResponse{OK: false, Message: "session not found"}, http.StatusNotFound
	}
	if s.taskManuallyPaused(task) {
		notBefore := ""
		if hold, ok := s.taskProviderRateLimitHold(slug); ok {
			notBefore = hold.Until.UTC().Format(time.RFC3339)
		}
		if _, err := flowdb.EnqueuePausedSessionInputAfter(s.cfg.DB, slug, text, notBefore); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "Queued instruction; resume the session to deliver it."}, http.StatusOK
	}
	// Live server PTY → inject straight away (paste + delayed Enter).
	if s.terminals != nil && s.terminals.running(slug) {
		if err := s.terminals.wakeTask(slug, text); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "Instruction sent to session"}, http.StatusOK
	}
	// No browser PTY is attached in this server process, but the agent may still
	// be alive in its detached tmux session — the common case after a `flow ui
	// serve` restart, since the tmux session outlives the server. Inject straight
	// through tmux, mirroring the inbox monitor's deliverInboxEvents path. Without
	// this a still-live flow session is misread as a native (user-owned) one below
	// and the nudge is wrongly refused.
	if s.terminals != nil && s.terminals.wakeSharedTask(slug, text) {
		return actionResponse{OK: true, Message: "Instruction sent to session"}, http.StatusOK
	}
	// A native (user-owned terminal) session is alive: flow has no PTY to inject
	// into and must not spawn a duplicate.
	if s.taskAgentProcessLive(task) {
		return actionResponse{OK: false, Message: "Session is running in an external terminal — open it there to send."}, http.StatusConflict
	}
	if task.Status != "backlog" && task.Status != "in-progress" {
		return actionResponse{OK: false, Message: "Session is finished — reopen it to send an instruction."}, http.StatusConflict
	}
	provider := task.SessionProvider
	if provider == "" {
		provider = "claude"
	}
	if err := s.ensureProviderAvailable(provider); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	// No live session: wakeTask resumes a server PTY (--resume the prior session)
	// via attach, then injects the instruction.
	if err := s.terminals.wakeTask(slug, text); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "Resumed session and sent instruction"}, http.StatusOK
}

func qualifiedEntityRef(kind, slug string) (string, error) {
	switch strings.TrimSpace(kind) {
	case "task", "project", "playbook":
		return kind + "/" + slug, nil
	case "":
		return slug, nil
	default:
		return "", fmt.Errorf("invalid entity kind %q", kind)
	}
}

func (s *Server) pauseTask(target string) (actionResponse, int) {
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if err := s.terminals.stop(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	s.markTaskPaused(task)
	agent, err := s.agentForTask(target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "paused " + target + "; agent is idle", Agent: agent}, http.StatusOK
}

func (s *Server) markTaskPaused(task *flowdb.Task) {
	if s == nil || s.cfg.DB == nil || task == nil {
		return
	}
	provider := task.SessionProvider
	if provider == "" {
		provider = "claude"
	}
	_ = flowdb.PauseSession(s.cfg.DB, task.Slug, provider, task.SessionID.String)
}

func (s *Server) markLaunchResumed(launch terminalLaunch) bool {
	if s == nil || s.cfg.DB == nil || launch.FreeAgent {
		return false
	}
	return s.clearTaskPause(launch.Slug)
}

func (s *Server) taskManuallyPaused(task *flowdb.Task) bool {
	if task == nil {
		return false
	}
	provider := task.SessionProvider
	if provider == "" {
		provider = "claude"
	}
	return s.taskManuallyPausedByID(provider, task.Slug, task.SessionID.String)
}

func (s *Server) taskManuallyPausedByID(provider, slug, sessionID string) bool {
	if s == nil || s.cfg.DB == nil {
		return false
	}
	_, ok, err := flowdb.GetPausedSession(s.cfg.DB, slug)
	if err != nil || !ok {
		return false
	}
	if s.taskSessionLive(slug, sessionID) {
		moved := s.clearTaskPause(slug)
		if moved && s.terminals != nil {
			s.terminals.flushWakes(slug)
		}
		return false
	}
	return true
}

func (s *Server) clearTaskPause(slug string) bool {
	if s == nil || s.cfg.DB == nil {
		return false
	}
	n, err := flowdb.MovePausedSessionInputsToPendingWakes(s.cfg.DB, slug)
	if err != nil {
		return false
	}
	_ = flowdb.ClearPausedSession(s.cfg.DB, slug)
	return n > 0
}

func (s *Server) taskSessionLive(slug, sessionID string) bool {
	if s == nil {
		return false
	}
	if s.terminals != nil && (s.terminals.running(slug) || s.terminals.sharedRunning(slug)) {
		return true
	}
	live, err := s.cachedLiveAgentSessions()
	if err != nil {
		return false
	}
	if sid := strings.ToLower(strings.TrimSpace(sessionID)); sid != "" && live[sid] {
		return true
	}
	task, err := flowdb.GetTask(s.cfg.DB, slug)
	if err != nil || task == nil || task.SessionProvider != "codex" {
		return false
	}
	if k := codexDirLiveKey(task.WorkDir); k != "" && live[k] {
		return true
	}
	if task.WorktreePath.Valid {
		if k := codexDirLiveKey(task.WorktreePath.String); k != "" && live[k] {
			return true
		}
	}
	return false
}

func (s *Server) clearWaiting(target string) (actionResponse, int) {
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	if _, err := flowdb.GetTask(s.cfg.DB, target); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if _, err := s.cfg.DB.Exec(`UPDATE tasks SET waiting_on = NULL, updated_at = ? WHERE slug = ?`, flowdb.NowISO(), target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	agent, err := s.agentForTask(target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "cleared waiting block for " + target, Agent: agent, Bridge: true}, http.StatusOK
}

func (s *Server) markInboxRead(target string) (actionResponse, int) {
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	if _, err := flowdb.GetTask(s.cfg.DB, target); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	now := flowdb.NowISO()
	if _, err := s.cfg.DB.Exec(`UPDATE tasks SET inbox_seen_at = ?, updated_at = ? WHERE slug = ?`, now, now, target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "marked inbox read for " + target}, http.StatusOK
}

func (s *Server) killSession(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if target != "" {
		if err := validateSlug(target); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return actionResponse{OK: false, Message: "session_id is required"}, http.StatusBadRequest
	}
	if !safeSessionRe.MatchString(sessionID) {
		return actionResponse{OK: false, Message: "invalid session_id"}, http.StatusBadRequest
	}
	pid, err := claudePIDForSession(sessionID)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusNotFound
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return actionResponse{OK: false, Message: fmt.Sprintf("SIGTERM %d: %v", pid, err)}, http.StatusInternalServerError
	}
	if target == "" {
		target = sessionID[:8]
	}
	return actionResponse{OK: true, Message: fmt.Sprintf("sent SIGTERM to %s (pid %d)", target, pid)}, http.StatusOK
}

func (s *Server) ackApproval(kind, target string) (actionResponse, int) {
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	return actionResponse{
		OK:      true,
		Message: fmt.Sprintf("%s noted for %s; answer terminal-native prompts in the opened task terminal", kind, target),
	}, http.StatusOK
}

func (h *terminalHub) running(slug string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	sess := h.sessions[slug]
	return sess != nil && sess.running()
}

func (s *Server) agentForTask(slug string) (*uiAgent, error) {
	task, err := flowdb.GetTask(s.cfg.DB, slug)
	if err != nil {
		return nil, err
	}
	live, _ := s.cachedLiveAgentSessions()
	view, err := BuildTaskView(s.cfg.DB, s.cfg.FlowRoot, task, live)
	if err != nil {
		return nil, err
	}
	agent := s.uiAgent(view, live)
	if transcript := s.fullUITranscriptForTask(view); len(transcript) > 0 {
		agent.Transcript = transcript
		agent.RecentTools = recentTools(transcript)
	}
	return &agent, nil
}

func claudePIDForSession(sessionID string) (int, error) {
	out, err := psRunner()
	if err != nil {
		return 0, fmt.Errorf("ps: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(strings.ToLower(line), "claude") {
			continue
		}
		matches := claudeSessionArgRe.FindAllStringSubmatch(line, -1)
		for _, match := range matches {
			if len(match) < 2 || !strings.EqualFold(match[1], sessionID) {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				return 0, errors.New("matched session but could not read pid")
			}
			pid, err := strconv.Atoi(fields[0])
			if err != nil {
				return 0, fmt.Errorf("matched session but invalid pid %q", fields[0])
			}
			return pid, nil
		}
	}
	return 0, errors.New("no live Claude process found for session " + sessionID[:8])
}

func (s *Server) runFlowCommand(args ...string) (string, error) {
	exe := s.cfg.CommandPath
	if exe == "" {
		return "", errors.New("flow command path is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	text := string(out)
	if ctx.Err() == context.DeadlineExceeded {
		return text, errors.New("flow command timed out")
	}
	if err != nil {
		return text, fmt.Errorf("%s: %w", strings.Join(append([]string{exe}, args...), " "), err)
	}
	return text, nil
}

func validateSlug(slug string) error {
	if slug == "" {
		return errors.New("target slug is required")
	}
	if !safeSlugRe.MatchString(slug) || strings.Contains(slug, "..") {
		return fmt.Errorf("invalid slug %q", slug)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func writeJSONStatus(w http.ResponseWriter, v any, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
