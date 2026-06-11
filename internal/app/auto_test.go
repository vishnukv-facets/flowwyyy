package app

import (
	"database/sql"
	"encoding/json"
	"flow/internal/flowdb"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// autoTestSetup initializes a temp FLOW_ROOT and returns the db path.
func autoTestSetup(t *testing.T) (flowRoot string, db *sql.DB) {
	t.Helper()
	tmp := t.TempDir()
	flowRoot = filepath.Join(tmp, "flow")
	t.Setenv("FLOW_ROOT", flowRoot)
	t.Setenv("HOME", tmp)

	if rc := cmdInit(nil); rc != 0 {
		t.Fatalf("flow init: rc=%d", rc)
	}

	var err error
	db, err = flowdb.OpenDB(filepath.Join(flowRoot, "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return flowRoot, db
}

// seedAutoTask inserts a task with an in-progress status and a session_id.
func seedAutoTask(t *testing.T, db *sql.DB, slug, sessionID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, work_dir, session_provider, session_id, kind, created_at, updated_at)
		 VALUES (?, ?, 'in-progress', '/tmp', 'claude', ?, 'regular', datetime('now'), datetime('now'))`,
		slug, "test task "+slug, sessionID,
	)
	if err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
}

// stubAutoRunner overrides autoRunner to capture the prompt and return retErr.
func stubAutoRunner(t *testing.T, retErr error) *string {
	t.Helper()
	captured := new(string)
	old := autoRunner
	autoRunner = func(req autoRunRequest) error {
		*captured = req.Prompt
		return retErr
	}
	t.Cleanup(func() { autoRunner = old })
	return captured
}

// stubProcessAlive overrides processAlive to return a fixed value.
func stubProcessAlive(t *testing.T, alive bool) {
	t.Helper()
	old := processAlive
	processAlive = func(pid int) bool { return alive }
	t.Cleanup(func() { processAlive = old })
}

func TestAutoExecFinalizesCompleted(t *testing.T) {
	_, db := autoTestSetup(t)
	stubClaudeRunner(t, nil)
	seedAutoTask(t, db, "at-comp", "sess-comp-1")

	// autoRunner stub: mark task done (simulates headless run calling flow done).
	old := autoRunner
	autoRunner = func(req autoRunRequest) error {
		if rc := cmdDone([]string{"at-comp"}); rc != 0 {
			return fmt.Errorf("cmdDone returned %d", rc)
		}
		return nil
	}
	t.Cleanup(func() { autoRunner = old })

	rc := cmdAutoExec([]string{"at-comp"})
	if rc != 0 {
		t.Fatalf("cmdAutoExec: rc=%d", rc)
	}

	task, err := flowdb.GetTask(db, "at-comp")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "completed" {
		t.Errorf("auto_run_status: got %q, want 'completed'", task.AutoRunStatus.String)
	}
	if task.AutoRunPID.Valid {
		t.Errorf("auto_run_pid should be NULL after finalize, got %d", task.AutoRunPID.Int64)
	}
	if !task.AutoRunFinished.Valid || task.AutoRunFinished.String == "" {
		t.Error("auto_run_finished should be set after finalize")
	}
}

func TestAutoExecFinalizesDead(t *testing.T) {
	_, db := autoTestSetup(t)
	seedAutoTask(t, db, "at-dead", "sess-dead-1")

	old := autoRunner
	autoRunner = func(req autoRunRequest) error {
		return fmt.Errorf("claude exited with code 1")
	}
	t.Cleanup(func() { autoRunner = old })

	rc := cmdAutoExec([]string{"at-dead"})
	if rc != 1 {
		t.Fatalf("cmdAutoExec should return 1 on runner error, got %d", rc)
	}

	task, err := flowdb.GetTask(db, "at-dead")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "dead" {
		t.Errorf("auto_run_status: got %q, want 'dead'", task.AutoRunStatus.String)
	}
	if task.AutoRunPID.Valid {
		t.Errorf("auto_run_pid should be NULL after finalize")
	}
	if !task.AutoRunFinished.Valid || task.AutoRunFinished.String == "" {
		t.Error("auto_run_finished should be set after finalize")
	}
}

func TestAutoExecRunsCodexWithoutSessionID(t *testing.T) {
	_, db := autoTestSetup(t)
	stubClaudeRunner(t, nil)
	_, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, work_dir, session_provider, kind, created_at, updated_at)
		 VALUES ('at-codex', 'codex auto task', 'in-progress', '/tmp', 'codex', 'regular', datetime('now'), datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("seed codex task: %v", err)
	}

	oldVersion := codexExecVersion
	codexExecVersion = func() string { return "codex-cli-exec test-version" }
	t.Cleanup(func() { codexExecVersion = oldVersion })

	called := false
	const codexSessionID = "codex-auto-thread-1"
	old := autoRunner
	autoRunner = func(req autoRunRequest) error {
		called = true
		if req.SessionID != "" {
			t.Fatalf("codex auto runner sessionID = %q, want empty", req.SessionID)
		}
		if req.Provider != sessionProviderCodex {
			t.Fatalf("codex auto runner provider = %q, want codex", req.Provider)
		}
		if !strings.Contains(req.Prompt, "flow done at-codex") {
			t.Fatalf("codex auto prompt missing closeout instruction:\n%s", req.Prompt)
		}
		t.Setenv("FLOW_TASK", "at-codex")
		t.Setenv("FLOW_SESSION_PROVIDER", sessionProviderCodex)
		t.Setenv("CODEX_THREAD_ID", codexSessionID)
		if rc := cmdDone([]string{"at-codex"}); rc != 0 {
			return fmt.Errorf("cmdDone returned %d", rc)
		}
		return nil
	}
	t.Cleanup(func() { autoRunner = old })

	rc := cmdAutoExec([]string{"at-codex"})
	if rc != 0 {
		t.Fatalf("cmdAutoExec codex: rc=%d", rc)
	}
	if !called {
		t.Fatal("autoRunner was not called for codex task without session_id")
	}

	task, err := flowdb.GetTask(db, "at-codex")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "completed" {
		t.Errorf("auto_run_status: got %q, want completed", task.AutoRunStatus.String)
	}
	if !task.SessionID.Valid || task.SessionID.String != codexSessionID {
		t.Errorf("session_id: got %+v, want %s", task.SessionID, codexSessionID)
	}
}

func TestAutoExecAppendsInjection(t *testing.T) {
	_, db := autoTestSetup(t)
	seedAutoTask(t, db, "at-inj", "sess-inj-1")
	stubClaudeRunner(t, nil)
	capturedPrompt := stubAutoRunner(t, nil)

	rc := cmdAutoExec([]string{"at-inj", "--with", "extra instruction here"})
	if rc != 0 {
		t.Fatalf("cmdAutoExec: rc=%d", rc)
	}

	if !strings.Contains(*capturedPrompt, withInjectionMarker) {
		t.Errorf("prompt missing withInjectionMarker; got:\n%s", *capturedPrompt)
	}
	if !strings.Contains(*capturedPrompt, "extra instruction here") {
		t.Errorf("prompt missing injected text; got:\n%s", *capturedPrompt)
	}
}

func TestReconcileAutoRunDeadPid(t *testing.T) {
	_, db := autoTestSetup(t)
	stubProcessAlive(t, false)

	_, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, work_dir, session_provider, session_id, kind,
		 auto_run_status, auto_run_pid, created_at, updated_at)
		 VALUES ('at-recon', 'reconcile test', 'in-progress', '/tmp', 'claude', 'sess-recon-1', 'regular',
		 'running', 99999, datetime('now'), datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	task, err := flowdb.GetTask(db, "at-recon")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}

	reconcileAutoRun(db, task)

	if !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "dead" {
		t.Errorf("in-memory auto_run_status: got %q, want 'dead'", task.AutoRunStatus.String)
	}
	if task.AutoRunPID.Valid {
		t.Errorf("in-memory auto_run_pid should be zero after reconcile")
	}

	reloaded, err := flowdb.GetTask(db, "at-recon")
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if !reloaded.AutoRunStatus.Valid || reloaded.AutoRunStatus.String != "dead" {
		t.Errorf("db auto_run_status: got %q, want 'dead'", reloaded.AutoRunStatus.String)
	}
}

func TestReconcileAutoRunLivePidStaysRunning(t *testing.T) {
	_, db := autoTestSetup(t)

	pid := os.Getpid()
	_, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, work_dir, session_provider, session_id, kind,
		 auto_run_status, auto_run_pid, created_at, updated_at)
		 VALUES ('at-live', 'live pid test', 'in-progress', '/tmp', 'claude', 'sess-live-1', 'regular',
		 'running', ?, datetime('now'), datetime('now'))`,
		pid,
	)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	task, err := flowdb.GetTask(db, "at-live")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}

	reconcileAutoRun(db, task)

	if !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "running" {
		t.Errorf("auto_run_status should remain 'running' for live pid, got %q", task.AutoRunStatus.String)
	}
}

