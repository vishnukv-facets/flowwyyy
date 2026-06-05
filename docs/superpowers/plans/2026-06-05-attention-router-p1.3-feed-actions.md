# Attention Router — P1.3 Feed Actions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

> ⚠️ **Repo rule:** work on branch `flow/attention-router-p1.1`; per-task commits on this branch are pre-approved. Never commit to `main`.

**Goal:** Build the executors that act on an Attention feed row — `make_task` (spawn a flow task from the pre-assembled context pack), `forward` (hand a summarized block to a matched task), and `dismiss` — plus an autonomy-gated `ApplyAction` router so the same executors are safe whether the operator drives them or (later) an autonomous caller does.

**Architecture:** Pure-Go executors in `internal/steering/actions.go`. Task creation and forwarding shell out to the `flow` CLI through mockable package-level function vars (`taskSpawner` → `flow spawn`, `taskTeller` → `flow tell`), the same seam `monitor.spawnFlowTask`/`tagFlowTask` use. Each executor records its outcome on the feed row via `flowdb.SetFeedItemStatus`. `ApplyAction` checks `AutonomyPolicy.Allow(action, item.Confidence)` for autonomous (`manual=false`) calls and bypasses it for operator-initiated (`manual=true`) calls. No CLI/UI invocation surface in this slice — that's P1.3b (a `flow attention` command) and P1.4 (the feed panel); both call these executors.

**Tech Stack:** Go, `database/sql`, `os/exec` (mocked), table-driven tests with `flowdb.OpenDB(t.TempDir()+"/flow.db")` and feed rows seeded via `flowdb.UpsertFeedItem`.

**Spec:** `docs/superpowers/specs/2026-06-04-attention-router-steerer-design.md` §8 (autonomy engine), §8.2 (context packs), §8.3 (forward-as-handoff).

**Builds on (merged on this branch):** `flowdb.FeedItem`, `flowdb.UpsertFeedItem`, `flowdb.ListFeedItems`, `flowdb.SetFeedItemStatus` (P1.1); `steering.Action`/`ActionMakeTask`/`ActionForward`/`ActionDrop`, `AutonomyPolicy`/`Allow`/`DefaultAutonomy` (P1.1).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/steering/actions.go` (create) | `taskSpawner`/`taskTeller` mockable shell-outs; `MakeTaskFromFeed`, `ForwardFeed`, `DismissFeed`; the context-pack brief + forward-message + slug builders; `ApplyAction` router + `ErrAutonomyDenied`. |
| `internal/steering/actions_test.go` (create) | Executor tests (make-task, forward, forward-requires-match, dismiss) + router/gate tests (manual bypass, autonomous denied, autonomous allowed, unsupported action). |

---

## Task 1: Feed action executors

**Files:**
- Create: `internal/steering/actions.go`
- Test: `internal/steering/actions_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/steering/actions_test.go
package steering

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func actionsDB(t *testing.T) *flowdb.DB { return nil } // placeholder; delete (see note)

func openActionsDB(t *testing.T) (db interface{ Close() error }) { return nil } // placeholder; delete

// stubActionIO swaps the shell-out vars and records calls.
type spawnRec struct{ name, slug, brief, project string }
type tellRec struct{ slug, msg string }

func stubActionIO(t *testing.T) (*[]spawnRec, *[]tellRec) {
	t.Helper()
	var spawns []spawnRec
	var tells []tellRec
	oldSpawn, oldTell := taskSpawner, taskTeller
	taskSpawner = func(_ context.Context, name, slug, brief, project string) error {
		spawns = append(spawns, spawnRec{name, slug, brief, project})
		return nil
	}
	taskTeller = func(_ context.Context, slug, msg string) error {
		tells = append(tells, tellRec{slug, msg})
		return nil
	}
	t.Cleanup(func() { taskSpawner, taskTeller = oldSpawn, oldTell })
	return &spawns, &tells
}

func seedFeed(t *testing.T, db interface {
	Exec(string, ...any) (interface{ RowsAffected() (int64, error) }, error)
}) {
}

