# Task Dependencies & Hierarchy — Phase 1 (Model + CLI + Validation) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split flow's single overloaded task edge into two orthogonal relationships — non-blocking **hierarchy** (`tasks.parent_slug`) and blocking **dependency** (`task_dependencies`) — with cycle validation, creation-time capture, and a one-shot legacy migration.

**Architecture:** Decouple `tasks.parent_slug` from the start-blocker. `task_dependencies` becomes blocking-only (read by `TaskStartBlockerFor`); `parent_slug` becomes hierarchy-only (drives the tree, never blocks). The Go mutators are renamed cleanly (internal package, no external consumers); the CLI keeps `--parent` as a deprecated alias for `--depends-on`. `flow spawn --parent` is repointed at hierarchy, which fixes the spawn-blocks-its-own-child contradiction.

**Tech Stack:** Go, `modernc.org/sqlite` (pure Go, no CGO), `flag.FlagSet`, table-driven `testing`.

**Spec:** `docs/superpowers/specs/2026-06-03-task-dependencies-hierarchy-design.md`

---

## Conventions for the executor

- **Commits are gated.** This repo's owner commits only when they ask (their standing rule). Treat every `git commit` step below as **"stage the listed files, run the tests, then pause for the owner's go-ahead"** — do not push commits autonomously. Keep the commit messages ready.
- Build with `make build` or `go build -o flow .`; test with `go test ./...` or a focused `go test -run <Name> -v ./internal/<pkg>/`.
- No CGO, RFC3339 timestamps via `flowdb.NowISO()`, exit codes 0/1/2.
- Test helpers already exist: `openTempDB(t)`, `insertProject(t, db, …)`, `insertTask(t, db, slug, name, status, priority, wd, project)` in **both** `internal/flowdb/db_test.go` and `internal/app/testhelpers_test.go`.

## File map

- `internal/flowdb/db.go` — schema (`schema_meta`), rename mutators, add hierarchy setters, cycle helpers, decouple blocker, new migration.
- `internal/flowdb/db_test.go` — new tests for the above.
- `internal/app/add.go` — `--depends-on`, `--subtask-of` flags on `add task`.
- `internal/app/update.go` — `--depends-on`/`--remove-dep`/`--clear-deps`, `--subtask-of`/`--unparent`; `--parent`* aliases.
- `internal/app/spawn.go` — `--parent` → hierarchy; add `--depends-on`.
- `internal/app/show.go` — split display into hierarchy vs dependency sections.
- `internal/app/*_test.go` — CLI tests.

---

### Task 1: `schema_meta` guard table

**Files:**
- Modify: `internal/flowdb/db.go` (schemaDDL + two helpers)
- Test: `internal/flowdb/db_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSchemaMetaMarker(t *testing.T) {
	db := openTempDB(t)
	has, err := schemaMetaHas(db, "demo-marker")
	if err != nil {
		t.Fatalf("schemaMetaHas: %v", err)
	}
	if has {
		t.Fatal("marker should be absent on a fresh DB")
	}
	if err := schemaMetaSet(db, "demo-marker"); err != nil {
		t.Fatalf("schemaMetaSet: %v", err)
	}
	has, err = schemaMetaHas(db, "demo-marker")
	if err != nil {
		t.Fatalf("schemaMetaHas after set: %v", err)
	}
	if !has {
		t.Fatal("marker should be present after set")
	}
	// Idempotent: second set must not error (INSERT OR IGNORE).
	if err := schemaMetaSet(db, "demo-marker"); err != nil {
		t.Fatalf("schemaMetaSet second: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestSchemaMetaMarker -v ./internal/flowdb/`
Expected: FAIL — `undefined: schemaMetaHas` / `schemaMetaSet`.

- [ ] **Step 3: Add the table to `schemaDDL`**

In `internal/flowdb/db.go`, inside the `schemaDDL` const, after the `task_dependencies` table block, add:

```sql
CREATE TABLE IF NOT EXISTS schema_meta (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    applied_at  TEXT NOT NULL
);
```

- [ ] **Step 4: Add the helpers**

Add near the other small helpers in `db.go`:

```go
// schemaMetaHas reports whether a one-shot migration marker has been recorded.
// Used to gate data migrations that cannot be inferred from schema structure.
func schemaMetaHas(db *sql.DB, key string) (bool, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// schemaMetaSet records a one-shot migration marker. Idempotent.
func schemaMetaSet(db *sql.DB, key string) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO schema_meta (key, value, applied_at) VALUES (?, '1', ?)`,
		key, NowISO(),
	)
	return err
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -run TestSchemaMetaMarker -v ./internal/flowdb/`
Expected: PASS.

- [ ] **Step 6: Commit (gated — stage + pause)**

```bash
git add internal/flowdb/db.go internal/flowdb/db_test.go
git commit -m "feat(flowdb): add schema_meta one-shot migration guard table"
```

---

### Task 2: Decouple the start-blocker from `parent_slug`

**Files:**
- Modify: `internal/flowdb/db.go` — `TaskStartBlockerFor` (remove the legacy `parent_slug` fallback, currently ~lines 365-383)
- Test: `internal/flowdb/db_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestStartBlockerIgnoresHierarchyParent(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "epic", "Epic", "in-progress", "medium", wd, nil)
	insertTask(t, db, "sub", "Subtask", "backlog", "medium", wd, nil)
	// Hierarchy only: sub is a subtask of epic, NO dependency row.
	if _, err := db.Exec(`UPDATE tasks SET parent_slug = 'epic' WHERE slug = 'sub'`); err != nil {
		t.Fatalf("set parent_slug: %v", err)
	}
	sub, err := GetTask(db, "sub")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	blocker, err := TaskStartBlockerFor(db, sub)
	if err != nil {
		t.Fatalf("TaskStartBlockerFor: %v", err)
	}
	if blocker != nil {
		t.Fatalf("hierarchy parent must NOT block; got %v", blocker)
	}
}