func TestBuildAutoBootstrapPrompt(t *testing.T) {
	prompt := buildAutoBootstrapPrompt("my-task", "task", "")

	checks := []string{
		"my-task",
		"flow done my-task",
		"AskUserQuestion",
		"PERSIST",
		"LAST RESORT",
		"EXHAUST",
		"NO HUMAN IS WATCHING",
	}
	for _, s := range checks {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing %q", s)
		}
	}
}

func TestBuildAutoBootstrapPromptPlaybookRun(t *testing.T) {
	prompt := buildAutoBootstrapPrompt("run-001", "playbook_run", "my-playbook")

	if !strings.Contains(prompt, "my-playbook") {
		t.Error("playbook_run prompt should mention playbook slug")
	}
	if !strings.Contains(prompt, "frozen snapshot") {
		t.Error("playbook_run prompt should mention frozen snapshot")
	}
}

func TestAutoChildEnvStripsSessionID(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "should-be-stripped")
	t.Setenv("CODEX_THREAD_ID", "should-be-stripped")
	t.Setenv("CODEX_SESSION_ID", "should-be-stripped")
	t.Setenv("FLOW_TASK", "parent-task")
	t.Setenv("FLOW_SESSION_PROVIDER", sessionProviderCodex)
	t.Setenv("FLOW_PERMISSION_MODE", "bypass")
	t.Setenv("FLOW_ROOT", "/tmp/test-flow-root")

	env := autoChildEnv()
	for _, stripped := range []string{
		"CLAUDE_CODE_SESSION_ID=",
		"CODEX_THREAD_ID=",
		"CODEX_SESSION_ID=",
		"FLOW_TASK=",
		"FLOW_SESSION_PROVIDER=",
		"FLOW_PERMISSION_MODE=",
	} {
		for _, kv := range env {
			if strings.HasPrefix(kv, stripped) {
				t.Errorf("%s should be stripped from child env, got %q", strings.TrimSuffix(stripped, "="), kv)
			}
		}
	}
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "FLOW_ROOT=") {
			found = true
			break
		}
	}
	if !found {
		t.Error("FLOW_ROOT should be present in child env via flowSessionEnv overlay")
	}
}

