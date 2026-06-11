package app

// Two-process autonomous run design:
//
//	flow do --auto <slug>
//	  ├─ pre-allocates session_id + flips status to in-progress (same tx
//	  │  as the interactive path)
//	  ├─ autoLauncher: starts a DETACHED `flow __auto-exec <slug>` process
//	  │  (own session via Setsid, cwd = work_dir, stdout/stderr → logfile)
//	  ├─ records auto_run_status='running' + pid + started + log
//	  └─ returns immediately
//
//	flow __auto-exec <slug>   (the detached supervisor; hidden subcommand)
//	  ├─ builds the autonomous bootstrap prompt
//	  ├─ runs the task's provider headlessly (`claude -p` or `codex exec`),
//	  │  BLOCKING until it exits
//	  └─ finalizes auto_run_status: 'completed' if the session marked the
//	     task done, else 'dead'; clears the pid

import (
	"database/sql"
	"errors"
	"flow/internal/flowdb"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// withInjectionMarker separates the operator's one-off --with instruction
// from the standard autonomous bootstrap prompt.
const withInjectionMarker = "--- OPERATOR INSTRUCTION FOR THIS RUN ---"

type autoRunRequest struct {
	TaskSlug       string
	Provider       string
	SessionID      string
	Prompt         string
	WorkDir        string
	FlowRoot       string
	PermissionMode string
	Model          string
}

// autoLauncher starts a detached `flow __auto-exec <slug>` process with its
// stdout/stderr redirected to logPath. Overridable in tests.
var autoLauncher = func(slug, workDir, logPath, provider, permissionMode, model, injection string, env []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate flow binary: %w", err)
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logF.Close()

	exArgs := []string{"__auto-exec", slug}
	if provider != "" {
		exArgs = append(exArgs, "--provider", provider)
	}
	if permissionMode != "" {
		exArgs = append(exArgs, "--permission-mode", permissionMode)
	}
	if model != "" {
		exArgs = append(exArgs, "--model", model)
	}
	if injection != "" {
		// Forward the one-off instruction to the supervisor, which appends
		// it (behind the marker) to the autonomous prompt. Passed as a
		// distinct arg — no shell parsing — so any characters are safe.
		exArgs = append(exArgs, "--with", injection)
	}
	cmd := exec.Command(self, exArgs...)
	cmd.Dir = workDir
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = logF
	cmd.Stderr = logF
	// New session → detached from the parent's controlling terminal, so
	// it survives `flow do --auto` returning.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start auto supervisor: %w", err)
	}
	pid := cmd.Process.Pid
	// We never Wait — the supervisor finalizes its own DB status. Release
	// so it can be reparented to init when it exits.
	_ = cmd.Process.Release()
	return pid, nil
}

// autoRunner executes the headless autonomous run, blocking until the provider
// CLI exits. stdout/stderr inherit the supervisor's (already pointed at the
// run log by autoLauncher). Overridable in tests.
var autoRunner = func(req autoRunRequest) error {
	provider := req.Provider
	if provider == "" {
		provider = sessionProviderClaude
	}
	var cmd *exec.Cmd
	if provider == sessionProviderCodex {
		cmd = commandRunner("codex", codexExecCLIArgs(req.WorkDir, req.FlowRoot, req.PermissionMode, req.Model)...)
		cmd.Stdin = strings.NewReader(req.Prompt)
	} else {
		argv := append([]string{"--session-id", req.SessionID}, claudeModelArgs(req.Model)...)
		argv = append(argv, "-p", req.Prompt)
		argv = append(argv, claudePermissionArgs(req.PermissionMode)...)
		cmd = exec.Command("claude", argv...)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = autoRunEnv(req.FlowRoot, req.TaskSlug, provider, req.PermissionMode)
	return cmd.Run()
}

var codexExecVersion = func() string {
	cmd := exec.Command("codex", "exec", "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text != "" {
			return "unknown (" + text + ")"
		}
		return "unknown (" + err.Error() + ")"
	}
	return strings.TrimSpace(string(out))
}

