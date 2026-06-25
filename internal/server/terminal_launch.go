package server

import (
	"database/sql"
	"errors"
	"flow/internal/agenthooks"
	"flow/internal/agents"
	"flow/internal/flowdb"
	"flow/internal/workdirreg"
	"flow/internal/worktree"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

var terminalAltScreenRE = regexp.MustCompile(`\x1b\[\?([0-9;]*)([hl])`)
var terminalGeneratedInputRE = regexp.MustCompile(`\x1b\[(?:\?[0-9;]*|>[0-9;]*)c`)

func stripTerminalAltScreenControls(data []byte) []byte {
	return terminalAltScreenRE.ReplaceAllFunc(data, func(seq []byte) []byte {
		match := terminalAltScreenRE.FindSubmatch(seq)
		if len(match) < 2 {
			return seq
		}
		for _, mode := range strings.Split(string(match[1]), ";") {
			switch mode {
			case "47", "1047", "1048", "1049":
				return nil
			}
		}
		return seq
	})
}

func stripTerminalGeneratedInput(data string) string {
	return terminalGeneratedInputRE.ReplaceAllString(data, "")
}

func (s *Server) prepareTerminalLaunch(slug string) (terminalLaunch, error) {
	tx, err := s.cfg.DB.Begin()
	if err != nil {
		return terminalLaunch{}, err
	}
	defer tx.Rollback()

	task, err := flowdb.ScanTask(tx.QueryRow("SELECT "+flowdb.TaskCols+" FROM tasks WHERE slug = ?", slug))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return terminalLaunch{}, fmt.Errorf("task not found: %s", slug)
		}
		return terminalLaunch{}, err
	}
	if task.Slug == overviewTaskSlug {
		flowRoot := strings.TrimSpace(s.cfg.FlowRoot)
		if flowRoot == "" {
			return terminalLaunch{}, errors.New("flow root is not configured")
		}
		absRoot, err := filepath.Abs(flowRoot)
		if err != nil {
			return terminalLaunch{}, err
		}
		if err := os.MkdirAll(filepath.Join(absRoot, "tasks", overviewTaskSlug, "updates"), 0o755); err != nil {
			return terminalLaunch{}, err
		}
		now := flowdb.NowISO()
		if _, err := tx.Exec(
			`UPDATE tasks SET
				project_slug = NULL,
				status = 'backlog',
				kind = 'regular',
				playbook_slug = NULL,
				work_dir = ?,
					waiting_on = NULL,
					session_provider = 'claude',
					harness = 'claude',
					session_id = NULL,
				session_started = NULL,
				session_last_resumed = NULL,
				status_changed_at = ?,
				updated_at = ?
			 WHERE slug = ?`,
			absRoot, now, now, task.Slug,
		); err != nil {
			return terminalLaunch{}, err
		}
		task.ProjectSlug = sql.NullString{}
		task.Status = "backlog"
		task.Kind = "regular"
		task.PlaybookSlug = sql.NullString{}
		task.WorkDir = absRoot
		task.WaitingOn = sql.NullString{}
		task.SessionID = sql.NullString{}
		task.SessionStarted = sql.NullString{}
		task.SessionLastResumed = sql.NullString{}
	}
	if strings.TrimSpace(task.WorkDir) == "" {
		return terminalLaunch{}, fmt.Errorf("task %s has no work_dir", task.Slug)
	}
	if err := reconcileAutoRunBeforeTerminalLaunch(tx, task); err != nil {
		return terminalLaunch{}, err
	}
	// A done task only reaches here via revisit/resume: both bridge entry points
	// (openBrowserTerminalBridge, openTaskBridge) gate startability for non-done
	// and rely on this path to reload the prior session, flipping it back to
	// in-progress below. So skip the startability gate for done — revisit must
	// not be blocked by a now-unfinished dependency — while a fresh start of a
	// non-done task still gets the full check.
	if task.Status != "done" {
		if err := flowdb.EnsureTaskStartable(s.cfg.DB, task); err != nil {
			return terminalLaunch{}, err
		}
	}

	now := flowdb.NowISO()
	sessionID := strings.TrimSpace(task.SessionID.String)
	provider := task.SessionProvider
	if provider == "" {
		provider = agents.ProviderClaude
	}
	created := false
	if sessionID == "" {
		created = true
		if provider == agents.ProviderCodex {
			if _, err := tx.Exec(
				`UPDATE tasks SET
					status = 'in-progress',
						status_changed_at = ?,
						session_provider = 'codex',
						harness = 'codex',
						session_id = NULL,
					session_started = ?,
					updated_at = ?
				 WHERE slug = ?`,
				now, now, now, task.Slug,
			); err != nil {
				return terminalLaunch{}, err
			}
		} else {
			sessionID = uuid.NewString()
			if _, err := tx.Exec(
				`UPDATE tasks SET
					status = 'in-progress',
						status_changed_at = ?,
						session_provider = 'claude',
						harness = 'claude',
						session_id = ?,
					session_started = ?,
					updated_at = ?
				 WHERE slug = ?`,
				now, sessionID, now, now, task.Slug,
			); err != nil {
				return terminalLaunch{}, err
			}
		}
	} else {
		if _, err := tx.Exec(
			`UPDATE tasks SET
				status = 'in-progress',
				session_last_resumed = ?,
				updated_at = ?
			 WHERE slug = ?`,
			now, now, task.Slug,
		); err != nil {
			return terminalLaunch{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return terminalLaunch{}, err
	}

	originalWorkDir := task.WorkDir
	if task.Slug != overviewTaskSlug {
		wt, wtErr := worktree.Ensure(originalWorkDir, provider, task.Slug)
		if wtErr != nil {
			if created {
				s.rollbackPreparedTerminalLaunch(terminalLaunch{
					Slug:      task.Slug,
					SessionID: sessionID,
					Provider:  provider,
				})
			}
			return terminalLaunch{}, fmt.Errorf("worktree setup failed for %s: %w", task.Slug, wtErr)
		}
		if wt.IsRepo {
			task.WorkDir = wt.WorktreePath
			task.WorktreePath = sql.NullString{String: wt.WorktreePath, Valid: true}
			if _, err := s.cfg.DB.Exec(
				`UPDATE tasks SET worktree_path = ?, updated_at = ? WHERE slug = ?`,
				wt.WorktreePath, flowdb.NowISO(), task.Slug,
			); err != nil {
				if created {
					s.rollbackPreparedTerminalLaunch(terminalLaunch{
						Slug:      task.Slug,
						SessionID: sessionID,
						Provider:  provider,
					})
				}
				return terminalLaunch{}, fmt.Errorf("persist worktree_path: %w", err)
			}
		}
	}

	if err := workdirreg.Touch(s.cfg.DB, originalWorkDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: bump workdir last_used_at: %v\n", err)
	}
	if _, err := agenthooks.InstallLocalWithOptions(task.WorkDir, agenthooks.InstallOptions{
		CommandPath: s.cfg.CommandPath,
		HookURL:     s.cfg.HookURL,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: install local agent hooks: %v\n", err)
	}

	if created {
		prompt := buildBrowserTerminalBootstrapPrompt(s.cfg.DB, task)
		if task.Slug == overviewTaskSlug {
			prompt = overviewInitialPrompt(s.cfg.FlowRoot, task)
		}
		model := s.resolveTaskLaunchModel(task, provider, true)
		effort, err := s.resolveTaskLaunchEffort(task, provider, model)
		if err != nil {
			return terminalLaunch{}, err
		}
		args := agentTerminalArgs(provider, true, sessionID, task.WorkDir, s.cfg.FlowRoot, prompt, task.PermissionMode, model, effort)
		return terminalLaunch{
			Slug:           task.Slug,
			SessionID:      sessionID,
			Provider:       provider,
			PermissionMode: task.PermissionMode,
			WorkDir:        task.WorkDir,
			Args:           args,
			Created:        created,
			NeedsCapture:   provider == agents.ProviderCodex,
			StartedAt:      time.Now().Add(-2 * time.Second),
		}, nil
	}
	model := s.resolveTaskLaunchModel(task, provider, false)
	effort, err := s.resolveTaskLaunchEffort(task, provider, model)
	if err != nil {
		return terminalLaunch{}, err
	}
	args := agentTerminalArgs(provider, false, sessionID, task.WorkDir, s.cfg.FlowRoot, "", task.PermissionMode, model, effort)
	return terminalLaunch{
		Slug:           task.Slug,
		SessionID:      sessionID,
		Provider:       provider,
		PermissionMode: task.PermissionMode,
		WorkDir:        task.WorkDir,
		Args:           args,
		Created:        created,
	}, nil
}

func (s *Server) prepareOverviewFloatingLaunch(req actionRequest) (terminalLaunch, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return terminalLaunch{}, errors.New("prompt is required")
	}
	flowRoot := strings.TrimSpace(s.cfg.FlowRoot)
	if flowRoot == "" {
		return terminalLaunch{}, errors.New("flow root is not configured")
	}
	absRoot, err := filepath.Abs(flowRoot)
	if err != nil {
		return terminalLaunch{}, err
	}
	provider, err := flowdb.NormalizeSessionProvider(req.Provider)
	if err != nil {
		return terminalLaunch{}, err
	}
	permissionMode, err := flowdb.NormalizePermissionMode(req.PermissionMode)
	if err != nil {
		return terminalLaunch{}, err
	}
	sessionID := uuid.NewString()
	args := agentTerminalArgs(provider, true, sessionID, absRoot, absRoot, overviewBrief(prompt), permissionMode, "", "")
	return terminalLaunch{
		Slug:           "overview-" + uuid.NewString(),
		SessionID:      sessionID,
		Provider:       provider,
		PermissionMode: permissionMode,
		WorkDir:        absRoot,
		Args:           args,
		FreeAgent:      true,
		Created:        true,
		// Codex mints its own session id on launch and ignores the flow-generated
		// one, so the stub above never resolves to a rollout file — capture must
		// overwrite it (via captureCodexChatSession) or the Chats sidebar can never
		// show this chat's last-reply preview. Claude accepts --session-id, so its
		// stub is already correct and no capture is needed. Mirrors openNewSlackChat.
		NeedsCapture: provider == agents.ProviderCodex,
		StartedAt:    time.Now().Add(-2 * time.Second),
	}, nil
}

// captureCodexChatSession is the chat-table analogue of
// CaptureCodexSessionForTaskSince. Codex assigns a brand-new session id on every
// launch/resume, captured only after the process starts. For task slugs that id
// is written to the tasks table; chats are task-less, so that writeback never
// matches and a chat's session_id would stay empty/stale — leaving its
// transcript (and the Chats last-reply preview) unresolvable. When the slug is a
// live chat, this finds the freshly-spawned codex session file and persists its
// id onto the chat row. Returns the captured id, or "" if the slug isn't a chat
// or no matching session file exists yet.
func (s *terminalSession) captureCodexChatSession(started time.Time) string {
	db := s.hub.server.cfg.DB
	if db == nil {
		return ""
	}
	if _, err := flowdb.GetChat(db, s.slug); err != nil {
		return "" // not a chat (or deleted) — nothing to capture here
	}
	candidate, err := agents.FindCodexSessionForTask(s.slug, s.workDir, started)
	if err != nil || candidate.ID == "" {
		return ""
	}
	if err := flowdb.SetChatSession(db, s.slug, candidate.ID, flowdb.NowISO()); err != nil {
		return ""
	}
	s.hub.server.publishUIChange("chats")
	return candidate.ID
}

func (s *terminalSession) captureCodexSession(started time.Time) {
	if started.IsZero() {
		started = time.Now().Add(-2 * time.Second)
	}
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case <-s.done:
			return
		case <-deadline:
			return
		case <-ticker.C:
			captured, err := agents.CaptureCodexSessionForTaskSince(s.hub.server.cfg.DB, s.slug, s.workDir, started)
			if err != nil {
				continue
			}
			if captured == "" {
				// Chats are task-less, so the task-table writeback above matched
				// no row. Persist the captured id onto the chat row instead, so
				// the chat transcript (and its last-reply preview) resolves.
				// Non-chats simply keep waiting for the task row.
				captured = s.captureCodexChatSession(started)
			}
			if captured == "" {
				continue
			}
			s.mu.Lock()
			s.sessionID = captured
			clients := make([]*terminalClient, 0, len(s.clients))
			for client := range s.clients {
				clients = append(clients, client)
			}
			s.mu.Unlock()
			for _, client := range clients {
				client.queue(terminalWSMessage{Type: "status", Message: "connected to codex session " + captured})
			}
			return
		}
	}
}