func TestCodexExecCLIArgsUseSandboxedNoApproval(t *testing.T) {
	args := codexExecCLIArgs("/tmp/work", "/tmp/flow-root", "auto", "gpt-5.4-mini")
	want := []string{
		"--ask-for-approval", "never",
		"--sandbox", "workspace-write",
		"exec",
		"--color", "never",
		"--cd", "/tmp/work",
		"--add-dir", "/tmp/flow-root",
		"--model", "gpt-5.4-mini",
		"-",
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("codex exec args = %#v, want %#v", args, want)
	}
	if testContainsString(args, "--json") {
		t.Fatalf("codex exec args should use plain log output, got %#v", args)
	}
}

func TestCodexExecCLIArgsBypassIsExplicit(t *testing.T) {
	args := codexExecCLIArgs("/tmp/work", "/tmp/flow-root", "bypass", "")
	if !testContainsString(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("bypass args missing dangerous bypass flag: %#v", args)
	}
	if testContainsString(args, "--sandbox") || testContainsString(args, "--ask-for-approval") {
		t.Fatalf("bypass args should not carry sandboxed approval flags: %#v", args)
	}
}

func TestCodexAutoRunEnvSetsTaskProviderAndPermission(t *testing.T) {
	t.Setenv("FLOW_ROOT", "/tmp/test-flow-root")
	t.Setenv("CODEX_THREAD_ID", "parent-codex-thread")
	t.Setenv("CODEX_SESSION_ID", "parent-codex-session")
	env := autoRunEnv(os.Getenv("FLOW_ROOT"), "codex-env-task", sessionProviderCodex, "auto")
	for _, want := range []string{
		"FLOW_TASK=codex-env-task",
		"FLOW_SESSION_PROVIDER=codex",
		"FLOW_PERMISSION_MODE=auto",
		"FLOW_ROOT=/tmp/test-flow-root",
		"FLOW_HOOK_OWNED=1",
	} {
		if !testContainsString(env, want) {
			t.Fatalf("auto run env missing %q: %#v", want, env)
		}
	}
	for _, stripped := range []string{"CODEX_THREAD_ID=", "CODEX_SESSION_ID="} {
		for _, kv := range env {
			if strings.HasPrefix(kv, stripped) {
				t.Fatalf("%s should not leak into codex auto run env: %#v", strings.TrimSuffix(stripped, "="), env)
			}
		}
	}
}