// processAlive reports whether the process with the given pid is alive.
// Uses signal 0 — no signal is delivered; only error checking runs.
// Overridable in tests.
var processAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 performs error checking without delivering a signal:
	// nil → alive and ours; EPERM → alive but owned by another user.
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

func launchAutoRun(task *flowdb.Task, root, cwd, provider, permissionMode, model, injection string) (int, string, error) {
	runsDir := filepath.Join(root, "tasks", task.Slug, "auto-runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return 0, "", fmt.Errorf("mkdir %s: %w", runsDir, err)
	}
	// Timestamped filename (UTC) keeps a per-run log history. time.Now is
	// fine here — this is a filename, not a slug or a stored timestamp.
	logName := time.Now().UTC().Format("2006-01-02-150405") + ".log"
	logPath := filepath.Join(runsDir, logName)

	pid, err := autoLauncher(task.Slug, cwd, logPath, provider, permissionMode, model, injection, autoChildEnv())
	if err != nil {
		return 0, "", err
	}
	return pid, logPath, nil
}

func recordAutoRunLaunched(db *sql.DB, slug string, pid int, logPath string) error {
	now := flowdb.NowISO()
	_, err := db.Exec(
		`UPDATE tasks SET auto_run_status='running', auto_run_pid=?, auto_run_started=?,
		 auto_run_finished=NULL, auto_run_log=?, updated_at=? WHERE slug=?`,
		pid, now, logPath, now, slug,
	)
	return err
}

func finalizeAutoRun(db *sql.DB, slug, status string) error {
	now := flowdb.NowISO()
	_, err := db.Exec(
		`UPDATE tasks SET auto_run_status=?, auto_run_finished=?, auto_run_pid=NULL, updated_at=? WHERE slug=?`,
		status, now, now, slug,
	)
	return err
}

// reconcileAutoRun promotes a stale 'running' row to 'dead' when its
// supervisor pid is no longer alive (crash, kill -9, reboot — anything
// that prevented finalizeAutoRun from running). No-op for any other
// state. Mutates both the DB and the in-memory task. Best-effort: DB
// errors leave the in-memory copy untouched.
//
// auto_run_* is the run lifecycle (pid-based); agent_runtime_states stays
// the hook-driven activity signal. Deliberately separate — see task auto-runs D2.
func reconcileAutoRun(db *sql.DB, t *flowdb.Task) {
	if !t.AutoRunStatus.Valid || t.AutoRunStatus.String != "running" {
		return
	}
	if t.AutoRunPID.Valid && processAlive(int(t.AutoRunPID.Int64)) {
		return
	}
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`UPDATE tasks SET auto_run_status='dead', auto_run_finished=COALESCE(auto_run_finished, ?),
		 auto_run_pid=NULL WHERE slug=? AND auto_run_status='running'`,
		now, t.Slug,
	); err != nil {
		return
	}
	t.AutoRunStatus = sql.NullString{String: "dead", Valid: true}
	if !t.AutoRunFinished.Valid {
		t.AutoRunFinished = sql.NullString{String: now, Valid: true}
	}
	t.AutoRunPID = sql.NullInt64{}
}

