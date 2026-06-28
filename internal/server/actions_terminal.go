package server

import (
	"database/sql"
	"errors"
	"flow/internal/flowdb"
	"flow/internal/iterm"
	"flow/internal/kitty"
	"flow/internal/spawner"
	macterminal "flow/internal/terminal"
	"flow/internal/warp"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func (s *Server) openBrowserTerminalBridge(target, providerChoice string) (actionResponse, int) {
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if task.Status != "done" {
		if err := flowdb.EnsureTaskStartable(s.cfg.DB, task); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, taskStartErrorStatus(err)
		}
	}
	if err := s.applyBacklogProviderChoice(target, providerChoice); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	agent, err := s.agentForTask(target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	provider := firstNonEmpty(agent.Provider, "claude")
	if err := s.ensureProviderAvailable(provider); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	return actionResponse{
		OK:      true,
		Message: "opening browser terminal for " + target,
		Agent:   agent,
		Bridge:  true,
	}, http.StatusOK
}

func (s *Server) restartBrowserTerminalBridge(target string) (actionResponse, int) {
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if task.Status == "done" {
		return actionResponse{OK: false, Message: "task " + target + " is done; move it back to in-progress before reopening"}, http.StatusBadRequest
	}
	if err := flowdb.EnsureTaskStartable(s.cfg.DB, task); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, taskStartErrorStatus(err)
	}
	provider := strings.TrimSpace(task.SessionProvider)
	if provider == "" {
		provider = "claude"
	}
	if err := s.ensureProviderAvailable(provider); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	s.terminals.stop(target)
	now := flowdb.NowISO()
	if strings.TrimSpace(task.SessionID.String) != "" {
		if _, err := s.cfg.DB.Exec(
			`UPDATE tasks SET
				status = 'in-progress',
				status_changed_at = CASE WHEN status != 'in-progress' THEN ? ELSE status_changed_at END,
				session_last_resumed = ?,
				updated_at = ?
			 WHERE slug = ?`,
			now, now, now, target,
		); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
	} else if _, err := s.cfg.DB.Exec(`UPDATE tasks SET updated_at = ? WHERE slug = ?`, now, target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	agent, _ := s.agentForTask(target)
	return actionResponse{OK: true, Message: "restarting browser terminal for " + target, Agent: agent, Bridge: true}, http.StatusOK
}

func (s *Server) restartFreshBrowserTerminalBridge(target string) (actionResponse, int) {
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if task.Status == "done" {
		return actionResponse{OK: false, Message: "task " + target + " is done; move it back to in-progress before reopening"}, http.StatusBadRequest
	}
	if err := flowdb.EnsureTaskStartable(s.cfg.DB, task); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, taskStartErrorStatus(err)
	}
	provider := strings.TrimSpace(task.SessionProvider)
	if provider == "" {
		provider = "claude"
	}
	if err := s.ensureProviderAvailable(provider); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	s.terminals.stop(target)
	now := flowdb.NowISO()
	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET
			status = 'backlog',
			status_changed_at = CASE WHEN status != 'backlog' THEN ? ELSE status_changed_at END,
			session_id = NULL,
			session_started = NULL,
			session_last_resumed = NULL,
			session_path = NULL,
			updated_at = ?
		 WHERE slug = ?`,
		now, now, target,
	); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	agent, _ := s.agentForTask(target)
	return actionResponse{OK: true, Message: "starting fresh browser terminal for " + target, Agent: agent, Bridge: true}, http.StatusOK
}

func (s *Server) openTaskBridge(target, terminalKind string, force bool) (actionResponse, int) {
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	provider := strings.TrimSpace(task.SessionProvider)
	if provider == "" {
		provider = "claude"
	}
	if err := s.ensureProviderAvailable(provider); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	// A done task is still resumable: opening it in a native terminal revisits
	// the prior session (loading its context), mirroring the browser bridge
	// which already special-cases done. Only gate startability for non-done.
	if task.Status != "done" {
		if err := flowdb.EnsureTaskStartable(s.cfg.DB, task); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, taskStartErrorStatus(err)
		}
	}
	sharedName, hasBrowserShared := s.terminals.sharedSessionName(target)
	createdShared := false
	if s.terminals.running(target) && !hasBrowserShared {
		return actionResponse{OK: false, Message: "browser terminal for " + target + " is already running without shared terminal multiplexing; stop it before opening a native shared terminal"}, http.StatusConflict
	}
	launch, err := s.prepareTerminalLaunch(target)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, taskStartErrorStatus(err)
	}
	if sharedName == "" {
		sharedName, createdShared, err = s.ensureSharedTerminalSession(launch, 120, 32)
		if err != nil {
			if launch.Created {
				s.rollbackPreparedTerminalLaunch(launch)
			}
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
	}
	command := "tmux attach-session -t " + shellQuoteArg(sharedName)
	if err := s.spawnNativeTerminalCommand(terminalKind, nativeTerminalTitle(task), launch.WorkDir, command, s.nativeTerminalEnv()); err != nil {
		if createdShared {
			_ = sharedTerminalKillSession(sharedName)
		}
		if launch.Created {
			s.rollbackPreparedTerminalLaunch(launch)
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if movedPaused := s.markLaunchResumed(launch); movedPaused && s.terminals != nil {
		go func() {
			waitForSharedSessionReady(sharedName, steererWakeStable, steererWakeTimeout)
			s.terminals.flushWakes(target)
		}()
	}
	agent, _ := s.agentForTask(target)
	if agent != nil {
		agent.Status = "running"
		agent.Terminal.Mode = "shared"
		agent.Terminal.Message = terminalModeMessage(firstNonEmpty(agent.Provider, "claude"), "shared")
	}
	return actionResponse{OK: true, Message: "opened " + target + " in " + terminalLabel(terminalKind), Agent: agent}, http.StatusOK
}

func (s *Server) spawnPlaybookRunBridge(target string, req actionRequest) (actionResponse, int) {
	provider, err := s.availableProvider(req.Provider)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	permissionMode, err := flowdb.NormalizePermissionMode(req.PermissionMode)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	pb, err := flowdb.GetPlaybook(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "playbook not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if pb.ArchivedAt.Valid {
		return actionResponse{OK: false, Message: "playbook " + target + " is archived"}, http.StatusBadRequest
	}
	if pb.DeletedAt.Valid {
		return actionResponse{OK: false, Message: "playbook " + target + " is deleted"}, http.StatusBadRequest
	}
	if strings.TrimSpace(pb.WorkDir) == "" {
		return actionResponse{OK: false, Message: "playbook " + target + " has no work_dir"}, http.StatusBadRequest
	}
	runSlug, err := s.createPlaybookRunTask(pb, provider, permissionMode)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	agent, err := s.agentForTask(runSlug)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{
		OK:      true,
		Message: "created playbook run " + runSlug + "; opening browser terminal",
		Agent:   agent,
		Bridge:  true,
	}, http.StatusOK
}

// closeFloatingTerminal ends an adhoc Ask Flow session: it terminates the PTY
// (if attached) and forgets the launch so the tray chip disappears. Idempotent
// — closing an already-gone session still returns OK so the UI can prune.
func (s *Server) closeFloatingTerminal(req actionRequest) (actionResponse, int) {
	id := strings.TrimSpace(req.Slug)
	if id == "" {
		id = strings.TrimSpace(req.Target)
	}
	if err := validateSlug(id); err != nil {
		return actionResponse{OK: false, Message: "floating terminal id: " + err.Error()}, http.StatusBadRequest
	}
	if s.terminals == nil {
		return actionResponse{OK: true, Message: "no terminal hub"}, http.StatusOK
	}
	s.terminals.stopFloating(id)
	return actionResponse{OK: true, Message: "closed floating terminal " + id}, http.StatusOK
}

func (s *Server) spawnNativeTerminalCommand(kind, title, workDir, command string, env map[string]string) error {
	switch kind {
	case "iterm":
		return iterm.SpawnTab(title, workDir, command, env)
	case "terminal":
		return macterminal.SpawnTab(title, workDir, command, env)
	case "kitty":
		return kitty.SpawnTab(title, workDir, command, env)
	case "warp":
		return warp.SpawnTab(title, workDir, command, env)
	case "alacritty":
		return startShellTerminal("alacritty", "Alacritty", workDir, command, env, "--working-directory", workDir, "-e")
	case "ghostty":
		return startShellTerminal("ghostty", "Ghostty", workDir, command, env, "--working-directory="+workDir, "-e")
	case "wezterm":
		args := append([]string{"start", "--cwd", workDir, "--"}, shellCommandArgs(command, env)...)
		return nativeCommandStarter("wezterm", "", args...)
	case "tmux":
		return nativeCommandStarter("tmux", "", "new-window", "-n", title, "-c", workDir, shellCommandLine(command, env))
	case "vscode":
		return nativeCommandStarter("code", "", "-n", "--reuse-window", workDir)
	default:
		return fmt.Errorf("unsupported terminal %q", kind)
	}
}

func agentShellCommand(provider string, args []string) string {
	bin := provider
	if bin == "" || bin == "claude" {
		bin = "claude"
	}
	parts := []string{bin}
	for _, arg := range args {
		parts = append(parts, shellQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuoteArg(arg string) string {
	if arg != "" {
		safe := true
		for _, r := range arg {
			if (r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') ||
				strings.ContainsRune("@%_+=:,./-", r) {
				continue
			}
			safe = false
			break
		}
		if safe {
			return arg
		}
	}
	return spawner.ShellQuote(arg)
}

func (s *Server) nativeTerminalEnv() map[string]string {
	env := map[string]string{}
	if root := strings.TrimSpace(s.cfg.FlowRoot); root != "" {
		env["FLOW_ROOT"] = root
	} else if root := os.Getenv("FLOW_ROOT"); root != "" {
		env["FLOW_ROOT"] = root
	}
	if hookURL := strings.TrimSpace(s.cfg.HookURL); hookURL != "" {
		env["FLOW_HOOK_URL"] = hookURL
	}
	if path := pathWithCommandDir(os.Getenv("PATH"), s.cfg.CommandPath); path != "" {
		env["PATH"] = path
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

func pathWithCommandDir(current, commandPath string) string {
	commandPath = strings.TrimSpace(commandPath)
	if commandPath == "" {
		return current
	}
	dir := filepath.Dir(commandPath)
	if dir == "." || dir == "" {
		return current
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for _, part := range filepath.SplitList(current) {
		if part == dir {
			return current
		}
	}
	if current == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + current
}

func nativeTerminalTitle(task *flowdb.Task) string {
	if task.ProjectSlug.Valid && task.ProjectSlug.String != "" {
		return task.ProjectSlug.String + "/" + task.Slug
	}
	return task.Slug
}

func shellCommandLine(command string, env map[string]string) string {
	prefix := ""
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+spawner.ShellQuote(env[k]))
		}
		prefix = strings.Join(parts, " ") + " "
	}
	return prefix + command
}

func shellCommandArgs(command string, env map[string]string) []string {
	return []string{"sh", "-lc", shellCommandLine(command, env)}
}

func startShellTerminal(bin, appName, cwd, command string, env map[string]string, args ...string) error {
	fullArgs := append(append([]string{}, args...), shellCommandArgs(command, env)...)
	if err := nativeCommandStarter(bin, "", fullArgs...); err == nil {
		return nil
	}
	return nativeCommandStarter("open", "", append([]string{"-na", appName, "--args"}, fullArgs...)...)
}

func startNativeCommand(name, dir string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = os.Environ()
	if dir != "" {
		cmd.Dir = dir
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(append([]string{name}, args...), " "), err)
	}
	return cmd.Process.Release()
}

func terminalLabel(kind string) string {
	switch kind {
	case "iterm":
		return "iTerm"
	case "terminal":
		return "Terminal.app"
	case "warp":
		return "Warp"
	case "kitty":
		return "kitty"
	case "alacritty":
		return "Alacritty"
	case "ghostty":
		return "Ghostty"
	case "wezterm":
		return "WezTerm"
	case "tmux":
		return "tmux"
	case "vscode":
		return "VS Code"
	default:
		return kind
	}
}

func taskStartErrorStatus(err error) int {
	var blocker *flowdb.TaskStartBlocker
	if errors.As(err, &blocker) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) switchBranch(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	branch := strings.TrimSpace(req.Branch)
	if err := validateBranch(branch); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if strings.TrimSpace(task.WorkDir) == "" {
		return actionResponse{OK: false, Message: "task " + target + " has no work_dir"}, http.StatusBadRequest
	}
	out, err := runGitCombined(task.WorkDir, "switch", branch)
	if err != nil && strings.Contains(branch, "/") {
		trackOut, trackErr := runGitCombined(task.WorkDir, "switch", "--track", branch)
		if trackErr == nil {
			out, err = trackOut, nil
		}
	}
	if err != nil {
		return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusConflict
	}
	// The branch and diff just changed; clear the per-workdir cache so the
	// agent snapshot we're about to return reflects the new HEAD.
	s.caches.invalidateWorkdir(task.WorkDir)
	agent, _ := s.agentForTask(target)
	return actionResponse{OK: true, Message: "switched " + target + " to " + branch, Output: out, Agent: agent}, http.StatusOK
}

func validateBranch(branch string) error {
	if branch == "" {
		return errors.New("branch is required")
	}
	if !safeBranchRe.MatchString(branch) ||
		strings.Contains(branch, "..") ||
		strings.Contains(branch, "@{") ||
		strings.HasPrefix(branch, "-") ||
		strings.HasSuffix(branch, "/") ||
		strings.HasSuffix(branch, ".") ||
		strings.Contains(branch, "//") {
		return fmt.Errorf("invalid branch %q", branch)
	}
	return nil
}

func runGitCombined(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(text))
	}
	return text, nil
}