type autoRunnerHelperCapture struct {
	Name  string   `json:"name"`
	Args  []string `json:"args"`
	Env   []string `json:"env"`
	Stdin string   `json:"stdin"`
}

func TestAutoRunnerCodexExecCommand(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("GO_WANT_AUTO_RUNNER_HELPER", "1")
	t.Setenv("AUTO_RUNNER_HELPER_CAPTURE", capturePath)

	old := commandRunner
	commandRunner = func(name string, args ...string) *exec.Cmd {
		helperArgs := append([]string{"-test.run=TestAutoRunnerHelperProcess", "--", name}, args...)
		return exec.Command(os.Args[0], helperArgs...)
	}
	t.Cleanup(func() { commandRunner = old })

	req := autoRunRequest{
		TaskSlug:       "codex-runner-task",
		Provider:       sessionProviderCodex,
		Prompt:         "do the autonomous work",
		WorkDir:        "/tmp/work",
		FlowRoot:       "/tmp/flow-root",
		PermissionMode: "auto",
		Model:          "gpt-5.4-mini",
	}
	if err := autoRunner(req); err != nil {
		t.Fatalf("autoRunner codex: %v", err)
	}

	var cap autoRunnerHelperCapture
	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if err := json.Unmarshal(data, &cap); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	if cap.Name != "codex" {
		t.Fatalf("command name = %q, want codex", cap.Name)
	}
	wantArgs := codexExecCLIArgs("/tmp/work", "/tmp/flow-root", "auto", "gpt-5.4-mini")
	if strings.Join(cap.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("command args = %#v, want %#v", cap.Args, wantArgs)
	}
	if cap.Stdin != "do the autonomous work" {
		t.Fatalf("stdin = %q, want prompt", cap.Stdin)
	}
	for _, want := range []string{
		"FLOW_TASK=codex-runner-task",
		"FLOW_SESSION_PROVIDER=codex",
		"FLOW_PERMISSION_MODE=auto",
		"FLOW_ROOT=/tmp/flow-root",
	} {
		if !testContainsString(cap.Env, want) {
			t.Fatalf("command env missing %q: %#v", want, cap.Env)
		}
	}
}

