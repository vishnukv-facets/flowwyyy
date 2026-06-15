package app

import (
	"context"
	"errors"
	"flow/internal/flowdb"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubClaudeRunner replaces claudeRunner with a capturing stub that returns
// the supplied error. Returns a *call counter and a *captured-args record so
// tests can assert how the runner was invoked.
type capturedClaudeCall struct {
	slug   string
	prompt string
}

func stubClaudeRunner(t *testing.T, retErr error) *[]capturedClaudeCall {
	t.Helper()
	old := claudeRunner
	calls := &[]capturedClaudeCall{}
	claudeRunner = func(slug, prompt string) error {
		*calls = append(*calls, capturedClaudeCall{slug: slug, prompt: prompt})
		return retErr
	}
	t.Cleanup(func() { claudeRunner = old })
	return calls
}

func stubTaskTmuxSessionCloser(t *testing.T, retErr error, events *[]string) *[]string {
	t.Helper()
	old := taskTmuxSessionCloser
	calls := &[]string{}
	taskTmuxSessionCloser = func(name string) error {
		*calls = append(*calls, name)
		if events != nil {
			*events = append(*events, "tmux:"+name)
		}
		return retErr
	}
	t.Cleanup(func() { taskTmuxSessionCloser = old })
	return calls
}

func TestCmdDoneHappyPath(t *testing.T) {
	setupFlowRoot(t)
	stubClaudeRunner(t, nil)
	stubTaskTmuxSessionCloser(t, nil, nil)
	if rc := cmdAdd([]string{"task", "Some Task", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	// Session-id invariant: done requires a session_id. Pre-seed one
	// so the close-out is legal.
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug='some-task'`,
		fakeSessionID("some-task"), flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if rc := cmdDone([]string{"some-task"}); rc != 0 {
		t.Fatalf("done rc=%d", rc)
	}
	db = openFlowDB(t)
	task, err := flowdb.GetTask(db, "some-task")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" {
		t.Errorf("status: got %q, want done", task.Status)
	}
}

func TestCmdDoneClosesTmuxSessionAfterCloseoutSweep(t *testing.T) {
	setupFlowRoot(t)
	events := []string{}
	oldClaude := claudeRunner
	claudeRunner = func(slug, prompt string) error {
		events = append(events, "sweep:"+slug)
		return nil
	}
	t.Cleanup(func() { claudeRunner = oldClaude })
	tmuxCalls := stubTaskTmuxSessionCloser(t, nil, &events)

	if rc := cmdAdd([]string{"task", "Reap Session", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	db := openFlowDB(t)
	sessionID := fakeSessionID("reap-session")
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug='reap-session'`,
		sessionID, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if rc := cmdDone([]string{"reap-session"}); rc != 0 {
		t.Fatalf("done rc=%d", rc)
	}
	wantEvents := []string{"sweep:reap-session", "tmux:flow-reap-session"}
	if len(events) != len(wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	for i := range wantEvents {
		if events[i] != wantEvents[i] {
			t.Fatalf("events = %v, want %v", events, wantEvents)
		}
	}
	if len(*tmuxCalls) != 1 || (*tmuxCalls)[0] != "flow-reap-session" {
		t.Fatalf("tmux calls = %v, want [flow-reap-session]", *tmuxCalls)
	}

	db = openFlowDB(t)
	task, err := flowdb.GetTask(db, "reap-session")
	if err != nil {
		t.Fatal(err)
	}
	if !task.SessionID.Valid || task.SessionID.String != sessionID {
		t.Fatalf("session_id = %+v, want preserved %s", task.SessionID, sessionID)
	}
}

func TestCmdDoneClosesTmuxAfterSweepFailure(t *testing.T) {
	setupFlowRoot(t)
	events := []string{}
	oldClaude := claudeRunner
	claudeRunner = func(slug, prompt string) error {
		events = append(events, "sweep:"+slug)
		return errors.New("sweep failed")
	}
	t.Cleanup(func() { claudeRunner = oldClaude })
	stubTaskTmuxSessionCloser(t, nil, &events)

	if rc := cmdAdd([]string{"task", "Sweep Then Reap", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug='sweep-then-reap'`,
		fakeSessionID("sweep-then-reap"), flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if rc := cmdDone([]string{"sweep-then-reap"}); rc != 0 {
		t.Fatalf("done rc=%d, want 0 even when sweep fails", rc)
	}
	wantEvents := []string{"sweep:sweep-then-reap", "tmux:flow-sweep-then-reap"}
	if len(events) != len(wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	for i := range wantEvents {
		if events[i] != wantEvents[i] {
			t.Fatalf("events = %v, want %v", events, wantEvents)
		}
	}
}

func TestCmdDoneLinksCurrentBranchPR(t *testing.T) {
	setupFlowRoot(t)
	stubClaudeRunner(t, nil)
	stubTaskTmuxSessionCloser(t, nil, nil)
	workDir := t.TempDir()
	if rc := cmdAdd([]string{"task", "Review Task", "--work-dir", workDir, "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug='review-task'`,
		fakeSessionID("review-task"), flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	old := openPRURLForBranch
	openPRURLForBranch = func(ctx context.Context, dir string) (string, error) {
		if dir != workDir {
			t.Fatalf("gh dir = %q, want %q", dir, workDir)
		}
		return "https://github.com/acme/app/pull/12", nil
	}
	t.Cleanup(func() { openPRURLForBranch = old })

	if rc := cmdDone([]string{"review-task"}); rc != 0 {
		t.Fatalf("done rc=%d, want 0", rc)
	}
	db = openFlowDB(t)
	tags, err := flowdb.GetTaskTags(db, "review-task")
	if err != nil {
		t.Fatalf("GetTaskTags() error = %v", err)
	}
	found := false
	for _, tag := range tags {
		if tag == "gh-pr:acme/app#12" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("tags = %v, want gh-pr:acme/app#12", tags)
	}
}

func TestCloseTaskTmuxSessionNoopsWhenNoLiveSession(t *testing.T) {
	old := taskTmuxCommandRunner
	defer func() { taskTmuxCommandRunner = old }()

	var commands [][]string
	taskTmuxCommandRunner = func(args ...string) ([]byte, error) {
		commands = append(commands, append([]string(nil), args...))
		return []byte("can't find session"), errors.New("missing session")
	}

	if err := closeTaskTmuxSession("flow-missing-session"); err != nil {
		t.Fatalf("closeTaskTmuxSession returned error for missing session: %v", err)
	}
	got := appCommandLog(commands)
	if !contains(got, "has-session -t flow-missing-session") {
		t.Fatalf("commands = %s, want has-session probe", got)
	}
	if contains(got, "run-shell") || contains(got, "kill-session") {
		t.Fatalf("missing session should not schedule kill; commands = %s", got)
	}
}

func TestCloseTaskTmuxSessionSchedulesDeferredKill(t *testing.T) {
	old := taskTmuxCommandRunner
	defer func() { taskTmuxCommandRunner = old }()

	var commands [][]string
	taskTmuxCommandRunner = func(args ...string) ([]byte, error) {
		commands = append(commands, append([]string(nil), args...))
		return nil, nil
	}

	if err := closeTaskTmuxSession("flow-self-session"); err != nil {
		t.Fatalf("closeTaskTmuxSession returned error: %v", err)
	}
	got := appCommandLog(commands)
	for _, want := range []string{
		"has-session -t flow-self-session",
		"run-shell -b",
		"sleep",
		"tmux kill-session -t 'flow-self-session'",
	} {
		if !contains(got, want) {
			t.Fatalf("commands missing %q:\n%s", want, got)
		}
	}
}

// TestCmdDoneLinksWorktreeBranchPR guards the worktree-blindness bug: a task
// with a dedicated worktree must have its PR looked up against the WORKTREE
// checkout (which is on the task branch), not work_dir (the repo root, usually
// on main with no PR).
func TestCmdDoneLinksWorktreeBranchPR(t *testing.T) {
	setupFlowRoot(t)
	stubClaudeRunner(t, nil)
	workDir := t.TempDir()
	worktree := t.TempDir()
	if rc := cmdAdd([]string{"task", "WT Task", "--work-dir", workDir, "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=?, worktree_path=? WHERE slug='wt-task'`,
		fakeSessionID("wt-task"), flowdb.NowISO(), worktree,
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	old := openPRURLForBranch
	openPRURLForBranch = func(ctx context.Context, dir string) (string, error) {
		if dir != worktree {
			t.Fatalf("gh dir = %q, want worktree %q (not work_dir)", dir, worktree)
		}
		return "https://github.com/acme/app/pull/42", nil
	}
	t.Cleanup(func() { openPRURLForBranch = old })

	if rc := cmdDone([]string{"wt-task"}); rc != 0 {
		t.Fatalf("done rc=%d, want 0", rc)
	}
	db = openFlowDB(t)
	tags, err := flowdb.GetTaskTags(db, "wt-task")
	if err != nil {
		t.Fatalf("GetTaskTags() error = %v", err)
	}
	found := false
	for _, tag := range tags {
		if tag == "gh-pr:acme/app#42" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tags = %v, want gh-pr:acme/app#42", tags)
	}
}

func TestCmdDoneUnknownRef(t *testing.T) {
	setupFlowRoot(t)
	stubClaudeRunner(t, nil)
	stubTaskTmuxSessionCloser(t, nil, nil)
	if rc := cmdDone([]string{"nope"}); rc == 0 {
		t.Error("expected rc!=0 for unknown task")
	}
}

func TestCmdDoneIdempotent(t *testing.T) {
	setupFlowRoot(t)
	stubClaudeRunner(t, nil)
	stubTaskTmuxSessionCloser(t, nil, nil)
	if rc := cmdAdd([]string{"task", "Idem", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	// Pre-seed a session_id so done is legal under the invariant.
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug='idem'`,
		fakeSessionID("idem"), flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if rc := cmdDone([]string{"idem"}); rc != 0 {
		t.Fatalf("first done rc=%d", rc)
	}
	// A second done should be idempotent (status already done, session_id
	// preserved → UPDATE is a no-op semantically).
	if rc := cmdDone([]string{"idem"}); rc != 0 {
		t.Errorf("second done rc=%d, want 0 (idempotent)", rc)
	}
}

func TestCmdDoneNoArgs(t *testing.T) {
	if rc := cmdDone(nil); rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
}

// TestCmdDoneRefusesTaskWithoutSession pins the session-id invariant
// at the friendly-error layer for `flow done`: a backlog task with
// no session_id has no transcript to sweep, so done refuses with a
// pointer to `flow archive`. Replaces the older
// "skips sweep when no session" test — that path is no longer
// reachable under the invariant.
func TestCmdDoneRefusesTaskWithoutSession(t *testing.T) {
	setupFlowRoot(t)
	calls := stubClaudeRunner(t, errors.New("should not be called"))
	tmuxCalls := stubTaskTmuxSessionCloser(t, errors.New("should not be called"), nil)
	if rc := cmdAdd([]string{"task", "No Session Task", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	if rc := cmdDone([]string{"no-session-task"}); rc != 1 {
		t.Errorf("done rc=%d, want 1 (sessionless task should be refused)", rc)
	}
	if len(*calls) != 0 {
		t.Errorf("expected 0 sweep calls, got %d", len(*calls))
	}
	if len(*tmuxCalls) != 0 {
		t.Errorf("expected 0 tmux close calls, got %d", len(*tmuxCalls))
	}
	db := openFlowDB(t)
	task, _ := flowdb.GetTask(db, "no-session-task")
	if task.Status != "backlog" {
		t.Errorf("status = %q, want backlog (refused done should not flip)", task.Status)
	}
}

func TestCmdDoneCapturesPendingCodexSessionBeforeSweep(t *testing.T) {
	setupFlowRoot(t)
	calls := stubClaudeRunner(t, nil)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()

	if rc := cmdAdd([]string{"task", "Codex Close", "--work-dir", workDir, "--agent", "codex"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	started := time.Now().Add(-1 * time.Minute).UTC()
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=NULL, session_started=? WHERE slug='codex-close'`,
		started.Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	sessionID := "55555555-5555-4555-8555-555555555555"
	sessionDir := filepath.Join(codexHome, "sessions", "2026", "05", "15")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"` + workDir + `","timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"flow task codex-close"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, sessionID+".jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := cmdDone([]string{"codex-close"}); rc != 0 {
		t.Fatalf("done rc=%d, want 0", rc)
	}
	db = openFlowDB(t)
	task, err := flowdb.GetTask(db, "codex-close")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" {
		t.Fatalf("status = %q, want done", task.Status)
	}
	if !task.SessionID.Valid || task.SessionID.String != sessionID {
		t.Fatalf("session_id = %+v, want %s", task.SessionID, sessionID)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 sweep call, got %d", len(*calls))
	}
	if !contains((*calls)[0].prompt, "flow transcript codex-close") {
		t.Fatalf("sweep prompt should read codex transcript, got:\n%s", (*calls)[0].prompt)
	}
}

func TestCmdDoneRefusesPendingCodexSessionWithoutCapture(t *testing.T) {
	setupFlowRoot(t)
	calls := stubClaudeRunner(t, errors.New("should not be called"))
	workDir := t.TempDir()
	if rc := cmdAdd([]string{"task", "Codex Missing Capture", "--work-dir", workDir, "--agent", "codex"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=NULL, session_started=? WHERE slug='codex-missing-capture'`,
		time.Now().Add(-1*time.Minute).UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if rc := cmdDone([]string{"codex-missing-capture"}); rc != 1 {
		t.Fatalf("done rc=%d, want 1", rc)
	}
	if len(*calls) != 0 {
		t.Fatalf("expected no sweep calls, got %d", len(*calls))
	}
	db = openFlowDB(t)
	task, err := flowdb.GetTask(db, "codex-missing-capture")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", task.Status)
	}
}

// TestCmdDoneRunsSweepWhenSessionExists verifies that done invokes the
// claude runner exactly once with the task slug and a sweep prompt
// when the task has a session_id, and returns rc=0 on success.
func TestCmdDoneRunsSweepWhenSessionExists(t *testing.T) {
	setupFlowRoot(t)
	calls := stubClaudeRunner(t, nil)
	if rc := cmdAdd([]string{"task", "Has Session", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	// Manually populate session_id so the sweep gate fires.
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug=?`,
		"deadbeef-uuid", flowdb.NowISO(), "has-session",
	); err != nil {
		t.Fatalf("seed session_id: %v", err)
	}
	db.Close()

	if rc := cmdDone([]string{"has-session"}); rc != 0 {
		t.Fatalf("done rc=%d, want 0", rc)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 sweep call, got %d", len(*calls))
	}
	got := (*calls)[0]
	if got.slug != "has-session" {
		t.Errorf("call slug = %q, want has-session", got.slug)
	}
	if got.prompt == "" {
		t.Error("call prompt is empty")
	}
	// Sanity-check the prompt mentions key behavior so a regression in
	// buildCloseoutSweepPrompt that drops the skill load or the
	// transcript step gets caught here.
	for _, want := range []string{"flow skill", "flow transcript has-session", "kb/"} {
		if !contains(got.prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// TestCmdDoneCloseoutSweepIncludesProjectStep verifies that when the
// task is attached to a project, the close-out prompt includes the
// project-update step pointing at the project's updates/ directory.
func TestCmdDoneCloseoutSweepIncludesProjectStep(t *testing.T) {
	setupFlowRoot(t)
	calls := stubClaudeRunner(t, nil)

	wd := t.TempDir()
	if rc := cmdAdd([]string{"project", "Some Proj", "--slug", "sp", "--work-dir", wd}); rc != 0 {
		t.Fatalf("add project rc=%d", rc)
	}
	if rc := cmdAdd([]string{"task", "Has Proj", "--project", "sp", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add task rc=%d", rc)
	}

	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug=?`,
		"hp-uuid", flowdb.NowISO(), "has-proj",
	); err != nil {
		t.Fatalf("seed session_id: %v", err)
	}
	db.Close()

	if rc := cmdDone([]string{"has-proj"}); rc != 0 {
		t.Fatalf("done rc=%d", rc)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 sweep call, got %d", len(*calls))
	}
	got := (*calls)[0].prompt
	for _, want := range []string{
		"Project update",
		"\"sp\"",
		// Path is templatized off flowRoot() so the test environment's
		// tempdir prefix appears here. Match the suffix only.
		"/projects/sp/updates/",
	} {
		if !contains(got, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// TestCmdDoneCloseoutSweepSkipsProjectStepForFloating verifies that
// floating tasks (no project) get a prompt without any project-update
// instructions or path references.
func TestCmdDoneCloseoutSweepSkipsProjectStepForFloating(t *testing.T) {
	setupFlowRoot(t)
	calls := stubClaudeRunner(t, nil)

	if rc := cmdAdd([]string{"task", "Floating", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add task rc=%d", rc)
	}
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug=?`,
		"f-uuid", flowdb.NowISO(), "floating",
	); err != nil {
		t.Fatalf("seed session_id: %v", err)
	}
	db.Close()

	if rc := cmdDone([]string{"floating"}); rc != 0 {
		t.Fatalf("done rc=%d", rc)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 sweep call, got %d", len(*calls))
	}
	got := (*calls)[0].prompt
	for _, unwanted := range []string{
		"Project update",
		"/projects/",
	} {
		if contains(got, unwanted) {
			t.Errorf("floating-task prompt unexpectedly contains %q", unwanted)
		}
	}
}

// TestCloseoutSweepPromptUpgradesOutdatedKB verifies the close-out sweep is told
// to UPGRADE existing KB entries this work made stale (a captured plan now
// executed), not only append — so the always-loaded KB stays current.
func TestCloseoutSweepPromptUpgradesOutdatedKB(t *testing.T) {
	setupFlowRoot(t)
	p := buildCloseoutSweepPrompt("some-slug", "")
	for _, want := range []string{"supersede", "outdated", "in place"} {
		if !contains(p, want) {
			t.Errorf("close-out prompt missing KB-upgrade cue %q", want)
		}
	}
}

// TestCmdDoneSweepFailureStillSucceeds verifies that a non-zero exit
// from the sweep runner does NOT fail the done command — the status
// flip is the durability boundary, the sweep is best-effort.
func TestCmdDoneSweepFailureStillSucceeds(t *testing.T) {
	setupFlowRoot(t)
	stubClaudeRunner(t, errors.New("exec: claude: executable file not found in $PATH"))
	if rc := cmdAdd([]string{"task", "Sweep Fail", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add rc=%d", rc)
	}
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, session_started=? WHERE slug=?`,
		"sf-uuid", flowdb.NowISO(), "sweep-fail",
	); err != nil {
		t.Fatalf("seed session_id: %v", err)
	}
	db.Close()

	if rc := cmdDone([]string{"sweep-fail"}); rc != 0 {
		t.Errorf("done rc=%d, want 0 even when sweep fails", rc)
	}
	// Status must still be flipped despite the sweep failure.
	db = openFlowDB(t)
	task, err := flowdb.GetTask(db, "sweep-fail")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" {
		t.Errorf("status = %q, want done", task.Status)
	}
}

// contains is a tiny strings.Contains shim so done_test.go doesn't need
// a strings import just for this.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func appCommandLog(commands [][]string) string {
	out := ""
	for _, cmd := range commands {
		for i, arg := range cmd {
			if i > 0 {
				out += " "
			}
			out += arg
		}
		out += "\n"
	}
	return out
}