func TestMakeTaskFromFeed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "f1", Source: "slack", ThreadKey: "C1:100.1", Summary: "Customer wants rollout date",
		SuggestedAction: "make_task", SuggestedProject: "goniyo", Reason: "names operator",
		Draft: "Targeting Friday.", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	if err := MakeTaskFromFeed(context.Background(), db, item); err != nil {
		t.Fatalf("MakeTaskFromFeed: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("taskSpawner calls = %d, want 1", len(*spawns))
	}
	got := (*spawns)[0]
	if got.project != "goniyo" {
		t.Errorf("project = %q, want goniyo", got.project)
	}
	if !strings.Contains(got.brief, "Customer wants rollout date") || !strings.Contains(got.brief, "C1:100.1") {
		t.Errorf("brief should embed summary + thread key:\n%s", got.brief)
	}
	if !strings.HasPrefix(got.slug, "att-") {
		t.Errorf("slug = %q, want att- prefix", got.slug)
	}
	// feed row marked acted
	if items, _ := flowdb.ListFeedItems(db, "acted"); len(items) != 1 {
		t.Errorf("feed item should be 'acted', got %d acted rows", len(items))
	}
}

func TestForwardFeed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	_, tells := stubActionIO(t)

	item := flowdb.FeedItem{ID: "f2", Source: "slack", ThreadKey: "C1:200.1", Summary: "rel q", MatchedTask: "kong-split", SuggestedAction: "forward", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ForwardFeed(context.Background(), db, item); err != nil {
		t.Fatalf("ForwardFeed: %v", err)
	}
	if len(*tells) != 1 || (*tells)[0].slug != "kong-split" {
		t.Fatalf("taskTeller = %+v, want one call to kong-split", *tells)
	}
	if !strings.Contains((*tells)[0].msg, "C1:200.1") {
		t.Errorf("forward message should reference the source thread: %q", (*tells)[0].msg)
	}
	if items, _ := flowdb.ListFeedItems(db, "acted"); len(items) != 1 {
		t.Errorf("forwarded item should be 'acted'")
	}
}

func TestForwardRequiresMatchedTask(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)
	item := flowdb.FeedItem{ID: "f3", Source: "slack", ThreadKey: "C1:300.1", SuggestedAction: "forward", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ForwardFeed(context.Background(), db, item); err == nil {
		t.Error("forward without matched_task must error")
	}
}

func TestDismissFeed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	item := flowdb.FeedItem{ID: "f4", Source: "slack", ThreadKey: "C1:400.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := DismissFeed(db, "f4"); err != nil {
		t.Fatalf("DismissFeed: %v", err)
	}
	if items, _ := flowdb.ListFeedItems(db, "dismissed"); len(items) != 1 {
		t.Errorf("item should be dismissed")
	}
}
```

> **Executor note:** delete the two placeholder lines `func actionsDB...` and `func openActionsDB...` and the empty `seedFeed` stub — they are leftover scaffolding markers. The real tests open the DB via `flowdb.OpenDB(...)` directly (shown in each test) and seed via `flowdb.UpsertFeedItem`. The final test file must contain only `stubActionIO`, the two record types, and the four `Test...` functions.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/steering/ -run 'TestMakeTaskFromFeed|TestForwardFeed|TestForwardRequiresMatchedTask|TestDismissFeed' -v`
Expected: build failure — `undefined: taskSpawner`, `taskTeller`, `MakeTaskFromFeed`, `ForwardFeed`, `DismissFeed`.

- [ ] **Step 3: Write the implementation** — `internal/steering/actions.go`