func TestAutoExecLogsCodexCommandHeader(t *testing.T) {
	_, db := autoTestSetup(t)
	_, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, work_dir, session_provider, kind, created_at, updated_at)
		 VALUES ('at-codex-header', 'codex auto header task', 'in-progress', '/tmp', 'codex', 'regular', datetime('now'), datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("seed codex task: %v", err)
	}

	oldVersion := codexExecVersion
	codexExecVersion = func() string { return "codex-cli-exec test-version" }
	t.Cleanup(func() { codexExecVersion = oldVersion })

	oldRunner := autoRunner
	autoRunner = func(req autoRunRequest) error {
		return fmt.Errorf("stop after header")
	}
	t.Cleanup(func() { autoRunner = oldRunner })

	out := captureStdout(t, func() {
		if rc := cmdAutoExec([]string{"at-codex-header", "--provider", "codex", "--permission-mode", "auto", "--model", "gpt-anything-preview"}); rc != 1 {
			t.Fatalf("cmdAutoExec rc=%d, want 1 from stub runner", rc)
		}
	})
	for _, want := range []string{
		"codex version: codex-cli-exec test-version",
		"codex command: codex --ask-for-approval never --sandbox workspace-write exec --color never",
		"--model gpt-anything-preview",
		"prompt: stdin",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("header missing %q; got:\n%s", want, out)
		}
	}
}

func TestAutoRunnerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_AUTO_RUNNER_HELPER") != "1" {
		return
	}
	idx := -1
	for i, arg := range os.Args {
		if arg == "--" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(os.Args) {
		os.Exit(2)
	}
	stdin, _ := io.ReadAll(os.Stdin)
	cap := autoRunnerHelperCapture{
		Name:  os.Args[idx+1],
		Args:  append([]string{}, os.Args[idx+2:]...),
		Env:   os.Environ(),
		Stdin: string(stdin),
	}
	data, err := json.Marshal(cap)
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("AUTO_RUNNER_HELPER_CAPTURE"), data, 0o644); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

// ── Stage 3: cmdDo --auto integration ──────────────────────────────────────

// noTabStub pins the spawner to iTerm and stubs iterm.Runner to a no-op,
// then returns a func() that returns how many times SpawnTab was called.
func noTabStub(t *testing.T) func() int64 {
	t.Helper()
	count, _ := stubITerm(t)
	return func() int64 { return *count }
}

// stubLauncherRecord overrides autoLauncher to record its last call and return pid 4242.
type launcherCall struct {
	slug           string
	workDir        string
	logPath        string
	provider       string
	permissionMode string
	model          string
	injection      string
}

func stubLauncherRecord(t *testing.T, retErr error) *launcherCall {
	t.Helper()
	rec := &launcherCall{}
	old := autoLauncher
	autoLauncher = func(slug, workDir, logPath, provider, permissionMode, model, injection string, env []string) (int, error) {
		rec.slug = slug
		rec.workDir = workDir
		rec.logPath = logPath
		rec.provider = provider
		rec.permissionMode = permissionMode
		rec.model = model
		rec.injection = injection
		if retErr != nil {
			return 0, retErr
		}
		return 4242, nil
	}
	t.Cleanup(func() { autoLauncher = old })
	return rec
}