func agentTerminalArgs(provider string, fresh bool, sessionID, workDir, flowRootPath, prompt, permissionMode, model, effort string) []string {
	if provider == agents.ProviderCodex {
		args := []string{"--no-alt-screen", "-C", workDir}
		args = appendCodexWritableRoot(args, workDir, flowRootPath)
		args = append(args, modelTerminalArgs(model)...)
		args = append(args, codexEffortArgs(effort)...)
		args = append(args, codexPermissionArgs(permissionMode)...)
		if fresh {
			return append(args, prompt)
		}
		resume := []string{"resume", "--include-non-interactive", "--no-alt-screen", "-C", workDir}
		resume = appendCodexWritableRoot(resume, workDir, flowRootPath)
		resume = append(resume, modelTerminalArgs(model)...)
		resume = append(resume, codexEffortArgs(effort)...)
		resume = append(resume, codexPermissionArgs(permissionMode)...)
		return append(resume, sessionID)
	}
	if fresh {
		args := []string{"--session-id", sessionID}
		args = append(args, modelTerminalArgs(model)...)
		args = append(args, claudeEffortArgs(effort)...)
		args = append(args, claudePermissionArgs(permissionMode)...)
		return append(args, prompt)
	}
	args := []string{"--resume", sessionID}
	args = append(args, modelTerminalArgs(model)...)
	args = append(args, claudeEffortArgs(effort)...)
	return append(args, claudePermissionArgs(permissionMode)...)
}

