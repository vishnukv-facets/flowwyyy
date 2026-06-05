# Attention Router — P1.3b `flow attention` CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

> ⚠️ **Repo rule:** work on branch `flow/attention-router-p1.1`; per-task commits pre-approved. Never commit to `main`.

**Goal:** Give the operator the first way to *see and act on* the Attention feed — a `flow attention list` / `flow attention act <id> <make-task|forward|dismiss>` CLI — so everything built in P1.1–P1.3 is usable from the terminal before the Mission Control UI (P1.4) exists.

**Architecture:** A thin CLI command in `internal/app/attention.go` following the established pattern (`flagSet`, `flowDBPath()`, `flowdb.OpenDB`, exit codes 0/1/2), dispatched from `app.go`. It reads via `flowdb.ListFeedItems`/a new `flowdb.GetFeedItem`, and acts via the already-tested `steering.ApplyAction` (with `manual=true` — the operator IS the authorization, so the autonomy gate is bypassed) and `steering.DismissFeed`. Feed rendering is a pure `renderAttentionFeed(items) string` so it's unit-testable without capturing stdout.

**Tech Stack:** Go, `database/sql`, table-driven tests with `t.Setenv("FLOW_ROOT", ...)` + `flowdb.OpenDB(flowDBPath())` + feed rows seeded via `flowdb.UpsertFeedItem`.

**Spec:** `docs/superpowers/specs/2026-06-04-attention-router-steerer-design.md` §7 (feed), §8 (actions). The CLI is an interim invocation surface; the canonical UI is P1.4.

**Builds on (merged on this branch):** `flowdb.FeedItem`/`ListFeedItems`/`UpsertFeedItem`/`SetFeedItemStatus` (P1.1); `steering.ApplyAction`/`DismissFeed`/`Action`/`ActionMakeTask`/`ActionForward`/`DefaultAutonomy` (P1.3); CLI helpers `flagSet`/`leadingHelpArg` (helpers.go), `flowDBPath()` (init.go:27), dispatch switch (app.go:34), sub-verb pattern (skill.go:176).

---

## File Structure

| File | Change |
|---|---|
| `internal/flowdb/attention.go` (modify) | Add `GetFeedItem(db, id) (FeedItem, error)` — fetch one row by id (needed to act on it). |
| `internal/flowdb/attention_test.go` (modify) | Add `TestGetFeedItem` (found + not-found). |
| `internal/app/attention.go` (create) | `cmdAttention` (sub-verb dispatch) · `cmdAttentionList` · `cmdAttentionAct` · pure `renderAttentionFeed`. |
| `internal/app/attention_test.go` (create) | `renderAttentionFeed` rendering + `cmdAttentionAct` dismiss/error paths. |
| `internal/app/app.go` (modify) | Register `case "attention": return cmdAttention(rest)`; add a usage line. |

---

## Task 1: `flowdb.GetFeedItem`

**Files:**
- Modify: `internal/flowdb/attention.go`
- Test: `internal/flowdb/attention_test.go`

- [ ] **Step 1: Write the failing test** (append to `internal/flowdb/attention_test.go`)

```go
func TestGetFeedItem(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	in := FeedItem{ID: "g1", Source: "slack", ThreadKey: "C1:1.1", Summary: "hi", SuggestedAction: "make_task", MatchedTask: "kong-split", Confidence: 0.7, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := UpsertFeedItem(db, in); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := GetFeedItem(db, "g1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.ID != "g1" || got.MatchedTask != "kong-split" || got.SuggestedAction != "make_task" || got.Confidence != 0.7 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if _, err := GetFeedItem(db, "nope"); err == nil {
		t.Error("GetFeedItem on a missing id must return an error")
	}
}
```