func TestCmdDoAutoLaunchesDetached(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "auto-det")

	const fixedSID = "auto-det-session-uuid"
	oldUUID := newUUID
	newUUID = func() (string, error) { return fixedSID, nil }
	t.Cleanup(func() { newUUID = oldUUID })

	tabCount := noTabStub(t)
	rec := stubLauncherRecord(t, nil)

	rc := cmdDo([]string{"auto-det", "--auto"})
	if rc != 0 {
		t.Fatalf("cmdDo --auto: rc=%d", rc)
	}

	// No iTerm tab should be spawned.
	if n := tabCount(); n != 0 {
		t.Errorf("SpawnTab called %d times, want 0", n)
	}

	// Launcher received the right slug.
	if rec.slug != "auto-det" {
		t.Errorf("launcher slug = %q, want 'auto-det'", rec.slug)
	}
	// Log path is under tasks/<slug>/auto-runs/.
	if !strings.Contains(rec.logPath, filepath.Join("tasks", "auto-det", "auto-runs")) {
		t.Errorf("log path %q missing expected dir", rec.logPath)
	}

	// DB: in-progress, session_id set, auto_run_status=running, pid=4242, started set.
	db := openFlowDB(t)
	task, err := flowdb.GetTask(db, "auto-det")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", task.Status)
	}
	if !task.SessionID.Valid || task.SessionID.String != fixedSID {
		t.Errorf("session_id = %q, want %q", task.SessionID.String, fixedSID)
	}
	if !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "running" {
		t.Errorf("auto_run_status = %q, want running", task.AutoRunStatus.String)
	}
	if !task.AutoRunPID.Valid || task.AutoRunPID.Int64 != 4242 {
		t.Errorf("auto_run_pid = %d, want 4242", task.AutoRunPID.Int64)
	}
	if !task.AutoRunStarted.Valid || task.AutoRunStarted.String == "" {
		t.Error("auto_run_started should be set")
	}
	if task.AutoRunFinished.Valid {
		t.Error("auto_run_finished should be NULL")
	}
}

func TestCmdDoAutoRejectsHere(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "here-conflict")
	rc := cmdDo([]string{"here-conflict", "--auto", "--here"})
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

func TestCmdDoWithRequiresAuto(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "with-no-auto")
	noTabStub(t)
	rc := cmdDo([]string{"with-no-auto", "--with", "some instruction"})
	if rc != 2 {
		t.Errorf("rc = %d, want 2 (--with without --auto)", rc)
	}
}

func TestCmdDoAutoCodexLaunchesDetached(t *testing.T) {
	setupFlowRoot(t)
	const orchestratorModel = "gpt-anything-preview"
	if rc := cmdAdd([]string{"task", "codex-auto", "--agent", "codex", "--model", orchestratorModel}); rc != 0 {
		t.Fatalf("add codex task rc=%d", rc)
	}
	noTabStub(t)
	rec := stubLauncherRecord(t, nil)
	rc := cmdDo([]string{"codex-auto", "--auto"})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if rec.slug != "codex-auto" {
		t.Errorf("launcher slug = %q, want codex-auto", rec.slug)
	}
	if rec.provider != sessionProviderCodex {
		t.Errorf("launcher provider = %q, want codex", rec.provider)
	}
	if rec.permissionMode != "auto" {
		t.Errorf("launcher permission mode = %q, want auto", rec.permissionMode)
	}
	if rec.model != orchestratorModel {
		t.Errorf("launcher model = %q, want %s", rec.model, orchestratorModel)
	}

	db := openFlowDB(t)
	task, err := flowdb.GetTask(db, "codex-auto")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", task.Status)
	}
	if task.SessionProvider != sessionProviderCodex {
		t.Errorf("session_provider = %q, want codex", task.SessionProvider)
	}
	if task.SessionID.Valid && task.SessionID.String != "" {
		t.Errorf("codex auto should not preallocate session_id, got %q", task.SessionID.String)
	}
	if !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "running" {
		t.Errorf("auto_run_status = %q, want running", task.AutoRunStatus.String)
	}
}