```go
package steering

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"flow/internal/flowdb"
)

// taskSpawner shells out to `flow spawn` to create a task from a feed item
// (mirrors the monitor.spawnFlowTask seam). Mockable in tests.
var taskSpawner = func(ctx context.Context, name, slug, brief, project string) error {
	args := []string{"spawn", name, "--slug", slug, "--priority", "high", "--prompt", brief, "--no-open", "--agent", "claude"}
	if p := strings.TrimSpace(project); p != "" {
		args = append(args, "--project", p)
	}
	cmd := exec.CommandContext(ctx, "flow", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("steering: flow spawn: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// taskTeller shells out to `flow tell` to forward a context block into an
// existing task's inbox. Mockable in tests.
var taskTeller = func(ctx context.Context, slug, message string) error {
	cmd := exec.CommandContext(ctx, "flow", "tell", slug, message)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("steering: flow tell %s: %w (output: %s)", slug, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// MakeTaskFromFeed spawns a flow task from a feed item's pre-assembled context
// pack and marks the feed row 'acted'.
func MakeTaskFromFeed(ctx context.Context, db *sql.DB, item flowdb.FeedItem) error {
	if err := taskSpawner(ctx, feedTaskName(item), feedTaskSlug(item), feedTaskBrief(item), item.SuggestedProject); err != nil {
		return err
	}
	return markActed(db, item.ID)
}

// ForwardFeed hands a summarized context block to the matched task's inbox via
// `flow tell` and marks the feed row 'acted'. Requires item.MatchedTask.
func ForwardFeed(ctx context.Context, db *sql.DB, item flowdb.FeedItem) error {
	target := strings.TrimSpace(item.MatchedTask)
	if target == "" {
		return fmt.Errorf("steering: forward requires a matched_task on feed item %q", item.ID)
	}
	if err := taskTeller(ctx, target, feedForwardMessage(item)); err != nil {
		return err
	}
	return markActed(db, item.ID)
}

// DismissFeed marks a feed row 'dismissed' (no external effect).
func DismissFeed(db *sql.DB, id string) error {
	return flowdb.SetFeedItemStatus(db, id, "dismissed", nowRFC3339())
}

func markActed(db *sql.DB, id string) error {
	return flowdb.SetFeedItemStatus(db, id, "acted", nowRFC3339())
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// feedTaskName is a short task title derived from the summary (or the thread
// key when there's no summary).
func feedTaskName(item flowdb.FeedItem) string {
	if s := strings.TrimSpace(item.Summary); s != "" {
		if len(s) > 60 {
			s = strings.TrimSpace(s[:60])
		}
		return s
	}
	return "Attention: " + item.ThreadKey
}

// feedTaskSlug derives a stable, filesystem-safe slug from the thread key.
func feedTaskSlug(item flowdb.FeedItem) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(item.ThreadKey) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "thread"
	}
	return "att-" + s
}

// feedTaskBrief assembles the context-pack brief for a new task (spec §8.2).
func feedTaskBrief(item flowdb.FeedItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", feedTaskName(item))
	summary := strings.TrimSpace(item.Summary)
	if summary == "" {
		summary = "Follow up on a message surfaced by the attention router."
	}
	fmt.Fprintf(&b, "## What\n%s\n\n", summary)
	fmt.Fprintf(&b, "## Why\nSurfaced by the attention router from %s.\n", item.Source)
	if r := strings.TrimSpace(item.Reason); r != "" {
		fmt.Fprintf(&b, "Reason flagged: %s\n", r)
	}
	fmt.Fprintf(&b, "\n## Source\nthread: %s (%s)\n", item.ThreadKey, item.Source)
	if d := strings.TrimSpace(item.Draft); d != "" {
		fmt.Fprintf(&b, "\n## Suggested reply (draft — review before sending)\n%s\n", d)
	}
	b.WriteString("\n---\n*Created from the attention feed. Read the linked thread before acting.*\n")
	return b.String()
}

// feedForwardMessage is the summarized context block forwarded to a matched
// task's inbox (spec §8.3).
func feedForwardMessage(item flowdb.FeedItem) string {
	var b strings.Builder
	b.WriteString("Forwarded by the attention router.\n")
	if s := strings.TrimSpace(item.Summary); s != "" {
		fmt.Fprintf(&b, "Summary: %s\n", s)
	}
	fmt.Fprintf(&b, "Source thread: %s (%s)\n", item.ThreadKey, item.Source)
	if r := strings.TrimSpace(item.Reason); r != "" {
		fmt.Fprintf(&b, "Why it may relate: %s\n", r)
	}
	return b.String()
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/steering/ -run 'TestMakeTaskFromFeed|TestForwardFeed|TestForwardRequiresMatchedTask|TestDismissFeed' -v`
Expected: PASS (4). Also `go build ./...`, `go vet ./internal/steering/`, `gofmt -l internal/steering/` (no output).