func cmdAutoExec(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: __auto-exec requires a task slug")
		return 2
	}
	slug := args[0]
	fs := flagSet("__auto-exec")
	providerFlag := fs.String("provider", "", "session provider: claude or codex")
	permissionModeFlag := fs.String("permission-mode", "", "agent permission mode: default|auto|bypass")
	modelFlag := fs.String("model", "", "resolved session model")
	withInstr := fs.String("with", "", "one-off instruction to append to the autonomous prompt")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := openConcurrentDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	task, err := flowdb.GetTask(db, slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load task %q: %v\n", slug, err)
		return 1
	}
	provider := task.SessionProvider
	if provider == "" {
		provider = sessionProviderClaude
	}
	if *providerFlag != "" {
		var perr error
		provider, perr = flowdb.NormalizeSessionProvider(*providerFlag)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", perr)
			return 2
		}
	}
	permissionMode := task.PermissionMode
	if permissionMode == "" {
		permissionMode = flowdb.DefaultPermissionMode
	}
	if *permissionModeFlag != "" {
		var perr error
		permissionMode, perr = flowdb.NormalizePermissionMode(*permissionModeFlag)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", perr)
			return 2
		}
	}
	model := flowdb.NormalizeModel(*modelFlag)
	if provider != sessionProviderCodex && (!task.SessionID.Valid || task.SessionID.String == "") {
		fmt.Fprintf(os.Stderr, "error: task %q has no session_id; cannot run headlessly\n", slug)
		_ = finalizeAutoRun(db, slug, "dead")
		return 1
	}

	playbookSlug := ""
	if task.PlaybookSlug.Valid {
		playbookSlug = task.PlaybookSlug.String
	}
	prompt := buildAutoBootstrapPrompt(slug, task.Kind, playbookSlug)

	// Fork adaptation 6: append DependencyBootstrapNote for non-playbook-run tasks.
	if task.Kind != "playbook_run" {
		if note := flowdb.DependencyBootstrapNote(db, slug); note != "" {
			prompt += "\n\n" + note
		}
	}

	// Fork adaptation 2: append the operator's one-off instruction.
	if *withInstr != "" {
		prompt += "\n\n" + withInjectionMarker + "\n" + *withInstr
	}

	sessionID := ""
	if task.SessionID.Valid {
		sessionID = task.SessionID.String
	}
	cwd, _ := os.Getwd()
	root, _ := flowRoot()
	req := autoRunRequest{
		TaskSlug:       task.Slug,
		Provider:       provider,
		SessionID:      sessionID,
		Prompt:         prompt,
		WorkDir:        cwd,
		FlowRoot:       root,
		PermissionMode: permissionMode,
		Model:          model,
	}
	if provider == sessionProviderCodex {
		printCodexAutoHeader(req)
	}
	runErr := autoRunner(req)

	// Re-read status: the session may have called `flow done` on itself.
	// The self-done is the authoritative success signal.
	status := "dead"
	if runErr == nil {
		if final, gerr := flowdb.GetTask(db, slug); gerr == nil && final.Status == "done" {
			status = "completed"
		}
	}
	if err := finalizeAutoRun(db, slug, status); err != nil {
		fmt.Fprintf(os.Stderr, "warning: finalize auto run %q: %v\n", slug, err)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "auto run for %q failed: %v\n", slug, runErr)
		return 1
	}
	fmt.Printf("auto run for %q finished: %s\n", slug, status)
	return 0
}

func printCodexAutoHeader(req autoRunRequest) {
	fmt.Printf("codex version: %s\n", codexExecVersion())
	fmt.Printf("codex command: %s\n", agentShellCommand("codex", codexExecCLIArgs(req.WorkDir, req.FlowRoot, req.PermissionMode, req.Model)))
	fmt.Printf("prompt: stdin (%d bytes)\n\n", len(req.Prompt))
}

// autoChildEnv builds the environment for the detached supervisor process.
// It strips ambient task/session identity (so `flow show task` inside the run
// doesn't reverse-lookup the dispatch session) and overlays flowSessionEnv so
// hook attribution (FLOW_ROOT, PATH, FLOW_HOOK_OWNED) works correctly.
func autoChildEnv() []string {
	return autoRunEnv(os.Getenv("FLOW_ROOT"), "", "", "")
}