func TestCmdDoAutoWithInjection(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "with-inj")
	noTabStub(t)
	rec := stubLauncherRecord(t, nil)

	rc := cmdDo([]string{"with-inj", "--auto", "--with", "extra step: verify X"})
	if rc != 0 {
		t.Fatalf("cmdDo --auto --with: rc=%d", rc)
	}
	if rec.injection != "extra step: verify X" {
		t.Errorf("launcher injection = %q, want 'extra step: verify X'", rec.injection)
	}
}

func TestCmdDoAutoRefusesWhenAlreadyRunning(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "already-running")
	noTabStub(t)
	stubProcessAlive(t, true)

	// Seed the task as already having a running auto run.
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id='sess-ar', auto_run_status='running', auto_run_pid=9999, session_started=datetime('now'), status='in-progress' WHERE slug='already-running'`,
	); err != nil {
		t.Fatalf("seed running state: %v", err)
	}

	rc := cmdDo([]string{"already-running", "--auto"})
	if rc != 1 {
		t.Errorf("rc = %d, want 1 (already in flight)", rc)
	}

	// With --force: should succeed.
	rec := stubLauncherRecord(t, nil)
	rc = cmdDo([]string{"already-running", "--auto", "--force"})
	if rc != 0 {
		t.Errorf("--force rc = %d, want 0", rc)
	}
	if rec.slug != "already-running" {
		t.Errorf("launcher not called with right slug under --force")
	}
}

func TestCmdDoAutoLaunchFailureRollsBack(t *testing.T) {
	setupFlowRoot(t)
	seedTask(t, "launch-fail")
	noTabStub(t)
	stubLauncherRecord(t, fmt.Errorf("supervisor spawn error"))

	oldUUID := newUUID
	newUUID = func() (string, error) { return "fail-session-uuid", nil }
	t.Cleanup(func() { newUUID = oldUUID })

	rc := cmdDo([]string{"launch-fail", "--auto"})
	if rc != 1 {
		t.Errorf("rc = %d, want 1 on launch failure", rc)
	}

	// session_id should be rolled back to NULL, status back to backlog.
	db := openFlowDB(t)
	task, err := flowdb.GetTask(db, "launch-fail")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != "backlog" {
		t.Errorf("status = %q after rollback, want backlog", task.Status)
	}
	if task.SessionID.Valid && task.SessionID.String != "" {
		t.Errorf("session_id = %q after rollback, want NULL", task.SessionID.String)
	}
}

func TestCmdDoAutoCodexLaunchFailureRollsBack(t *testing.T) {
	setupFlowRoot(t)
	if rc := cmdAdd([]string{"task", "codex launch fail", "--slug", "codex-launch-fail", "--agent", "codex"}); rc != 0 {
		t.Fatalf("add codex task rc=%d", rc)
	}
	noTabStub(t)
	stubLauncherRecord(t, fmt.Errorf("supervisor spawn error"))

	rc := cmdDo([]string{"codex-launch-fail", "--auto"})
	if rc != 1 {
		t.Errorf("rc = %d, want 1 on launch failure", rc)
	}

	db := openFlowDB(t)
	task, err := flowdb.GetTask(db, "codex-launch-fail")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != "backlog" {
		t.Errorf("status = %q after rollback, want backlog", task.Status)
	}
	if task.SessionID.Valid && task.SessionID.String != "" {
		t.Errorf("session_id = %q after rollback, want NULL", task.SessionID.String)
	}
	if task.AutoRunStatus.Valid {
		t.Errorf("auto_run_status = %q after rollback, want NULL", task.AutoRunStatus.String)
	}
}

// ---------- Stage 4: list/show surfacing ----------

func TestListTasksAutoColumn(t *testing.T) {
	root, db := showListEditDB(t)
	insertTask(t, db, "auto-list-task", "Auto List Task", "in-progress", "medium", filepath.Join(root, "repo"), nil)
	// Directly set auto_run fields as if recordAutoRunLaunched had been called.
	_, err := db.Exec(
		`UPDATE tasks SET auto_run_status='running', auto_run_pid=55555, auto_run_started=datetime('now') WHERE slug='auto-list-task'`,
	)
	if err != nil {
		t.Fatalf("set auto_run fields: %v", err)
	}
	// PID is alive — reconcile must not flip it to dead.
	stubProcessAlive(t, true)

	out := captureStdout(t, func() {
		if rc := cmdList([]string{"tasks"}); rc != 0 {
			t.Fatalf("cmdList: rc=%d", rc)
		}
	})

	if !strings.Contains(out, "[auto]") {
		t.Errorf("list output missing [auto] marker; got:\n%s", out)
	}
	if !strings.Contains(out, "AUTO") {
		t.Errorf("list output missing AUTO header; got:\n%s", out)
	}
}

func TestListTasksAutoColumnCompleted(t *testing.T) {
	root, db := showListEditDB(t)
	insertTask(t, db, "auto-done-task", "Auto Done Task", "done", "medium", filepath.Join(root, "repo"), nil)
	_, err := db.Exec(
		`UPDATE tasks SET auto_run_status='completed', auto_run_finished=datetime('now') WHERE slug='auto-done-task'`,
	)
	if err != nil {
		t.Fatalf("set auto_run fields: %v", err)
	}

	out := captureStdout(t, func() {
		cmdList([]string{"tasks", "--status", "done"})
	})

	if !strings.Contains(out, "[done]") {
		t.Errorf("list output missing [done] marker for completed auto run; got:\n%s", out)
	}
}

func TestShowTaskAutoRunLines(t *testing.T) {
	root, db := showListEditDB(t)
	insertTask(t, db, "auto-show-task", "Auto Show Task", "in-progress", "high", filepath.Join(root, "repo"), nil)
	_, err := db.Exec(
		`UPDATE tasks SET auto_run_status='running', auto_run_pid=7777, auto_run_started=datetime('now'),
		 auto_run_log='/tmp/auto-runs/2026-06-10-120000.log' WHERE slug='auto-show-task'`,
	)
	if err != nil {
		t.Fatalf("set auto_run fields: %v", err)
	}
	stubProcessAlive(t, true)

	out := captureStdout(t, func() {
		if rc := cmdShow([]string{"task", "auto-show-task"}); rc != 0 {
			t.Fatalf("cmdShow: rc=%d", rc)
		}
	})

	if !strings.Contains(out, "auto_run:") {
		t.Errorf("show output missing auto_run: line; got:\n%s", out)
	}
	if !strings.Contains(out, "7777") {
		t.Errorf("show output missing pid 7777; got:\n%s", out)
	}
	if !strings.Contains(out, "auto_run_log:") {
		t.Errorf("show output missing auto_run_log: line; got:\n%s", out)
	}
	if !strings.Contains(out, "/tmp/auto-runs/") {
		t.Errorf("show output missing log path; got:\n%s", out)
	}
}

func TestShowTaskAutoRunCompleted(t *testing.T) {
	root, db := showListEditDB(t)
	insertTask(t, db, "auto-show-done", "Auto Show Done", "done", "medium", filepath.Join(root, "repo"), nil)
	_, err := db.Exec(
		`UPDATE tasks SET auto_run_status='completed', auto_run_finished='2026-06-10T12:00:00Z' WHERE slug='auto-show-done'`,
	)
	if err != nil {
		t.Fatalf("set auto_run fields: %v", err)
	}

	out := captureStdout(t, func() {
		cmdShow([]string{"task", "auto-show-done"})
	})

	if !strings.Contains(out, "auto_run:      completed") {
		t.Errorf("show output missing 'auto_run: completed'; got:\n%s", out)
	}
	if !strings.Contains(out, "finished") {
		t.Errorf("show output missing finished timestamp; got:\n%s", out)
	}
}