- [ ] **Step 5: Commit**

```bash
git add internal/steering/actions.go internal/steering/actions_test.go
git commit -m "feat(steering): feed action executors (make-task, forward, dismiss)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Autonomy-gated action router

**Files:**
- Modify: `internal/steering/actions.go` (append `ErrAutonomyDenied` + `ApplyAction`)
- Test: `internal/steering/actions_test.go` (append router/gate tests)

- [ ] **Step 1: Write the failing tests** (append to `internal/steering/actions_test.go`)

```go
func TestApplyActionManualBypassesGate(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{ID: "g1", Source: "slack", ThreadKey: "C1:1.1", Summary: "s", SuggestedAction: "make_task", Confidence: 0.1, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// manual=true, even with all-off DefaultAutonomy and low confidence → executes.
	if err := ApplyAction(context.Background(), db, item, ActionMakeTask, DefaultAutonomy(), true); err != nil {
		t.Fatalf("manual ApplyAction: %v", err)
	}
	if len(*spawns) != 1 {
		t.Errorf("manual action should execute regardless of gate, spawns=%d", len(*spawns))
	}
}

func TestApplyActionAutonomousDenied(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{ID: "g2", Source: "slack", ThreadKey: "C1:2.1", SuggestedAction: "make_task", Confidence: 0.99, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// manual=false + DefaultAutonomy (all disabled) → denied, no execution.
	err = ApplyAction(context.Background(), db, item, ActionMakeTask, DefaultAutonomy(), false)
	if err != ErrAutonomyDenied {
		t.Fatalf("autonomous make_task under default policy should be ErrAutonomyDenied, got %v", err)
	}
	if len(*spawns) != 0 {
		t.Errorf("denied action must NOT execute, spawns=%d", len(*spawns))
	}
	if items, _ := flowdb.ListFeedItems(db, "new"); len(items) != 1 {
		t.Errorf("denied action must leave the feed row untouched ('new')")
	}
}

func TestApplyActionAutonomousAllowed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{ID: "g3", Source: "slack", ThreadKey: "C1:3.1", Summary: "s", SuggestedAction: "make_task", Confidence: 0.95, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	policy := AutonomyPolicy{ActionMakeTask: {Enabled: true, Threshold: 0.80}}
	if err := ApplyAction(context.Background(), db, item, ActionMakeTask, policy, false); err != nil {
		t.Fatalf("autonomous allowed ApplyAction: %v", err)
	}
	if len(*spawns) != 1 {
		t.Errorf("allowed autonomous action should execute, spawns=%d", len(*spawns))
	}
}

func TestApplyActionUnsupported(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)
	item := flowdb.FeedItem{ID: "g4", Source: "slack", ThreadKey: "C1:4.1", SuggestedAction: "reply", Confidence: 0.9, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// reply/afk_reply are outward sends — not implemented until P2. Manual or not, ApplyAction errors.
	if err := ApplyAction(context.Background(), db, item, ActionReply, DefaultAutonomy(), true); err == nil {
		t.Error("reply action is unsupported in P1.3 and must error")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/steering/ -run TestApplyAction -v`
Expected: build failure — `undefined: ApplyAction`, `ErrAutonomyDenied`.

- [ ] **Step 3: Write the implementation** — append to `internal/steering/actions.go`

Add `"errors"` to the import block, then append:

```go
// ErrAutonomyDenied is returned when an autonomous (non-manual) action is
// blocked by the autonomy policy.
var ErrAutonomyDenied = errors.New("steering: action denied by autonomy policy")

// ApplyAction performs action on a feed item. manual=true (operator-initiated)
// bypasses the autonomy gate — the operator IS the authorization. manual=false
// (autonomous) must pass autonomy.Allow(action, item.Confidence) or it returns
// ErrAutonomyDenied without side effects. Only make_task and forward are
// supported in P1.3; reply/afk_reply (outward sends) arrive in P2.
func ApplyAction(ctx context.Context, db *sql.DB, item flowdb.FeedItem, action Action, autonomy AutonomyPolicy, manual bool) error {
	if !manual && !autonomy.Allow(action, item.Confidence) {
		return ErrAutonomyDenied
	}
	switch action {
	case ActionMakeTask:
		return MakeTaskFromFeed(ctx, db, item)
	case ActionForward:
		return ForwardFeed(ctx, db, item)
	default:
		return fmt.Errorf("steering: action %q not supported in P1.3 (make_task/forward only)", action)
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/steering/ -run TestApplyAction -v` → PASS (4).
Then `go test ./internal/steering/` (full package), `go test ./...` (module), `go build ./...`, `go vet ./internal/steering/`, `gofmt -l internal/steering/`.

- [ ] **Step 5: Commit**

```bash
git add internal/steering/actions.go internal/steering/actions_test.go
git commit -m "feat(steering): autonomy-gated ApplyAction router (manual bypass)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**1. Spec coverage (P1.3 scope):**
- §8.2 pre-assembled context pack → `feedTaskBrief` (summary, reason, source thread, draft) consumed by `MakeTaskFromFeed`. ✅
- §8.3 forward-as-summarized-handoff → `ForwardFeed` + `feedForwardMessage` via `flow tell`. ✅
- §8 autonomy engine applied to actions → `ApplyAction` gates non-manual calls via `AutonomyPolicy.Allow(action, confidence)`; manual bypass. ✅
- dismiss → `DismissFeed` (status only, ungated — always operator-safe). ✅
- *Deferred (correct):* `reply`/`afk_reply` outward sends (P2 — `ApplyAction` explicitly errors on them); the CLI `flow attention` command and the UI feed buttons that *call* these executors (P1.3b / P1.4); presence-aware AFK + send-reply (P2).

**2. Placeholder scan:** No TBD/TODO in shipped code. Task 1 Step 1 has three deliberately-marked placeholder scaffolding lines with an explicit executor note to DELETE them; the real tests use `flowdb.OpenDB`/`UpsertFeedItem` directly. Every implementation step is complete code.

**3. Type consistency:**
- `taskSpawner func(ctx, name, slug, brief, project string) error` and `taskTeller func(ctx, slug, message string) error` — stubbed with matching signatures in `stubActionIO`. ✅
- `MakeTaskFromFeed(ctx, *sql.DB, flowdb.FeedItem) error`, `ForwardFeed(...)`, `DismissFeed(*sql.DB, string) error`, `ApplyAction(ctx, *sql.DB, flowdb.FeedItem, Action, AutonomyPolicy, bool) error` — all referenced consistently across Task 1/2 tests and impl. ✅
- `flowdb.FeedItem` fields used (ID, Source, ThreadKey, Summary, MatchedTask, SuggestedProject, Reason, Draft, Confidence, Status, CreatedAt) match P1.1's struct. ✅
- `flowdb.SetFeedItemStatus(db, id, status, actedAt string) error`, `UpsertFeedItem`, `ListFeedItems` match P1.1. ✅
- `AutonomyPolicy.Allow(Action, float64) bool`, `DefaultAutonomy()`, `ActionMakeTask`/`ActionForward`/`ActionReply` from P1.1. ✅
- `markActed`/`nowRFC3339` are new private helpers used only within actions.go. ✅

No unresolved issues.

---

## After P1.3

The executor layer is complete and tested. Remaining: **P1.3b** — a `flow attention list` / `flow attention act <id> <make-task|forward|dismiss>` CLI command (the first invocation surface, so the operator can act from the terminal before the UI exists) — and **P1.4** — Mission Control settings + the Attention feed panel (whose buttons call `ApplyAction`/`DismissFeed` with `manual=true`) + push. Both are their own plans.

## Execution Handoff

Plan complete. Execute subagent-driven (recommended) or inline?