// modelTerminalArgs returns the `--model <m>` flag passed to claude/codex when
// the task pinned (or flow resolved) an explicit model, or nil to let the
// provider use its own default. Both CLIs take `--model`. This is what makes a
// UI-launched session honor tasks.model — the #30 model feature only threaded
// --model through `flow do`, never the server's terminal bridge, so web-UI
// sessions silently launched on the provider default (e.g. claude → Opus)
// regardless of the pinned model.
func modelTerminalArgs(model string) []string {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return []string{"--model", strings.TrimSpace(model)}
}

func claudeEffortArgs(effort string) []string {
	if strings.TrimSpace(effort) == "" {
		return nil
	}
	return []string{"--effort", strings.TrimSpace(effort)}
}

func codexEffortArgs(effort string) []string {
	if strings.TrimSpace(effort) == "" {
		return nil
	}
	return []string{"-c", "model_reasoning_effort=" + strings.TrimSpace(effort)}
}

// resolveTaskLaunchModel mirrors app.resolveLaunchModel for the server's
// terminal-bridge launches. On bootstrap (fresh) it runs flow's tier resolution
// — an explicit per-task pin wins, otherwise the baseline tier (default medium)
// is downshifted one rung when the brief is descriptive enough. On resume it
// passes only an explicit pin, never re-running the heuristic, so a live session
// never silently switches models mid-life. Empty result = pass no --model.
func (s *Server) resolveTaskLaunchModel(task *flowdb.Task, provider string, fresh bool) string {
	if task == nil {
		return ""
	}
	explicit := ""
	if task.Model.Valid {
		explicit = task.Model.String
	}
	if !fresh {
		return flowdb.NormalizeModel(explicit)
	}
	briefText := ""
	if root := strings.TrimSpace(s.cfg.FlowRoot); root != "" {
		if b, err := os.ReadFile(filepath.Join(root, "tasks", task.Slug, "brief.md")); err == nil {
			briefText = string(b)
		}
	}
	return flowdb.ResolveSessionModel(provider, explicit, briefText, task.Priority).Model
}