func TestStartBlockerHonorsDependency(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "setup", "Setup", "in-progress", "medium", wd, nil)
	insertTask(t, db, "deploy", "Deploy", "backlog", "medium", wd, nil)
	now := NowISO()
	if _, err := db.Exec(
		`INSERT INTO task_dependencies (child_slug, parent_slug, created_at) VALUES ('deploy','setup',?)`, now,
	); err != nil {
		t.Fatalf("insert dep: %v", err)
	}
	deploy, err := GetTask(db, "deploy")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	blocker, err := TaskStartBlockerFor(db, deploy)
	if err != nil {
		t.Fatalf("TaskStartBlockerFor: %v", err)
	}
	if blocker == nil || blocker.Kind != "dependency" {
		t.Fatalf("dependency on non-done task must block; got %v", blocker)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test -run 'TestStartBlocker' -v ./internal/flowdb/`
Expected: `TestStartBlockerIgnoresHierarchyParent` FAILS (the current `parent_slug` fallback makes the hierarchy parent block).

- [ ] **Step 3: Remove the legacy `parent_slug` fallback**

In `TaskStartBlockerFor`, delete the entire fallback block that begins with the comment `// Fall back to the legacy parent_slug column…` and ends before `pendingParents := …`. Concretely, remove:

```go
	// Fall back to the legacy parent_slug column when the dependency table
	// has no row for this child (e.g. a code path inserted parent_slug
	// without calling AddTaskParent). The migration backfills existing
	// rows, so this should be rare in practice.
	if len(parents) == 0 && task.ParentSlug.Valid {
		if legacy := strings.TrimSpace(task.ParentSlug.String); legacy != "" {
			var p PendingParent
			p.Slug = legacy
			var del sql.NullString
			scanErr := db.QueryRow(
				`SELECT name, status, deleted_at FROM tasks WHERE slug = ?`,
				legacy,
			).Scan(&p.Name, &p.Status, &del)
			if errors.Is(scanErr, sql.ErrNoRows) {
				p.Missing = true
			} else if scanErr != nil {
				return nil, scanErr
			} else {
				p.Deleted = del.Valid
			}
			parents = []PendingParent{p}
		}
	}
```

Update the doc comment on `TaskStartBlockerFor` to state it reads dependencies from `task_dependencies` only (hierarchy via `parent_slug` is non-blocking).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestStartBlocker' -v ./internal/flowdb/`
Expected: both PASS.

- [ ] **Step 5: Run the full flowdb suite (catch fallback-dependent tests)**

Run: `go test ./internal/flowdb/`
Expected: PASS. If a pre-existing test relied on the `parent_slug` fallback to block, convert it to insert a `task_dependencies` row instead (that was the real dependency mechanism).

- [ ] **Step 6: Commit (gated — stage + pause)**

```bash
git add internal/flowdb/db.go internal/flowdb/db_test.go
git commit -m "feat(flowdb): start-blocker reads task_dependencies only; parent_slug no longer blocks"
```

---

### Task 3: Hierarchy setters + hierarchy cycle detection

**Files:**
- Modify: `internal/flowdb/db.go`
- Test: `internal/flowdb/db_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestSetTaskHierarchyParent(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "epic", "Epic", "backlog", "medium", wd, nil)
	insertTask(t, db, "sub", "Sub", "backlog", "medium", wd, nil)
	if err := SetTaskHierarchyParent(db, "sub", "epic"); err != nil {
		t.Fatalf("SetTaskHierarchyParent: %v", err)
	}
	got, err := GetTask(db, "sub")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !got.ParentSlug.Valid || got.ParentSlug.String != "epic" {
		t.Fatalf("parent_slug = %v, want epic", got.ParentSlug)
	}
	if err := ClearTaskHierarchyParent(db, "sub"); err != nil {
		t.Fatalf("ClearTaskHierarchyParent: %v", err)
	}
	got, _ = GetTask(db, "sub")
	if got.ParentSlug.Valid {
		t.Fatalf("parent_slug should be NULL after clear, got %v", got.ParentSlug)
	}
}

func TestSetTaskHierarchyParentRejectsCycle(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "a", "A", "backlog", "medium", wd, nil)
	insertTask(t, db, "b", "B", "backlog", "medium", wd, nil)
	insertTask(t, db, "c", "C", "backlog", "medium", wd, nil)
	mustNoErr(t, SetTaskHierarchyParent(db, "b", "a")) // b ⊂ a
	mustNoErr(t, SetTaskHierarchyParent(db, "c", "b")) // c ⊂ b
	// a ⊂ c would close the cycle a→c→b→a.
	if err := SetTaskHierarchyParent(db, "a", "c"); err == nil {
		t.Fatal("expected hierarchy cycle to be rejected")
	}
	// self-parent rejected too
	if err := SetTaskHierarchyParent(db, "a", "a"); err == nil {
		t.Fatal("expected self-parent to be rejected")
	}
}
```

Add this helper to `db_test.go` if not present:

```go
func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test -run 'TestSetTaskHierarchyParent' -v ./internal/flowdb/`
Expected: FAIL — `undefined: SetTaskHierarchyParent`.

- [ ] **Step 3: Implement the setters + cycle helper**

Add to `db.go`:

```go
// wouldCycleHierarchy reports whether making `child` a subtask of `parent`
// would create a cycle in the parent_slug chain (child already an ancestor of
// parent, or child == parent). Depth-bounded as a runaway guard.
func wouldCycleHierarchy(db *sql.DB, child, parent string) (bool, error) {
	if child == parent {
		return true, nil
	}
	cur := parent
	for i := 0; i < 1000 && cur != ""; i++ {
		if cur == child {
			return true, nil
		}
		var next sql.NullString
		err := db.QueryRow(`SELECT parent_slug FROM tasks WHERE slug = ?`, cur).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !next.Valid {
			return false, nil
		}
		cur = next.String
	}
	return false, nil
}

// SetTaskHierarchyParent sets childSlug's organizational parent (subtask-of).
// Hierarchy is non-blocking — it never gates task start. Validates existence,
// self-loop, and cycle-freedom.
func SetTaskHierarchyParent(db *sql.DB, childSlug, parentSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	parentSlug = strings.TrimSpace(parentSlug)
	if childSlug == "" || parentSlug == "" {
		return errors.New("child and parent slugs are required")
	}
	if childSlug == parentSlug {
		return errors.New("a task cannot be a subtask of itself")
	}
	var exists string
	if err := db.QueryRow(`SELECT slug FROM tasks WHERE slug = ?`, parentSlug).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("parent task %q not found", parentSlug)
		}
		return err
	}
	cyc, err := wouldCycleHierarchy(db, childSlug, parentSlug)
	if err != nil {
		return err
	}
	if cyc {
		return fmt.Errorf("making %q a subtask of %q would create a hierarchy cycle", childSlug, parentSlug)
	}
	_, err = db.Exec(`UPDATE tasks SET parent_slug = ?, updated_at = ? WHERE slug = ?`,
		parentSlug, NowISO(), childSlug)
	return err
}

// ClearTaskHierarchyParent removes childSlug's organizational parent.
func ClearTaskHierarchyParent(db *sql.DB, childSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	if childSlug == "" {
		return errors.New("child slug is required")
	}
	_, err := db.Exec(`UPDATE tasks SET parent_slug = NULL, updated_at = ? WHERE slug = ?`,
		NowISO(), childSlug)
	return err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestSetTaskHierarchyParent' -v ./internal/flowdb/`
Expected: PASS.

- [ ] **Step 5: Commit (gated — stage + pause)**

```bash
git add internal/flowdb/db.go internal/flowdb/db_test.go
git commit -m "feat(flowdb): add hierarchy setters with cycle detection"
```

---

### Task 4: Dependency mutators decoupled from `parent_slug` + dependency cycle detection

**Files:**
- Modify: `internal/flowdb/db.go` — rename `AddTaskParent`→`AddTaskDependency`, `RemoveTaskParent`→`RemoveTaskDependency`, `ClearTaskParents`→`ClearTaskDependencies`; drop `syncLegacyParentSlug` (delete it — it becomes dead code); add cycle helper. Update the two internal callers’ imports (done in Tasks 7-8).
- Test: `internal/flowdb/db_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestAddTaskDependencyDoesNotMirrorParentSlug(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "setup", "Setup", "backlog", "medium", wd, nil)
	insertTask(t, db, "deploy", "Deploy", "backlog", "medium", wd, nil)
	if err := AddTaskDependency(db, "deploy", "setup"); err != nil {
		t.Fatalf("AddTaskDependency: %v", err)
	}
	got, _ := GetTask(db, "deploy")
	if got.ParentSlug.Valid {
		t.Fatalf("dependency must NOT set parent_slug (hierarchy); got %v", got.ParentSlug)
	}
	parents, err := ListParentSlugs(db, "deploy")
	if err != nil {
		t.Fatalf("ListParentSlugs: %v", err)
	}
	if len(parents) != 1 || parents[0] != "setup" {
		t.Fatalf("dependency parents = %v, want [setup]", parents)
	}
}

func TestAddTaskDependencyRejectsCycle(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "a", "A", "backlog", "medium", wd, nil)
	insertTask(t, db, "b", "B", "backlog", "medium", wd, nil)
	insertTask(t, db, "c", "C", "backlog", "medium", wd, nil)
	mustNoErr(t, AddTaskDependency(db, "b", "a")) // b depends on a
	mustNoErr(t, AddTaskDependency(db, "c", "b")) // c depends on b
	// a depends on c would close a→c→b→a.
	if err := AddTaskDependency(db, "a", "c"); err == nil {
		t.Fatal("expected dependency cycle to be rejected")
	}
	if err := AddTaskDependency(db, "a", "a"); err == nil {
		t.Fatal("expected self-dependency to be rejected")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test -run 'TestAddTaskDependency' -v ./internal/flowdb/`
Expected: FAIL — `undefined: AddTaskDependency`.

- [ ] **Step 3: Rename + reimplement the dependency mutators**

Replace `AddTaskParent`, `RemoveTaskParent`, `ClearTaskParents`, and `syncLegacyParentSlug` in `db.go` with:

```go
// wouldCycleDependency reports whether adding the edge "child depends on parent"
// would create a cycle, i.e. `parent` can already reach `child` by following
// depends-on edges. Bounded as a runaway guard.
func wouldCycleDependency(db *sql.DB, child, parent string) (bool, error) {
	if child == parent {
		return true, nil
	}
	visited := make(map[string]bool)
	stack := []string{parent}
	for len(stack) > 0 && len(visited) < 100000 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == child {
			return true, nil
		}
		if visited[cur] {
			continue
		}
		visited[cur] = true
		rows, err := db.Query(`SELECT parent_slug FROM task_dependencies WHERE child_slug = ?`, cur)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return false, err
			}
			stack = append(stack, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false, err
		}
		rows.Close()
	}
	return false, nil
}

// AddTaskDependency declares childSlug as blocked by parentSlug. The child
// cannot start until the parent is done (enforced by TaskStartBlockerFor).
// Idempotent (INSERT OR IGNORE). Does NOT touch tasks.parent_slug (that column
// is hierarchy, a separate concept). Rejects self-loops and cycles.
func AddTaskDependency(db *sql.DB, childSlug, parentSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	parentSlug = strings.TrimSpace(parentSlug)
	if childSlug == "" || parentSlug == "" {
		return errors.New("child and parent slugs are required")
	}
	if childSlug == parentSlug {
		return errors.New("a task cannot depend on itself")
	}
	cyc, err := wouldCycleDependency(db, childSlug, parentSlug)
	if err != nil {
		return err
	}
	if cyc {
		return fmt.Errorf("adding dependency %q → %q would create a cycle", childSlug, parentSlug)
	}
	_, err = db.Exec(
		`INSERT OR IGNORE INTO task_dependencies (child_slug, parent_slug, created_at) VALUES (?, ?, ?)`,
		childSlug, parentSlug, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// RemoveTaskDependency drops the (child, parent) blocking edge if present.
func RemoveTaskDependency(db *sql.DB, childSlug, parentSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	parentSlug = strings.TrimSpace(parentSlug)
	if childSlug == "" || parentSlug == "" {
		return errors.New("child and parent slugs are required")
	}
	_, err := db.Exec(
		`DELETE FROM task_dependencies WHERE child_slug = ? AND parent_slug = ?`,
		childSlug, parentSlug,
	)
	return err
}

// ClearTaskDependencies removes every blocking dependency for the child.
func ClearTaskDependencies(db *sql.DB, childSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	if childSlug == "" {
		return errors.New("child slug is required")
	}
	_, err := db.Exec(`DELETE FROM task_dependencies WHERE child_slug = ?`, childSlug)
	return err
}
```

Delete `syncLegacyParentSlug` entirely (no remaining callers after this task + Tasks 7-8). `ListParentSlugs` and `loadParentsForBlocker` stay unchanged — they already read `task_dependencies`.

- [ ] **Step 4: Build to find broken callers**

Run: `go build ./...`
Expected: compile errors only in `internal/app/spawn.go` and `internal/app/update.go` (the old `AddTaskParent`/`RemoveTaskParent`/`ClearTaskParents` names). Leave those — Tasks 7 and 8 rewrite them. To keep the suite green in the meantime, temporarily update those call sites to the new names with a `// TODO(task7/8)` note, OR implement Tasks 7-8 before running the full build. (Recommended: do Tasks 7-8 next, then run the full build once.)

- [ ] **Step 5: Run the dependency tests**

Run: `go test -run 'TestAddTaskDependency' -v ./internal/flowdb/`
Expected: PASS.

- [ ] **Step 6: Commit (gated — stage + pause)**

```bash
git add internal/flowdb/db.go internal/flowdb/db_test.go
git commit -m "feat(flowdb): dependency mutators decoupled from parent_slug; add cycle detection"
```

---

### Task 5: One-shot migration — null legacy `parent_slug` mirrors

**Files:**
- Modify: `internal/flowdb/db.go` — add `migrateSplitHierarchyDependency`, call it in `runMigrations` **after** `migrateTaskDependencies`.
- Test: `internal/flowdb/db_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestMigrateSplitHierarchyDependency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flow.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	wd := t.TempDir()
	insertTask(t, db, "parent", "Parent", "done", "medium", wd, nil)
	insertTask(t, db, "child", "Child", "backlog", "medium", wd, nil)
	// Simulate a legacy mirror: a dependency row PLUS the parent_slug mirror
	// pointing at the same edge (what the old AddTaskParent produced).
	now := NowISO()
	if _, err := db.Exec(
		`INSERT INTO task_dependencies (child_slug, parent_slug, created_at) VALUES ('child','parent',?)`, now,
	); err != nil {
		t.Fatalf("seed dep: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET parent_slug = 'parent' WHERE slug = 'child'`); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	db.Close()

	// Reopen → migration runs.
	db, err = OpenDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	// The dependency edge is preserved (still blocking).
	parents, _ := ListParentSlugs(db, "child")
	if len(parents) != 1 || parents[0] != "parent" {
		t.Fatalf("dependency must survive migration; got %v", parents)
	}
	// The mirror is nulled (hierarchy starts clean).
	got, _ := GetTask(db, "child")
	if got.ParentSlug.Valid {
		t.Fatalf("legacy parent_slug mirror should be nulled; got %v", got.ParentSlug)
	}
	// Idempotent + non-destructive to NEW hierarchy: set a real hierarchy
	// parent that is NOT a dependency, reopen, and confirm it survives.
	mustNoErr(t, SetTaskHierarchyParent(db, "child", "parent")) // hierarchy only now
	db.Close()
	db2, err := OpenDB(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	defer db2.Close()
	got, _ = GetTask(db2, "child")
	if !got.ParentSlug.Valid || got.ParentSlug.String != "parent" {
		t.Fatalf("new hierarchy edge must survive re-open (marker should gate the migration); got %v", got.ParentSlug)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `go test -run TestMigrateSplitHierarchyDependency -v ./internal/flowdb/`
Expected: FAIL — mirror not nulled (no migration yet).

- [ ] **Step 3: Implement the migration**

Add to `db.go`:

```go
// migrateSplitHierarchyDependency runs once per DB. It nulls tasks.parent_slug
// values that merely mirror an existing task_dependencies edge — the artifact
// of the pre-split era when "parent" meant both hierarchy and blocking
// dependency. After this, parent_slug means hierarchy only and task_dependencies
// means blocking only. Gated by a schema_meta marker so a legitimately-set
// hierarchy edge that later happens to coincide with a dependency is never
// clobbered on a subsequent open.
func migrateSplitHierarchyDependency(db *sql.DB) error {
	done, err := schemaMetaHas(db, "hierarchy_dependency_split")
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	if _, err := db.Exec(`
		UPDATE tasks SET parent_slug = NULL
		WHERE parent_slug IS NOT NULL
		  AND EXISTS (
		      SELECT 1 FROM task_dependencies d
		      WHERE d.child_slug = tasks.slug AND d.parent_slug = tasks.parent_slug
		  )
	`); err != nil {
		return fmt.Errorf("null legacy parent_slug mirrors: %w", err)
	}
	return schemaMetaSet(db, "hierarchy_dependency_split")
}
```

In `runMigrations`, add the call immediately after the existing `migrateTaskDependencies` block:

```go
	if err := migrateTaskDependencies(db); err != nil {
		return fmt.Errorf("migrate task_dependencies: %w", err)
	}
	if err := migrateSplitHierarchyDependency(db); err != nil {
		return fmt.Errorf("migrate split hierarchy/dependency: %w", err)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestMigrateSplitHierarchyDependency -v ./internal/flowdb/`
Expected: PASS.

- [ ] **Step 5: Run the full flowdb suite**

Run: `go test ./internal/flowdb/`
Expected: PASS.

- [ ] **Step 6: Commit (gated — stage + pause)**

```bash
git add internal/flowdb/db.go internal/flowdb/db_test.go
git commit -m "feat(flowdb): one-shot migration to null legacy parent_slug dependency mirrors"
```

---

### Task 6: `flow add task` — `--depends-on` and `--subtask-of`

**Files:**
- Modify: `internal/app/add.go` — `addTask`
- Test: `internal/app/add_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestAddTaskWithDependsOnAndSubtaskOf(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", t.TempDir())
	db := openFlowDB(t)
	wd := t.TempDir()
	insertTask(t, db, "epic", "Epic", "backlog", "medium", wd, nil)
	insertTask(t, db, "setup", "Setup", "backlog", "medium", wd, nil)
	db.Close()

	rc := cmdAdd([]string{"task", "Build feature", "--agent", "claude",
		"--subtask-of", "epic", "--depends-on", "setup", "--work-dir", wd})
	if rc != 0 {
		t.Fatalf("cmdAdd rc = %d, want 0", rc)
	}

	db = openFlowDB(t)
	defer db.Close()
	created, err := resolveJustCreatedTaskSlug(db, "Build feature", "")
	if err != nil {
		t.Fatalf("locate created: %v", err)
	}
	task, _ := flowdb.GetTask(db, created)
	if !task.ParentSlug.Valid || task.ParentSlug.String != "epic" {
		t.Fatalf("subtask-of not set: %v", task.ParentSlug)
	}
	parents, _ := flowdb.ListParentSlugs(db, created)
	if len(parents) != 1 || parents[0] != "setup" {
		t.Fatalf("depends-on not set: %v", parents)
	}
}
```

(Confirm `openFlowDB` reopens the same `FLOW_ROOT` DB — see `internal/app/add_test.go:28`.)

- [ ] **Step 2: Run test to verify failure**

Run: `go test -run TestAddTaskWithDependsOnAndSubtaskOf -v ./internal/app/`
Expected: FAIL — `flag provided but not defined: -subtask-of`.

- [ ] **Step 3: Add the flags + wiring to `addTask`**

In `internal/app/add.go`, in `addTask`, register the flags alongside the others:

```go
	var dependsOn stringSliceFlag
	fs.Var(&dependsOn, "depends-on", "slug of a task this one is blocked by (repeatable)")
	subtaskOf := fs.String("subtask-of", "", "slug of the parent task in the hierarchy (organizational, non-blocking)")
```

After the task INSERT succeeds (after the `db.Exec(INSERT INTO tasks …)` block, before the success print), add:

```go
	if s := strings.TrimSpace(*subtaskOf); s != "" {
		if err := flowdb.SetTaskHierarchyParent(db, slug, s); err != nil {
			fmt.Fprintf(os.Stderr, "error: --subtask-of: %v\n", err)
			return 1
		}
	}
	for _, dep := range dependsOn {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		if err := flowdb.AddTaskDependency(db, slug, dep); err != nil {
			fmt.Fprintf(os.Stderr, "error: --depends-on %q: %v\n", dep, err)
			return 1
		}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestAddTaskWithDependsOnAndSubtaskOf -v ./internal/app/`
Expected: PASS.

- [ ] **Step 5: Commit (gated — stage + pause)**

```bash
git add internal/app/add.go internal/app/add_test.go
git commit -m "feat(add): capture --depends-on and --subtask-of at task creation"
```

---

### Task 7: `flow update task` — dependency + hierarchy flags (with `--parent` alias)

**Files:**
- Modify: `internal/app/update.go` — `cmdUpdateTask`
- Test: `internal/app/update_test.go` (create if absent)

- [ ] **Step 1: Write the failing tests**

```go
func TestUpdateTaskDependsOnAndSubtaskOf(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", t.TempDir())
	db := openFlowDB(t)
	wd := t.TempDir()
	insertTask(t, db, "epic", "Epic", "backlog", "medium", wd, nil)
	insertTask(t, db, "setup", "Setup", "backlog", "medium", wd, nil)
	insertTask(t, db, "feat", "Feat", "backlog", "medium", wd, nil)
	db.Close()

	if rc := cmdUpdateTask([]string{"feat", "--subtask-of", "epic", "--depends-on", "setup"}); rc != 0 {
		t.Fatalf("update rc = %d", rc)
	}
	db = openFlowDB(t)
	defer db.Close()
	task, _ := flowdb.GetTask(db, "feat")
	if task.ParentSlug.String != "epic" {
		t.Fatalf("subtask-of: %v", task.ParentSlug)
	}
	parents, _ := flowdb.ListParentSlugs(db, "feat")
	if len(parents) != 1 || parents[0] != "setup" {
		t.Fatalf("depends-on: %v", parents)
	}
	// --unparent clears hierarchy; --clear-deps clears dependencies.
	if rc := cmdUpdateTask([]string{"feat", "--unparent", "--clear-deps"}); rc != 0 {
		t.Fatalf("clear rc = %d", rc)
	}
	db2 := openFlowDB(t)
	defer db2.Close()
	task, _ = flowdb.GetTask(db2, "feat")
	if task.ParentSlug.Valid {
		t.Fatalf("hierarchy not cleared: %v", task.ParentSlug)
	}
	parents, _ = flowdb.ListParentSlugs(db2, "feat")
	if len(parents) != 0 {
		t.Fatalf("deps not cleared: %v", parents)
	}
}

func TestUpdateTaskParentAliasIsDependency(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", t.TempDir())
	db := openFlowDB(t)
	wd := t.TempDir()
	insertTask(t, db, "setup", "Setup", "backlog", "medium", wd, nil)
	insertTask(t, db, "feat", "Feat", "backlog", "medium", wd, nil)
	db.Close()
	// Legacy --parent must still create a *dependency*, NOT hierarchy.
	if rc := cmdUpdateTask([]string{"feat", "--parent", "setup"}); rc != 0 {
		t.Fatalf("update rc = %d", rc)
	}
	db = openFlowDB(t)
	defer db.Close()
	task, _ := flowdb.GetTask(db, "feat")
	if task.ParentSlug.Valid {
		t.Fatalf("--parent must not set hierarchy; got %v", task.ParentSlug)
	}
	parents, _ := flowdb.ListParentSlugs(db, "feat")
	if len(parents) != 1 || parents[0] != "setup" {
		t.Fatalf("--parent should add dependency; got %v", parents)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test -run 'TestUpdateTask(DependsOn|ParentAlias)' -v ./internal/app/`
Expected: FAIL — `flag provided but not defined: -depends-on`.

- [ ] **Step 3: Add flags, alias the legacy `--parent`, repoint to new funcs**

In `cmdUpdateTask`, register the new flags next to the existing parent flags:

```go
	var dependsOn stringSliceFlag
	fs.Var(&dependsOn, "depends-on", "add a blocking dependency slug (repeatable)")
	var removeDeps stringSliceFlag
	fs.Var(&removeDeps, "remove-dep", "remove a blocking dependency slug (repeatable)")
	clearDeps := fs.Bool("clear-deps", false, "clear ALL blocking dependencies")
	subtaskOf := fs.String("subtask-of", "", "set the hierarchy parent (organizational, non-blocking)")
	unparent := fs.Bool("unparent", false, "clear the hierarchy parent")
```

Fold the legacy parent flags into the dependency lists right after parsing (so `--parent` behaves exactly as `--depends-on`), and emit a one-time deprecation hint:

```go
	if len(addParents) > 0 || len(removeParents) > 0 || *clearParent {
		fmt.Fprintln(os.Stderr, "note: --parent/--remove-parent/--clear-parent are deprecated aliases for --depends-on/--remove-dep/--clear-deps (blocking dependencies)")
	}
	dependsOn = append(dependsOn, addParents...)
	removeDeps = append(removeDeps, removeParents...)
	clearDepsEff := *clearDeps || *clearParent
```

Extend `anyField` to include the new flags:

```go
	anyField := /* …existing… */ ||
		len(dependsOn) > 0 || len(removeDeps) > 0 || *clearDeps ||
		*subtaskOf != "" || *unparent
```

Add mutual-exclusion guards mirroring the existing ones:

```go
	if clearDepsEff && (len(dependsOn) > 0 || len(removeDeps) > 0) {
		fmt.Fprintln(os.Stderr, "error: clearing all dependencies is mutually exclusive with --depends-on/--remove-dep")
		return 2
	}
	if *unparent && *subtaskOf != "" {
		fmt.Fprintln(os.Stderr, "error: --subtask-of and --unparent are mutually exclusive")
		return 2
	}
```

Replace the existing `addParents`/`removeParents`/`clearParent` execution blocks (the `for _, p := range addParents` … `if *clearParent` region) with dependency + hierarchy execution:

```go
	for _, p := range dependsOn {
		parentTask, err := ResolveTask(db, p, true)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fmt.Fprintf(os.Stderr, "error: dependency task %q not found\n", p)
				return 1
			}
			fmt.Fprintf(os.Stderr, "error: resolve dependency %q: %v\n", p, err)
			return 1
		}
		if err := flowdb.AddTaskDependency(db, task.Slug, parentTask.Slug); err != nil {
			fmt.Fprintf(os.Stderr, "error: add dependency %q: %v\n", parentTask.Slug, err)
			return 1
		}
		fmt.Printf("depends-on + %s\n", parentTask.Slug)
	}
	for _, p := range removeDeps {
		parentTask, err := ResolveTask(db, p, true)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				fmt.Fprintf(os.Stderr, "error: resolve dependency %q: %v\n", p, err)
				return 1
			}
			parentTask = &flowdb.Task{Slug: p}
		}
		if err := flowdb.RemoveTaskDependency(db, task.Slug, parentTask.Slug); err != nil {
			fmt.Fprintf(os.Stderr, "error: remove dependency %q: %v\n", parentTask.Slug, err)
			return 1
		}
		fmt.Printf("depends-on - %s\n", parentTask.Slug)
	}
	if clearDepsEff {
		if err := flowdb.ClearTaskDependencies(db, task.Slug); err != nil {
			fmt.Fprintf(os.Stderr, "error: clear dependencies: %v\n", err)
			return 1
		}
		fmt.Println("dependencies cleared")
	}
	if *subtaskOf != "" {
		parentTask, err := ResolveTask(db, *subtaskOf, true)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fmt.Fprintf(os.Stderr, "error: hierarchy parent %q not found\n", *subtaskOf)
				return 1
			}
			fmt.Fprintf(os.Stderr, "error: resolve hierarchy parent %q: %v\n", *subtaskOf, err)
			return 1
		}
		if err := flowdb.SetTaskHierarchyParent(db, task.Slug, parentTask.Slug); err != nil {
			fmt.Fprintf(os.Stderr, "error: --subtask-of: %v\n", err)
			return 1
		}
		fmt.Printf("subtask-of → %s\n", parentTask.Slug)
	}
	if *unparent {
		if err := flowdb.ClearTaskHierarchyParent(db, task.Slug); err != nil {
			fmt.Fprintf(os.Stderr, "error: --unparent: %v\n", err)
			return 1
		}
		fmt.Println("hierarchy parent cleared")
	}
```

Update the `updated_at` bump condition (formerly keyed on `addParents/removeParents/clearParent`) to fire on any of the new dependency ops too:

```go
	if len(dependsOn) > 0 || len(removeDeps) > 0 || clearDepsEff {
		if _, err := db.Exec(`UPDATE tasks SET updated_at=? WHERE slug=?`, now, task.Slug); err != nil {
			fmt.Fprintf(os.Stderr, "error: bump updated_at: %v\n", err)
			return 1
		}
	}
```

(The `SetTaskHierarchyParent`/`ClearTaskHierarchyParent` helpers already bump `updated_at` themselves.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestUpdateTask(DependsOn|ParentAlias)' -v ./internal/app/`
Expected: PASS.

- [ ] **Step 5: Commit (gated — stage + pause)**

```bash
git add internal/app/update.go internal/app/update_test.go
git commit -m "feat(update): split dependency vs hierarchy flags; --parent becomes a dependency alias"
```

---

### Task 8: `flow spawn` — `--parent` becomes hierarchy; add `--depends-on`

**Files:**
- Modify: `internal/app/spawn.go` — `cmdSpawn`
- Test: `internal/app/spawn_test.go` (create if absent)

- [ ] **Step 1: Write the failing test (the contradiction regression)**

```go
func TestSpawnParentIsHierarchyNotBlocking(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", t.TempDir())
	// Stub the terminal spawn so cmdDo doesn't try to open iTerm.
	origRunner := iterm.Runner
	iterm.Runner = func(script string) (string, error) { return "", nil }
	t.Cleanup(func() { iterm.Runner = origRunner })

	db := openFlowDB(t)
	wd := t.TempDir()
	// Parent is IN-PROGRESS (the normal orchestration case).
	insertTask(t, db, "epic", "Epic", "in-progress", "medium", wd, nil)
	// epic must carry a session_id to satisfy the in-progress invariant.
	if _, err := db.Exec(`UPDATE tasks SET session_id='11111111-1111-4111-8111-111111111111', session_started=? WHERE slug='epic'`, flowdb.NowISO()); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	db.Close()

	rc := cmdSpawn([]string{"child work", "--parent", "epic", "--agent", "claude", "--no-open", "--work-dir", wd})
	if rc != 0 {
		t.Fatalf("spawn rc = %d, want 0", rc)
	}
	db = openFlowDB(t)
	defer db.Close()
	child, err := resolveJustCreatedTaskSlug(db, "child work", "")
	if err != nil {
		t.Fatalf("locate child: %v", err)
	}
	task, _ := flowdb.GetTask(db, child)
	// Hierarchy parent set…
	if task.ParentSlug.String != "epic" {
		t.Fatalf("spawn --parent should set hierarchy; got %v", task.ParentSlug)
	}
	// …and NO blocking dependency, so the child can start while epic runs.
	if blocker, _ := flowdb.TaskStartBlockerFor(db, task); blocker != nil {
		t.Fatalf("spawned child must not be blocked by an in-progress hierarchy parent; got %v", blocker)
	}
}
```

(Confirm the package import path for the iTerm mock — `internal/iterm` exposes `iterm.Runner`; see CLAUDE.md.)

- [ ] **Step 2: Run test to verify failure**

Run: `go test -run TestSpawnParentIsHierarchyNotBlocking -v ./internal/app/`
Expected: FAIL — child is blocked (current `AddTaskParent` made it a dependency).

- [ ] **Step 3: Repoint `--parent` to hierarchy; add `--depends-on`**

In `cmdSpawn`, register a new flag:

```go
	var dependsOn stringSliceFlag
	fs.Var(&dependsOn, "depends-on", "slug of a task this spawn is blocked by (repeatable)")
```

Replace the parent-linkage block (currently the `if parentSlug != "" { AddTaskParent(...) ... }` region) with:

```go
	if parentSlug != "" {
		if err := flowdb.SetTaskHierarchyParent(db, createdSlug, parentSlug); err != nil {
			fmt.Fprintf(os.Stderr, "warning: set hierarchy parent: %v\n", err)
		}
	}
	for _, dep := range dependsOn {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		if err := flowdb.AddTaskDependency(db, createdSlug, dep); err != nil {
			fmt.Fprintf(os.Stderr, "warning: add dependency %q: %v\n", dep, err)
		}
	}
```

(`SetTaskHierarchyParent` bumps `updated_at`; the explicit `UPDATE tasks SET updated_at` that followed the old `AddTaskParent` can be removed.) Update the flag help for `--parent` to read: `"slug of the parent task in the hierarchy (this spawn becomes its subtask; non-blocking)"`.

- [ ] **Step 4: Run test + full app build**

Run: `go test -run TestSpawnParentIsHierarchyNotBlocking -v ./internal/app/`
Then: `go build ./...`
Expected: test PASS; build clean (all old `AddTaskParent`/etc. references now gone).

- [ ] **Step 5: Commit (gated — stage + pause)**

```bash
git add internal/app/spawn.go internal/app/spawn_test.go
git commit -m "fix(spawn): --parent sets non-blocking hierarchy (fixes spawn-blocks-own-child); add --depends-on"
```

---

### Task 9: `flow show task` — separate hierarchy vs dependency sections

**Files:**
- Modify: `internal/app/show.go` — `printTaskMetadata`, add `loadTaskDependencyParents` + `loadTaskDependents`
- Test: `internal/app/show_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestShowTaskSeparatesHierarchyAndDependencies(t *testing.T) {
	root, db := showListEditDB(t) // existing helper: returns (root, *sql.DB)
	wd := t.TempDir()
	insertTask(t, db, "epic", "Epic", "backlog", "medium", wd, nil)
	insertTask(t, db, "setup", "Setup", "done", "medium", wd, nil)
	insertTask(t, db, "feat", "Feat", "backlog", "medium", wd, nil)
	mustNoErrApp(t, flowdb.SetTaskHierarchyParent(db, "feat", "epic"))
	mustNoErrApp(t, flowdb.AddTaskDependency(db, "feat", "setup"))

	out := captureStdout(t, func() {
		feat, _ := flowdb.GetTask(db, "feat")
		printTaskMetadata(db, feat, root)
	})
	if !strings.Contains(out, "subtask of:") || !strings.Contains(out, "epic") {
		t.Fatalf("expected hierarchy 'subtask of: epic' in output:\n%s", out)
	}
	if !strings.Contains(out, "depends on:") || !strings.Contains(out, "setup") {
		t.Fatalf("expected 'depends on: setup' in output:\n%s", out)
	}
}
```

If `captureStdout` / `mustNoErrApp` don't exist in the app test package, add minimal versions to `internal/app/testhelpers_test.go`:

```go
func mustNoErrApp(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `go test -run TestShowTaskSeparatesHierarchyAndDependencies -v ./internal/app/`
Expected: FAIL — output uses old `parent:`/`children:` labels and omits `depends on:`.

- [ ] **Step 3: Add dependency loaders + rework the display block**

Add to `show.go`:

```go
// loadTaskDependencyParents returns the blocking dependencies of a task
// (the tasks it depends on), with status for at-a-glance blocked detection.
func loadTaskDependencyParents(db *sql.DB, slug string) ([]taskRelationSummary, error) {
	return queryRelationSummaries(db, `
		SELECT t.slug, t.name, t.status
		FROM task_dependencies d JOIN tasks t ON t.slug = d.parent_slug
		WHERE d.child_slug = ? AND t.deleted_at IS NULL
		ORDER BY d.created_at ASC, t.slug ASC`, slug)
}

// loadTaskDependents returns the tasks blocked by this task.
func loadTaskDependents(db *sql.DB, slug string) ([]taskRelationSummary, error) {
	return queryRelationSummaries(db, `
		SELECT t.slug, t.name, t.status
		FROM task_dependencies d JOIN tasks t ON t.slug = d.child_slug
		WHERE d.parent_slug = ? AND t.deleted_at IS NULL
		ORDER BY d.created_at ASC, t.slug ASC`, slug)
}

func queryRelationSummaries(db *sql.DB, query, arg string) ([]taskRelationSummary, error) {
	rows, err := db.Query(query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []taskRelationSummary
	for rows.Next() {
		var s taskRelationSummary
		if err := rows.Scan(&s.Slug, &s.Name, &s.Status); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
```

In `printTaskMetadata`, replace the existing `parent:` block and the `children:` block with hierarchy-labelled output, then add the dependency output below it:

```go
	// Hierarchy (organizational, non-blocking).
	if t.ParentSlug.Valid && t.ParentSlug.String != "" {
		label := t.ParentSlug.String
		if parent, err := loadTaskRelationSummary(db, t.ParentSlug.String); err == nil {
			label = fmt.Sprintf("%s (%s) %s", parent.Slug, parent.Status, parent.Name)
		} else if err != sql.ErrNoRows {
			fmt.Fprintf(os.Stderr, "warning: load hierarchy parent: %v\n", err)
		}
		fmt.Printf("subtask of:    %s\n", label)
	}
	if subs, err := loadTaskChildren(db, t.Slug); err != nil {
		fmt.Fprintf(os.Stderr, "warning: load subtasks: %v\n", err)
	} else if len(subs) > 0 {
		fmt.Println("subtasks:")
		for _, s := range subs {
			fmt.Printf("  - %s (%s) %s\n", s.Slug, s.Status, s.Name)
		}
	}

	// Dependencies (blocking).
	if deps, err := loadTaskDependencyParents(db, t.Slug); err != nil {
		fmt.Fprintf(os.Stderr, "warning: load dependencies: %v\n", err)
	} else if len(deps) > 0 {
		fmt.Println("depends on:")
		for _, d := range deps {
			fmt.Printf("  - %s (%s) %s\n", d.Slug, d.Status, d.Name)
		}
	}
	if blocks, err := loadTaskDependents(db, t.Slug); err != nil {
		fmt.Fprintf(os.Stderr, "warning: load dependents: %v\n", err)
	} else if len(blocks) > 0 {
		fmt.Println("blocks:")
		for _, b := range blocks {
			fmt.Printf("  - %s (%s) %s\n", b.Slug, b.Status, b.Name)
		}
	}
```

Add `"bytes"`, `"io"` imports to the test helper file if `captureStdout` needs them.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestShowTaskSeparatesHierarchyAndDependencies -v ./internal/app/`
Expected: PASS.

- [ ] **Step 5: Full suite + build**

Run: `go test ./...` then `make build`
Expected: all PASS, binary builds.

- [ ] **Step 6: Commit (gated — stage + pause)**

```bash
git add internal/app/show.go internal/app/show_test.go internal/app/testhelpers_test.go
git commit -m "feat(show): display hierarchy (subtask of / subtasks) separately from dependencies (depends on / blocks)"
```

---

## E2E coverage (append to `internal/app/e2e_test.go`)

- [ ] Add a step that creates `epic`, `setup` (claude tasks), then
  `flow add task "feat" --agent claude --subtask-of epic --depends-on setup`,
  and asserts: `flow do feat` is **blocked** while `setup` is not done, but the
  hierarchy parent `epic` (regardless of its status) does **not** block. Mark
  `setup` done and assert `feat` becomes startable. Keep the iTerm runner mocked
  per the existing e2e pattern.

---

## Self-review (completed by author)

- **Spec coverage:** model split (Tasks 2-4), decoupling (Task 2,4), cycle detection both edges (Tasks 3,4), migration preserving prior behavior (Task 5), creation-time capture (Task 6), edit-for-existing CLI (Task 7), spawn fix (Task 8), show display (Task 9), `--parent` deprecated alias (Task 7). Skill intake + UI are **Phase 2/3**, out of scope here.
- **Placeholders:** none — every code step carries full code.
- **Type consistency:** `AddTaskDependency`/`RemoveTaskDependency`/`ClearTaskDependencies`/`SetTaskHierarchyParent`/`ClearTaskHierarchyParent`/`ListParentSlugs`/`TaskStartBlockerFor` names used identically across tasks. `taskRelationSummary` reused from existing `show.go`.
- **Known follow-ups for Phase 3:** the server `TaskView` builder and UI tree currently read `parent_slug` as the spine and derive "Depends on/blocks" from it — after this phase those must be re-sourced (hierarchy spine from `parent_slug`; dependency edges from `task_dependencies`). Tracked in the spec's UI section; **not** in this plan.
```