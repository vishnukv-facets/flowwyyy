package app

import (
	"flow/internal/flowdb"
	"flow/internal/iterm"
	"flow/internal/spawner"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestE2EFullRoundtrip exercises the full command surface in the order a
// user would hit it for a realistic session: init, add project, add task
// under the project, do (bootstrap + spawn), show both, list both, waiting
// set/clear, priority change, update file drop, done, archive, unarchive,
// workdir registry.
//
// Mocks claudeRunner and iterm.Runner so nothing actually spawns
// claude or osascript. Uses a temp FLOW_ROOT so the user's real ~/.flow is
// untouched.
func TestE2EFullRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	flowRoot := filepath.Join(tmp, "flow")
	t.Setenv("FLOW_ROOT", flowRoot)
	t.Setenv("HOME", tmp)
	t.Setenv("CODEX_HOME", filepath.Join(tmp, ".codex"))

	// Fake repo that serves as the project's work_dir.
	repo := filepath.Join(tmp, "code", "budgeting-app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pin the spawner backend so a kitty/zellij/Terminal.app host does
	// not route SpawnTab to a real terminal CLI. Without this, running
	// the test inside kitty (KITTY_WINDOW_ID set) opens a real tab and
	// types the fixture command into the user's shell.
	oldOverride := spawner.Override
	spawner.Override = spawner.BackendITerm
	t.Cleanup(func() { spawner.Override = oldOverride })

	// Stub osascript for the whole test.
	oldOsa := iterm.Runner
	iterm.Runner = func(args []string) error { return nil }
	t.Cleanup(func() { iterm.Runner = oldOsa })

	// Stub the headless claude runner so cmdDone doesn't try to invoke
	// the real claude CLI for its post-flip KB sweep.
	oldClaude := claudeRunner
	claudeRunner = func(slug, prompt string) error { return nil }
	t.Cleanup(func() { claudeRunner = oldClaude })

	// Pin the UUID `flow do` allocates so downstream assertions can
	// reference a known session_id. In production, newUUID produces a
	// random v4 UUID that is also written to tasks.session_id before
	// the iTerm tab spawns and passed to `claude --session-id`.
	const fixedSID = "e2e-session-uuid"
	oldNewUUID := newUUID
	newUUID = func() (string, error) { return fixedSID, nil }
	t.Cleanup(func() { newUUID = oldNewUUID })

	step := func(name string, rc int) {
		t.Helper()
		if rc != 0 {
			t.Fatalf("%s: rc=%d", name, rc)
		}
	}

	// 1. init — creates tree, db, installs skill
	step("init", cmdInit(nil))
	if _, err := os.Stat(filepath.Join(flowRoot, "flow.db")); err != nil {
		t.Fatalf("flow.db not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(flowRoot, "projects")); err != nil {
		t.Fatalf("projects dir not created: %v", err)
	}

	// 2. add project
	step("add project", cmdAdd([]string{"project", "Budgeting App Revamp", "--work-dir", repo}))
	if _, err := os.Stat(filepath.Join(flowRoot, "projects", "budgeting-app-revamp", "brief.md")); err != nil {
		t.Fatalf("project brief.md not created: %v", err)
	}

	// 3. add task under the project
	step("add task", cmdAdd([]string{"task", "Fix Auth Token Expiry",
		"--project", "budgeting-app-revamp", "--agent", "claude"}))
	taskDir := filepath.Join(flowRoot, "tasks", "fix-auth-token-expiry")
	if _, err := os.Stat(filepath.Join(taskDir, "brief.md")); err != nil {
		t.Fatalf("task brief.md not created: %v", err)
	}

	// 4. add a floating task (auto workspace)
	step("add floating task", cmdAdd([]string{"task", "Scratch Investigation", "--agent", "claude"}))
	scratchDir := filepath.Join(flowRoot, "tasks", "scratch-investigation", "workspace")
	if _, err := os.Stat(scratchDir); err != nil {
		t.Fatalf("floating task workspace not created: %v", err)
	}

	// 5. do — pre-allocates the session UUID and spawns the tab. The
	// session_id lands in the DB synchronously; no self-registration
	// step is needed.
	step("do", cmdDo([]string{"fix-auth-token-expiry"}))
	db, err := flowdb.OpenDB(filepath.Join(flowRoot, "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := flowdb.GetTask(db, "fix-auth-token-expiry")
	if err != nil {
		t.Fatal(err)
	}
	if !task.SessionID.Valid || task.SessionID.String != fixedSID {
		t.Errorf("session_id after fresh spawn: got %+v, want %s", task.SessionID, fixedSID)
	}
	if task.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", task.Status)
	}

	// 5b. Write real jsonl content at the path claude would have used
	// given our pre-allocated session_id, so transcript can parse it.
	{
		encoded := EncodeCwdForClaude(task.WorkDir)
		sessionDir := filepath.Join(tmp, ".claude", "projects", encoded)
		if err := os.MkdirAll(sessionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sessionFile := filepath.Join(sessionDir, fixedSID+".jsonl")
		content := `{"type":"user","message":{"role":"user","content":"Hello"},"uuid":"u1","timestamp":"2026-04-12T10:00:00Z","sessionId":"` + fixedSID + `"}` + "\n" +
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hi there!"}]},"uuid":"a1","timestamp":"2026-04-12T10:00:01Z","sessionId":"` + fixedSID + `"}` + "\n"
		if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
			t.Fatalf("write session jsonl: %v", err)
		}
	}

	// 5c. transcript — should succeed now that session exists.
	step("transcript", cmdTranscript([]string{"fix-auth-token-expiry"}))

	// 6. do again — now session_id is populated, should spawn --resume.
	step("do resume", cmdDo([]string{"fix-auth-token-expiry"}))
	task, _ = flowdb.GetTask(db, "fix-auth-token-expiry")
	if task.SessionID.String != fixedSID {
		t.Errorf("session_id should be preserved across resume: got %q", task.SessionID.String)
	}
	if !task.SessionLastResumed.Valid {
		t.Error("session_last_resumed should be set after resume")
	}

	// 7. show task
	step("show task", cmdShow([]string{"task", "fix-auth-token-expiry"}))

	// 8. show project
	step("show project", cmdShow([]string{"project", "budgeting-app-revamp"}))

	// 9. list tasks — should include both
	step("list tasks", cmdList([]string{"tasks"}))

	// 10. list tasks filtered by project
	step("list tasks --project", cmdList([]string{"tasks", "--project", "budgeting-app-revamp"}))

	// 11. list projects
	step("list projects", cmdList([]string{"projects"}))

	// 12. waiting (via flow update task)
	step("waiting set", cmdUpdate([]string{"task", "fix-auth-token-expiry", "--waiting", "Alice review"}))
	task, _ = flowdb.GetTask(db, "fix-auth-token-expiry")
	if !task.WaitingOn.Valid || task.WaitingOn.String != "Alice review" {
		t.Errorf("waiting_on = %v, want Alice review", task.WaitingOn)
	}

	step("waiting clear", cmdUpdate([]string{"task", "fix-auth-token-expiry", "--clear-waiting"}))
	task, _ = flowdb.GetTask(db, "fix-auth-token-expiry")
	if task.WaitingOn.Valid {
		t.Errorf("waiting_on should be cleared, got %v", task.WaitingOn)
	}

	// 13. priority (via flow update task)
	step("priority", cmdUpdate([]string{"task", "fix-auth-token-expiry", "--priority", "high"}))
	task, _ = flowdb.GetTask(db, "fix-auth-token-expiry")
	if task.Priority != "high" {
		t.Errorf("priority = %q, want high", task.Priority)
	}

	// 14. drop an update file (skill-written, we simulate with os.WriteFile)
	updatePath := filepath.Join(taskDir, "updates", "2026-04-11-first-milestone.md")
	if err := os.WriteFile(updatePath, []byte("# First milestone\n\nFinished the token refresh endpoint.\n"), 0o644); err != nil {
		t.Fatalf("write update: %v", err)
	}

	// 15. show task again — should list the update file
	// (we can't easily capture stdout here, but we can verify the command returns 0
	// and the file is on disk)
	step("show task with update", cmdShow([]string{"task", "fix-auth-token-expiry"}))

	// 16. done
	step("done", cmdDone([]string{"fix-auth-token-expiry"}))
	task, _ = flowdb.GetTask(db, "fix-auth-token-expiry")
	if task.Status != "done" {
		t.Errorf("status after done = %q, want done", task.Status)
	}
	// session_id should still be present (flow done is DB-only)
	if task.SessionID.String != "e2e-session-uuid" {
		t.Errorf("session_id cleared by done: %v", task.SessionID)
	}

	// 17. archive
	step("archive", cmdArchive([]string{"fix-auth-token-expiry"}))
	task, _ = flowdb.GetTask(db, "fix-auth-token-expiry")
	if !task.ArchivedAt.Valid {
		t.Errorf("archived_at not set after archive")
	}

	// 18. list tasks (archived should be hidden)
	step("list tasks post-archive", cmdList([]string{"tasks"}))
	tasks, err := flowdb.ListTasks(db, flowdb.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.Slug == "fix-auth-token-expiry" && !task.ArchivedAt.Valid {
			t.Errorf("archived task leaked into default list")
		}
	}

	// 19. unarchive
	step("unarchive", cmdUnarchive([]string{"fix-auth-token-expiry"}))
	task, _ = flowdb.GetTask(db, "fix-auth-token-expiry")
	if task.ArchivedAt.Valid {
		t.Errorf("archived_at not cleared after unarchive: %v", task.ArchivedAt)
	}

	// 20. workdir list — the project's work_dir should have been auto-registered
	step("workdir list", cmdWorkdir([]string{"list"}))
	wd, err := flowdb.GetWorkdir(db, repo)
	if err != nil {
		t.Fatalf("repo not auto-registered as workdir: %v", err)
	}
	if wd == nil {
		t.Fatal("GetWorkdir returned nil for auto-registered path")
	}
}

// TestE2EDependencyVsHierarchySplit proves that the dependency/hierarchy split
// works end-to-end through real CLI commands.
//
// Scenario:
//   - epic   — standalone task (hierarchy parent, non-blocking)
//   - setup  — standalone task (blocking dependency)
//   - feat   — subtask-of epic (hierarchy) AND depends-on setup (blocking)
//
// Expected behaviour:
//   - While setup is not done, feat is blocked; the blocker Kind is
//     "dependency" and the pending parent is setup, not epic.
//   - After marking setup done, feat is no longer blocked.
//   - epic's status never contributes to the blocker regardless.
func TestE2EDependencyVsHierarchySplit(t *testing.T) {
	// --- environment isolation (same pattern as TestE2EFullRoundtrip) ---
	tmp := t.TempDir()
	flowRoot := filepath.Join(tmp, "flow")
	t.Setenv("FLOW_ROOT", flowRoot)
	t.Setenv("HOME", tmp)
	t.Setenv("CODEX_HOME", filepath.Join(tmp, ".codex"))

	// Pin the spawner backend so a kitty/zellij/Terminal.app host doesn't
	// open a real tab during the test.
	oldOverride := spawner.Override
	spawner.Override = spawner.BackendITerm
	t.Cleanup(func() { spawner.Override = oldOverride })

	oldOsa := iterm.Runner
	iterm.Runner = func(args []string) error { return nil }
	t.Cleanup(func() { iterm.Runner = oldOsa })

	oldClaude := claudeRunner
	claudeRunner = func(slug, prompt string) error { return nil }
	t.Cleanup(func() { claudeRunner = oldClaude })

	step := func(name string, rc int) {
		t.Helper()
		if rc != 0 {
			t.Fatalf("%s: rc=%d", name, rc)
		}
	}

	// 1. init
	step("init", cmdInit(nil))

	// Shared work-dir for all tasks (they are floating — no project needed).
	wd := filepath.Join(tmp, "code", "shared")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}

	// 2. Create epic and setup as plain claude tasks.
	step("add epic", cmdAdd([]string{"task", "Epic", "--agent", "claude", "--work-dir", wd}))
	step("add setup", cmdAdd([]string{"task", "Setup", "--agent", "claude", "--work-dir", wd}))

	// 3. Create feat: subtask of epic (hierarchy) + depends on setup (blocking).
	step("add feat", cmdAdd([]string{
		"task", "Feat",
		"--agent", "claude",
		"--work-dir", wd,
		"--subtask-of", "epic",
		"--depends-on", "setup",
	}))

	// Open the DB to inspect state directly (Direct assertion approach).
	dbPath := filepath.Join(flowRoot, "flow.db")
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 4. Assert that feat has the correct hierarchy parent (epic) and
	//    the correct dependency (setup).
	feat, err := flowdb.GetTask(db, "feat")
	if err != nil {
		t.Fatalf("GetTask(feat): %v", err)
	}
	if !feat.ParentSlug.Valid || feat.ParentSlug.String != "epic" {
		t.Errorf("hierarchy parent: got %+v, want epic", feat.ParentSlug)
	}
	parents, err := flowdb.ListParentSlugs(db, "feat")
	if err != nil {
		t.Fatalf("ListParentSlugs(feat): %v", err)
	}
	if len(parents) != 1 || parents[0] != "setup" {
		t.Errorf("dependency parents: got %v, want [setup]", parents)
	}

	// 5. While setup is not done, feat must be blocked by a DEPENDENCY blocker
	//    whose pending parent is setup — NOT epic.
	blocker, err := flowdb.TaskStartBlockerFor(db, feat)
	if err != nil {
		t.Fatalf("TaskStartBlockerFor(feat) before setup done: %v", err)
	}
	if blocker == nil {
		t.Fatal("expected a blocker while setup is not done, got nil")
	}
	if blocker.Kind != "dependency" {
		t.Errorf("blocker.Kind = %q, want dependency", blocker.Kind)
	}
	if len(blocker.Parents) != 1 || blocker.Parents[0].Slug != "setup" {
		t.Errorf("blocker.Parents = %+v, want [{Slug:setup}]", blocker.Parents)
	}
	// Confirm epic is not among the pending parents.
	for _, p := range blocker.Parents {
		if p.Slug == "epic" {
			t.Errorf("epic should not appear in dependency blocker parents, but it did: %+v", blocker.Parents)
		}
	}

	// 6. Explicitly confirm epic is not blocking feat even when epic is
	//    in-progress (requires a session_id for the DB invariant).
	now := time.Now().UTC().Format(time.RFC3339)
	const epicSessionID = "e2e-epic-session-uuid"
	if _, err := db.Exec(
		`UPDATE tasks SET status='in-progress', session_id=?, updated_at=? WHERE slug='epic'`,
		epicSessionID, now,
	); err != nil {
		t.Fatalf("set epic in-progress: %v", err)
	}
	feat, _ = flowdb.GetTask(db, "feat")
	blockerAfterEpicInProgress, err := flowdb.TaskStartBlockerFor(db, feat)
	if err != nil {
		t.Fatalf("TaskStartBlockerFor after epic in-progress: %v", err)
	}
	// Must still be blocked — but ONLY by setup.
	if blockerAfterEpicInProgress == nil {
		t.Fatal("feat must still be blocked after epic goes in-progress")
	}
	for _, p := range blockerAfterEpicInProgress.Parents {
		if p.Slug == "epic" {
			t.Errorf("epic (in-progress) should not block feat: %+v", blockerAfterEpicInProgress.Parents)
		}
	}

	// 7. Mark setup done directly via SQL (done rows may have NULL session_id
	//    per the schema CHECK: status IN ('backlog','done') OR session_id IS NOT NULL).
	if _, err := db.Exec(
		`UPDATE tasks SET status='done', status_changed_at=?, updated_at=? WHERE slug='setup'`,
		now, now,
	); err != nil {
		t.Fatalf("mark setup done: %v", err)
	}

	// 8. After setup is done, feat should be startable (no blocker).
	feat, err = flowdb.GetTask(db, "feat")
	if err != nil {
		t.Fatalf("GetTask(feat) after setup done: %v", err)
	}
	blockerAfterSetupDone, err := flowdb.TaskStartBlockerFor(db, feat)
	if err != nil {
		t.Fatalf("TaskStartBlockerFor after setup done: %v", err)
	}
	if blockerAfterSetupDone != nil {
		t.Errorf("feat should be startable after setup is done, but got blocker: %v", blockerAfterSetupDone)
	}

	// 9. Confirm epic (still in-progress) still does not block feat.
	epic, err := flowdb.GetTask(db, "epic")
	if err != nil {
		t.Fatalf("GetTask(epic): %v", err)
	}
	if epic.Status != "in-progress" {
		t.Errorf("epic.Status = %q, want in-progress (test setup invariant)", epic.Status)
	}
	// Blocker is nil (verified above) even though epic is in-progress — the
	// hierarchy parent is never a blocker.
}