func (s *Server) resolveTaskLaunchEffort(task *flowdb.Task, provider, model string) (string, error) {
	explicit := ""
	if task != nil && task.Effort.Valid {
		explicit = task.Effort.String
	}
	return flowdb.ResolveSessionEffort(provider, model, explicit)
}

func reconcileAutoRunBeforeTerminalLaunch(tx *sql.Tx, task *flowdb.Task) error {
	if task == nil || !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "running" {
		return nil
	}
	pid := 0
	if task.AutoRunPID.Valid {
		pid = int(task.AutoRunPID.Int64)
	}
	if terminalProcessAlive(pid) {
		return fmt.Errorf("task %q autonomous run is already running (pid %d); wait for it to finish before opening an interactive session", task.Slug, pid)
	}
	now := flowdb.NowISO()
	if _, err := tx.Exec(
		`UPDATE tasks SET auto_run_status='dead', auto_run_finished=COALESCE(auto_run_finished, ?),
		 auto_run_pid=NULL, updated_at=? WHERE slug=? AND auto_run_status='running'`,
		now, now, task.Slug,
	); err != nil {
		return err
	}
	task.AutoRunStatus = sql.NullString{String: "dead", Valid: true}
	task.AutoRunPID = sql.NullInt64{}
	if !task.AutoRunFinished.Valid {
		task.AutoRunFinished = sql.NullString{String: now, Valid: true}
	}
	return nil
}

func terminalProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func appendCodexWritableRoot(args []string, workDir, flowRootPath string) []string {
	args = appendCodexAddDir(args, workDir, flowRootPath)
	return appendCodexAddDir(args, workDir, worktree.LinkedWorktreeGitCommonDir(workDir))
}

func appendCodexAddDir(args []string, workDir, dir string) []string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return args
	}
	cleanWorkDir := strings.TrimSpace(workDir)
	if cleanWorkDir != "" {
		if abs, err := filepath.Abs(cleanWorkDir); err == nil {
			cleanWorkDir = abs
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if cleanWorkDir == dir {
		return args
	}
	return append(args, "--add-dir", dir)
}

func claudePermissionArgs(mode string) []string {
	switch strings.TrimSpace(mode) {
	case "auto":
		return []string{"--permission-mode", "auto"}
	case "bypass":
		return []string{"--dangerously-skip-permissions"}
	default:
		// `default` is the moderate baseline: auto-accept file edits but
		// still prompt for execution. `auto` and `bypass` cover the more
		// permissive options.
		return []string{"--permission-mode", "acceptEdits"}
	}
}

func codexPermissionArgs(mode string) []string {
	// Codex's workspace-write sandbox blocks outbound network by default, which
	// breaks tools flow tasks routinely need — `gh` (PR create/edit), `git
	// push`, package installs — with "error connecting to api.github.com". Flip
	// network on for the sandboxed modes. The sandbox is all-or-nothing here
	// (Codex has no per-domain allowlist), so this enables full egress; `bypass`
	// already runs unsandboxed.
	const allowNetwork = "sandbox_workspace_write.network_access=true"
	switch strings.TrimSpace(mode) {
	case "auto":
		return []string{"--ask-for-approval", "never", "--sandbox", "workspace-write", "-c", allowNetwork}
	case "bypass":
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	default:
		return []string{"--ask-for-approval", "on-request", "--sandbox", "workspace-write", "-c", allowNetwork}
	}
}

func overviewInitialPrompt(root string, task *flowdb.Task) string {
	body, err := os.ReadFile(filepath.Join(root, "tasks", task.Slug, "brief.md"))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(body))
	const marker = "Latest user request:"
	if idx := strings.LastIndex(text, marker); idx >= 0 {
		return strings.TrimSpace(text[idx+len(marker):])
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "#") {
		text = strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}
	return text
}