(`openTempDB` is the existing flowdb test helper used by the other attention tests.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/flowdb/ -run TestGetFeedItem -v`
Expected: build failure — `undefined: GetFeedItem`.

- [ ] **Step 3: Write the implementation** (append to `internal/flowdb/attention.go`)

```go
// GetFeedItem fetches a single feed row by id. Returns a wrapped error
// (including sql.ErrNoRows) when no row matches.
func GetFeedItem(db *sql.DB, id string) (FeedItem, error) {
	var it FeedItem
	var matched, project, priority, urgency, draft, reason, ctx, snooze, acted sql.NullString
	var isVIP int
	err := db.QueryRow(
		`SELECT id, source, thread_key, summary, suggested_action, matched_task,
		        suggested_project, suggested_priority, urgency, is_vip, confidence,
		        draft, reason, context_json, status, snooze_until, created_at, acted_at
		 FROM attention_feed WHERE id = ?`, id,
	).Scan(
		&it.ID, &it.Source, &it.ThreadKey, &it.Summary, &it.SuggestedAction, &matched,
		&project, &priority, &urgency, &isVIP, &it.Confidence,
		&draft, &reason, &ctx, &it.Status, &snooze, &it.CreatedAt, &acted,
	)
	if err != nil {
		return FeedItem{}, fmt.Errorf("flowdb: get feed item %q: %w", id, err)
	}
	it.MatchedTask = matched.String
	it.SuggestedProject = project.String
	it.SuggestedPriority = priority.String
	it.Urgency = urgency.String
	it.IsVIP = isVIP != 0
	it.Draft = draft.String
	it.Reason = reason.String
	it.ContextJSON = ctx.String
	it.SnoozeUntil = snooze.String
	it.ActedAt = acted.String
	return it, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/flowdb/ -run TestGetFeedItem -v` → PASS. Then `go test ./internal/flowdb/` (full package), `go build ./...`, `go vet ./internal/flowdb/`, `gofmt -l internal/flowdb/` (no output).

- [ ] **Step 5: Commit**

```bash
git add internal/flowdb/attention.go internal/flowdb/attention_test.go
git commit -m "feat(flowdb): GetFeedItem by id

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: `flow attention` command

**Files:**
- Create: `internal/app/attention.go`
- Test: `internal/app/attention_test.go`
- Modify: `internal/app/app.go` (register the command + usage line)

- [ ] **Step 1: Write the failing test** — `internal/app/attention_test.go`

```go
package app

import (
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

// attentionTestDB points FLOW_ROOT/HOME at a temp dir and returns an open DB at
// the same path the command will use (flowDBPath()).
func attentionTestDB(t *testing.T) *flowdb.DB {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", root)
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRenderAttentionFeed(t *testing.T) {
	items := []flowdb.FeedItem{
		{ID: "abc123", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "make_task", Confidence: 0.88, Urgency: "urgent", MatchedTask: "", Summary: "Customer wants rollout date"},
	}
	out := renderAttentionFeed(items)
	if !strings.Contains(out, "abc123") || !strings.Contains(out, "make_task") || !strings.Contains(out, "Customer wants rollout date") {
		t.Errorf("rendered feed missing fields:\n%s", out)
	}

	empty := renderAttentionFeed(nil)
	if !strings.Contains(strings.ToLower(empty), "no ") && strings.TrimSpace(empty) == "" {
		t.Errorf("empty feed should render a friendly message, got %q", empty)
	}
}

func TestCmdAttentionActDismiss(t *testing.T) {
	db := attentionTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{ID: "d1", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rc := cmdAttentionAct([]string{"d1", "dismiss"}); rc != 0 {
		t.Fatalf("act dismiss rc = %d, want 0", rc)
	}
	if items, _ := flowdb.ListFeedItems(db, "dismissed"); len(items) != 1 {
		t.Errorf("item should be dismissed, got %d dismissed rows", len(items))
	}
}

func TestCmdAttentionActErrors(t *testing.T) {
	db := attentionTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{ID: "e1", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rc := cmdAttentionAct([]string{"e1"}); rc != 2 {
		t.Errorf("missing action arg should rc=2, got %d", rc)
	}
	if rc := cmdAttentionAct([]string{"e1", "frobnicate"}); rc != 2 {
		t.Errorf("unknown action should rc=2, got %d", rc)
	}
	if rc := cmdAttentionAct([]string{"missing-id", "dismiss"}); rc != 1 {
		t.Errorf("missing feed item should rc=1, got %d", rc)
	}
}

func TestCmdAttentionListRuns(t *testing.T) {
	attentionTestDB(t)
	if rc := cmdAttentionList(nil); rc != 0 {
		t.Errorf("list on empty feed should rc=0, got %d", rc)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/app/ -run 'TestRenderAttentionFeed|TestCmdAttention' -v`
Expected: build failure — `undefined: renderAttentionFeed`, `cmdAttentionAct`, `cmdAttentionList`.

- [ ] **Step 3: Write the implementation** — `internal/app/attention.go`

```go
package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/steering"
)

// cmdAttention implements `flow attention <list|act>` — the terminal surface
// for the attention router's feed (the Mission Control feed panel is P1.4).
func cmdAttention(args []string) int {
	if leadingHelpArg(args) || len(args) == 0 {
		printAttentionUsage()
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdAttentionList(rest)
	case "act":
		return cmdAttentionAct(rest)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown attention subcommand %q (want list|act)\n", sub)
		printAttentionUsage()
		return 2
	}
}

func printAttentionUsage() {
	fmt.Println(`flow attention — review and act on the attention feed

  flow attention list [--status new|acted|dismissed|snoozed|all]   (default: new)
  flow attention act <id> <make-task|forward|dismiss>`)
}

func cmdAttentionList(args []string) int {
	fs := flagSet("attention list")
	status := fs.String("status", "new", "filter: new|acted|dismissed|snoozed|all")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}
	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	filter := strings.TrimSpace(*status)
	if filter == "all" {
		filter = ""
	}
	items, err := flowdb.ListFeedItems(db, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Print(renderAttentionFeed(items))
	return 0
}

func cmdAttentionAct(args []string) int {
	if leadingHelpArg(args) {
		printAttentionUsage()
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "error: act requires <id> and <make-task|forward|dismiss>")
		return 2
	}
	id, actionArg := args[0], strings.ToLower(strings.TrimSpace(args[1]))

	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	item, err := flowdb.GetFeedItem(db, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: no feed item with id %q\n", id)
		return 1
	}

	switch actionArg {
	case "dismiss":
		if err := steering.DismissFeed(db, id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Printf("dismissed %s\n", id)
		return 0
	case "make-task", "make_task":
		return runAttentionAction(db, item, steering.ActionMakeTask, "made task from")
	case "forward":
		return runAttentionAction(db, item, steering.ActionForward, "forwarded")
	default:
		fmt.Fprintf(os.Stderr, "error: unknown action %q (want make-task|forward|dismiss)\n", actionArg)
		return 2
	}
}

// runAttentionAction applies an operator-initiated (manual) feed action and
// reports the result. manual=true bypasses the autonomy gate — the operator
// at the terminal is the authorization.
func runAttentionAction(db *flowdb.DB, item flowdb.FeedItem, action steering.Action, verb string) int {
	if err := steering.ApplyAction(context.Background(), db, item, action, steering.DefaultAutonomy(), true); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("%s %s\n", verb, item.ID)
	return 0
}

func openAttentionDB() (*flowdb.DB, int) {
	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return nil, 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return nil, 1
	}
	return db, 0
}

// renderAttentionFeed renders feed items as a compact table. Pure (no I/O) so
// it's unit-testable.
func renderAttentionFeed(items []flowdb.FeedItem) string {
	if len(items) == 0 {
		return "No attention items.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s  %-7s  %-10s  %-5s  %-7s  %-14s  %s\n",
		"ID", "SOURCE", "ACTION", "CONF", "URGENCY", "MATCHED", "SUMMARY")
	for _, it := range items {
		matched := it.MatchedTask
		if matched == "" {
			matched = "-"
		}
		fmt.Fprintf(&b, "%-10s  %-7s  %-10s  %-5.2f  %-7s  %-14s  %s\n",
			shortID(it.ID), it.Source, it.SuggestedAction, it.Confidence,
			orDash(it.Urgency), matched, it.Summary)
	}
	return b.String()
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
```

> **Type note:** `flowdb.OpenDB` returns `*sql.DB`. This plan writes `*flowdb.DB` in a couple of signatures (`runAttentionAction`, `openAttentionDB`) as shorthand — when implementing, use whatever type `flowdb.OpenDB` actually returns (it is `*sql.DB`; `steering.ApplyAction`/`DismissFeed` and `flowdb.*` all take `*sql.DB`). If `*flowdb.DB` does not exist, use `*sql.DB` and add `"database/sql"` to the imports. Verify by reading the return type of `flowdb.OpenDB` before writing the signatures.

- [ ] **Step 4: Register the command** — in `internal/app/app.go`, add to the dispatch `switch cmd` (e.g., after the `case "wait":` block):

```go
	case "attention":
		return cmdAttention(rest)
```

And add a usage block to `printUsage()` (after the `Read:` section's `flow list tags` line is a good spot):

```
  flow attention list  [--status new|acted|dismissed|all]   (review the attention-router feed)
  flow attention act   <id> <make-task|forward|dismiss>
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/app/ -run 'TestRenderAttentionFeed|TestCmdAttention' -v` → PASS (4).
Then `go test ./internal/app/` (full package), `go test ./...` (module), `go build ./...`, `go build -o flow .`, `go vet ./internal/app/`, `gofmt -l internal/app/attention.go internal/app/app.go` (no output).

- [ ] **Step 6: Commit**

```bash
git add internal/app/attention.go internal/app/attention_test.go internal/app/app.go
git commit -m "feat(app): flow attention list/act CLI for the feed

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**1. Spec coverage (P1.3b scope):**
- §7 view the feed → `flow attention list` + `renderAttentionFeed` + `flowdb.ListFeedItems`. ✅
- §8 act on a feed item → `flow attention act` → `steering.ApplyAction(manual=true)` / `steering.DismissFeed`; `GetFeedItem` fetches the row to act on. ✅
- Operator authorization → `manual=true` bypasses the autonomy gate (operator at the terminal). ✅
- *Deferred (correct):* the Mission Control feed panel + settings + push (P1.4); autonomous action paths (P2). The `make-task`/`forward` happy paths shell out to `flow spawn`/`flow tell` (covered by `steering`'s own mocked unit tests in P1.3) — the CLI test exercises `list`, `dismiss`, and error paths, which need no shell-out.

**2. Placeholder scan:** No TBD/TODO. The `*flowdb.DB` vs `*sql.DB` type note is an explicit instruction to verify-and-use the real return type, not a placeholder. Every step has complete code.

**3. Type consistency:**
- `cmdAttention`/`cmdAttentionList`/`cmdAttentionAct([]string) int`, `renderAttentionFeed([]flowdb.FeedItem) string`, `runAttentionAction`, `openAttentionDB` — referenced consistently between tests and impl. ✅
- `steering.ApplyAction(ctx, db, FeedItem, Action, AutonomyPolicy, bool) error`, `steering.DismissFeed(db, id) error`, `steering.ActionMakeTask`/`ActionForward`, `steering.DefaultAutonomy()` — match P1.3. ✅
- `flowdb.GetFeedItem(db, id) (FeedItem, error)` defined in Task 1, used in Task 2. ✅
- `flowdb.ListFeedItems(db, status)` / `UpsertFeedItem` / `FeedItem` fields — match P1.1. ✅
- CLI helpers `flagSet`/`leadingHelpArg`/`parseFlagSet`/`flowDBPath` — match helpers.go/init.go. ✅
- DB type: must match `flowdb.OpenDB`'s actual return (`*sql.DB`) — flagged in the type note; verified at implementation time. ✅

No unresolved issues.

---

## After P1.3b

The operator can now run `flow attention list` and act on items from the terminal — P1 is end-to-end usable without the UI. Remaining: **P1.4** (Mission Control settings page with the channel multi-select + the Attention feed panel whose buttons call `ApplyAction`/`DismissFeed` + push notifications). Then P2–P4.

## Execution Handoff

Plan complete. Execute subagent-driven (recommended) or inline?