func autoRunEnv(root, taskSlug, provider, permissionMode string) []string {
	overlay := flowSessionEnv(root)
	if strings.TrimSpace(taskSlug) != "" {
		overlay["FLOW_TASK"] = taskSlug
	}
	if strings.TrimSpace(provider) != "" {
		overlay["FLOW_SESSION_PROVIDER"] = provider
	}
	if strings.TrimSpace(permissionMode) != "" {
		overlay["FLOW_PERMISSION_MODE"] = permissionMode
	}
	out := make([]string, 0, len(os.Environ())+len(overlay))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if isAmbientAgentContextEnv(name) {
			continue
		}
		if _, shadowed := overlay[name]; shadowed {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overlay {
		out = append(out, k+"="+v)
	}
	return out
}

func isAmbientAgentContextEnv(name string) bool {
	switch name {
	case "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID",
		"FLOW_TASK", "FLOW_SESSION_PROVIDER", "FLOW_PERMISSION_MODE":
		return true
	default:
		return false
	}
}

func codexExecCLIArgs(cwd, flowRootPath, permissionMode, model string) []string {
	args := append([]string{}, codexPermissionArgs(permissionMode)...)
	args = append(args, "exec", "--color", "never")
	if cwd != "" {
		args = append(args, "--cd", cwd)
	}
	args = appendCodexWritableRoots(args, cwd, flowRootPath)
	args = append(args, codexModelArgs(model)...)
	return append(args, "-")
}

// buildAutoBootstrapPrompt constructs the headless-run system prompt.
// Autonomous rules: no AskUserQuestion, no waiting, end-to-end execution,
// conservative on irreversible actions, run `flow done` when brief criteria met.
func buildAutoBootstrapPrompt(slug, kind, playbookSlug string) string {
	showStep := "2. Run: flow show task. Read the file at the brief: path AND every file under updates:. Files under other: are on-demand references."
	if kind == "playbook_run" {
		showStep = fmt.Sprintf(
			"2. Run: flow show playbook %s for context, then flow show task. Read the run brief at the brief: path (it is a frozen snapshot — your authoritative instructions) AND every file under updates:. Do NOT edit the run brief.",
			playbookSlug,
		)
	}

	return fmt.Sprintf(
		"You are an AUTONOMOUS, headless execution session for flow task %s. NO HUMAN IS WATCHING and there is no terminal to prompt. Work end to end on your own and then close yourself out.\n\n"+
			"Bootstrap (do these in order before any work):\n"+
			"1. Invoke the flow skill via the Skill tool — it governs workflows, the bootstrap contract, KB discipline, and scope-creep detection.\n"+
			showStep+"\n"+
			"3. If a project is listed on the task, run: flow show project <slug> and read its brief + updates.\n"+
			"4. Read AGENTS.md and/or CLAUDE.md in your work_dir and any nested convention files under subtrees you will modify — they override the brief.\n\n"+
			"Operating rules for autonomous mode:\n"+
			"- Do NOT use AskUserQuestion and do NOT wait for user input — there is no one to answer. Where the interactive workflow would ask, decide using best engineering judgment and proceed. Resolve any deferred/unclear brief sections yourself rather than stopping.\n"+
			"- Follow the repo's conventions (build/test commands, TDD if the repo expects it). Verify your work by running the tests before considering the task complete.\n"+
			"- Be conservative with irreversible or outward-facing actions: do NOT push branches, open PRs, deploy, or message anyone unless the brief EXPLICITLY authorizes it. Make and verify the local changes; leave publishing to the user.\n"+
			"- Your objective is to reach a state where you can run `flow done`. PERSIST toward it. Keep going through transient errors, and before declaring a blocker, EXHAUST reasonable avenues: try alternative approaches, re-read the brief and CLAUDE.md, search the codebase, retry flaky steps. Only stop as a LAST RESORT — when you have genuinely tried everything and truly cannot proceed without a human decision or external access you do not have. If you must stop, write a precise progress note to the task's updates/ directory stating exactly what is blocking and what you already tried, and exit WITHOUT marking the task done (it will surface as a failed autonomous run).\n\n"+
			"Closing out:\n"+
			"- When the brief's \"Done when\" criteria are met and your changes are verified, run: flow done %s. That flips the task to done and triggers the close-out sweep (KB + project update) — it is how this autonomous run is recorded as successful. Do this yourself; no human will.\n",
		slug, slug,
	)
}