func (s *Server) rollbackPreparedTerminalLaunch(launch terminalLaunch) {
	if launch.Slug == "" {
		return
	}
	if launch.Provider == agents.ProviderCodex && launch.SessionID == "" {
		if _, err := s.cfg.DB.Exec(
			`UPDATE tasks SET
				session_id = NULL,
				session_started = NULL,
				status = 'backlog',
				status_changed_at = NULL,
				updated_at = ?
			 WHERE slug = ? AND session_provider = 'codex' AND session_id IS NULL`,
			flowdb.NowISO(), launch.Slug,
		); err != nil {
			fmt.Fprintf(os.Stderr, "warning: rollback browser codex terminal session: %v\n", err)
		}
		return
	}
	if launch.SessionID == "" {
		return
	}
	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET
			session_id = NULL,
			session_started = NULL,
			status = 'backlog',
			status_changed_at = NULL,
			updated_at = ?
		 WHERE slug = ? AND session_id = ?`,
		flowdb.NowISO(), launch.Slug, launch.SessionID,
	); err != nil {
		fmt.Fprintf(os.Stderr, "warning: rollback browser terminal session: %v\n", err)
	}
}

func buildBrowserTerminalBootstrapPrompt(db *sql.DB, task *flowdb.Task) string {
	if task.Kind != "playbook_run" {
		prompt := fmt.Sprintf(
			"You are the execution session for flow task %s. Do ALL of the following in order before touching code:\n"+
				"1. Load the flow operating manual. If a Skill tool is available, invoke the flow skill via the Skill tool. Otherwise read ~/.codex/skills/flow/SKILL.md or ~/.claude/skills/flow/SKILL.md, whichever exists. This governs workflows, bootstrap contract, KB discipline, and scope-creep detection.\n"+
				"2. Run: flow show task. Read the file at the brief: path AND every file listed under updates:. Files listed under other: are sidecar references; load on demand when relevant, not eagerly.\n"+
				"3. If a project is listed on the task, run: flow show project <that-project-slug>. Read its brief AND every file under updates:. Files under other: are on-demand references.\n"+
				"4. Read AGENTS.md and/or CLAUDE.md in your work_dir and any nested convention files under subdirectories you will modify. These override any assumption from the brief.\n"+
				"5. When creating reports, generated data, screenshots, or other deliverables for this task, write them under $FLOW_ROOT/tasks/%s/artifacts/ (default ~/.flow/tasks/%s/artifacts/). Mission Control's Artifacts tab reads that directory.\n"+
				"6. Only then begin work. If any brief section is blank or unclear, ASK; do not infer.",
			task.Slug, task.Slug, task.Slug,
		)
		// Brief the session on upstream dependency work that may be unmerged.
		if note := flowdb.DependencyBootstrapNote(db, task.Slug); note != "" {
			prompt += "\n\n" + note
		}
		return prompt
	}
	playbookSlug := ""
	if task.PlaybookSlug.Valid {
		playbookSlug = task.PlaybookSlug.String
	}
	isFirstRun := false
	if playbookSlug != "" {
		var runCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM tasks WHERE playbook_slug = ? AND kind = 'playbook_run' AND archived_at IS NULL AND deleted_at IS NULL`,
			playbookSlug,
		).Scan(&runCount); err == nil {
			isFirstRun = runCount <= 1
		}
	}
	prompt := fmt.Sprintf(
		"You are running playbook %s as run %s. Do ALL of the following in order before executing anything:\n"+
			"1. Load the flow operating manual. If a Skill tool is available, invoke the flow skill via the Skill tool. Otherwise read ~/.codex/skills/flow/SKILL.md or ~/.claude/skills/flow/SKILL.md, whichever exists.\n"+
			"2. Run: flow show playbook %s. This shows the playbook definition and recent runs as context only, not your instructions.\n"+
			"3. Run: flow show task. Read the file at the brief: path AND every file listed under updates:. The brief is your authoritative snapshot for this run.\n"+
			"4. If a project is listed on the task, run: flow show project <that-project-slug>. Read its brief and every file under updates:.\n"+
			"5. Read AGENTS.md and/or CLAUDE.md in your work_dir.\n"+
			"6. Only then begin executing your brief.",
		playbookSlug, task.Slug, playbookSlug,
	)
	if isFirstRun {
		prompt += "\n\nThis is the first run of this playbook. Be proactive about asking whether scripts, decision rules, and edge cases discovered during the run should be captured back into the live playbook for future runs."
	}
	return prompt
}
