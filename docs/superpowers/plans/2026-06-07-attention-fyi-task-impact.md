# Attention FYI Task Impact Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make availability/FYI Attention cards task-aware, move pure digest-only FYI out of Needs action, and make Mission Control briefing rows explain the operator impact clearly.

**Architecture:** Add a deterministic task-impact hint builder in `internal/steering`, feed those hints into Stage 3 deep triage, then classify Attention feed rows into briefing buckets based on `suggested_action`, `matched_task`, and task waiting state. Keep the existing `attention_feed` schema and five-column Overview layout; the server returns clearer `Item.Detail` and `Item.Action` fields that the current UI can render.

**Tech Stack:** Go, SQLite via `modernc.org/sqlite`, existing Flow task/update filesystem layout, React/TypeScript Mission Control UI for smoke verification.

---

## File Structure

- Create: `internal/steering/task_impact.go`
  - Defines `TaskImpactHint`, `BuildTaskImpactHints`, text normalization, conservative match scoring, and bounded task snippet collection.
- Create: `internal/steering/task_impact_test.go`
  - Tests strong `waiting_on` matches, no-match FYI, and weak-token suppression.
- Modify: `internal/steering/triage.go`
  - Add a hints-aware prompt builder and prompt instructions.
- Modify: `internal/steering/triage_test.go`
  - Assert Stage 3 prompt contains task-impact hints and explicit action guidance.
- Modify: `internal/steering/cascade.go`
  - Build task-impact hints before Stage 3 and pass them into the prompt.
- Modify: `internal/briefing/briefing.go`
  - Split Attention items into NeedsAction, Waiting, and FYI based on action and matched task.
- Modify: `internal/briefing/briefing_test.go`
  - Pin digest-only FYI placement and task-impact/waiting placement.
- Modify: `internal/server/ui/src/screens/Overview.tsx`
  - Render briefing action labels as the first metadata token so FYI cards show
    `No action` or `Review affected task` before lower-signal source metadata.

---

### Task 1: Add Deterministic Task-Impact Hints

**Files:**
- Create: `internal/steering/task_impact.go`
- Create: `internal/steering/task_impact_test.go`

- [ ] **Step 1: Write failing test for waiting_on association**

Create `internal/steering/task_impact_test.go`:

```go
package steering

import (
	"database/sql"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
)

func taskImpactDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, root
}

func seedImpactTask(t *testing.T, db *sql.DB, slug, name, status, priority, waitingOn, assignee string) {
	t.Helper()
	sessionID := any(nil)
	if status != "backlog" {
		sessionID = "00000000-0000-4000-8000-000000000001"
	}
	_, err := db.Exec(
		`INSERT INTO tasks (
			slug, name, status, priority, work_dir, waiting_on, assignee,
			session_provider, session_id, status_changed_at, created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, 'codex', ?, '2026-06-07T07:00:00Z', '2026-06-07T07:00:00Z', '2026-06-07T07:00:00Z')`,
		slug, name, status, priority, t.TempDir(), nullString(waitingOn), nullString(assignee), sessionID,
	)
	if err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestBuildTaskImpactHintsMatchesWaitingOnPerson(t *testing.T) {
	db, _ := taskImpactDB(t)
	seedImpactTask(t, db, "raptor-review", "Raptor airgapped review", "in-progress", "high", "Rohit review on PR #159", "")
	seedImpactTask(t, db, "unrelated", "Unrelated rollout", "in-progress", "medium", "Anshul approval", "")

	hints, err := BuildTaskImpactHints(db, TaskImpactInput{
		Source: "slack",
		People: []string{"Rohit Raveendran"},
		Text:   "I will be on leave tomorrow and the day after.",
	})
	if err != nil {
		t.Fatalf("BuildTaskImpactHints: %v", err)
	}
	if len(hints) != 1 {
		t.Fatalf("hints len = %d, want 1: %+v", len(hints), hints)
	}
	if hints[0].TaskSlug != "raptor-review" {
		t.Fatalf("TaskSlug = %q, want raptor-review: %+v", hints[0].TaskSlug, hints[0])
	}
	if hints[0].Strength != "strong" || hints[0].Reason == "" || hints[0].Evidence == "" {
		t.Fatalf("hint missing strength/reason/evidence: %+v", hints[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./internal/steering -run TestBuildTaskImpactHintsMatchesWaitingOnPerson
```

Expected: FAIL with `undefined: BuildTaskImpactHints` and `undefined: TaskImpactInput`.

- [ ] **Step 3: Implement minimal hint types and waiting_on matching**

Create `internal/steering/task_impact.go`:

```go
package steering

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"flow/internal/flowdb"
)

type TaskImpactInput struct {
	Source string
	People []string
	Text   string
}

type TaskImpactHint struct {
	TaskSlug    string `json:"task_slug"`
	TaskName    string `json:"task_name"`
	ProjectSlug string `json:"project_slug,omitempty"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	Strength    string `json:"strength"`
	Reason      string `json:"reason"`
	Evidence    string `json:"evidence"`
}

var impactTokenRE = regexp.MustCompile(`[a-z0-9#._/-]+`)

func BuildTaskImpactHints(db *sql.DB, in TaskImpactInput) ([]TaskImpactHint, error) {
	if db == nil {
		return nil, fmt.Errorf("steering: task impact requires db")
	}
	people := impactNames(in)
	if len(people) == 0 {
		return nil, nil
	}
	tasks, err := flowdb.ListTasks(db, flowdb.TaskFilter{Kind: "", IncludeArchived: false})
	if err != nil {
		return nil, fmt.Errorf("steering: list tasks for impact hints: %w", err)
	}
	var hints []TaskImpactHint
	for _, task := range tasks {
		if task.DeletedAt.Valid || task.Status == "done" {
			continue
		}
		if task.WaitingOn.Valid {
			if person, ok := matchImpactPerson(task.WaitingOn.String, people); ok {
				hints = append(hints, taskImpactHint(task, "strong", "waiting_on mentions "+person, strings.TrimSpace(task.WaitingOn.String)))
			}
		}
	}
	sort.SliceStable(hints, func(i, j int) bool {
		if impactStrengthRank(hints[i].Strength) != impactStrengthRank(hints[j].Strength) {
			return impactStrengthRank(hints[i].Strength) > impactStrengthRank(hints[j].Strength)
		}
		if hints[i].Priority != hints[j].Priority {
			return impactPriorityRank(hints[i].Priority) > impactPriorityRank(hints[j].Priority)
		}
		return hints[i].TaskSlug < hints[j].TaskSlug
	})
	if len(hints) > 3 {
		hints = hints[:3]
	}
	return hints, nil
}

func impactNames(in TaskImpactInput) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		key := strings.ToLower(s)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, s)
	}
	for _, p := range in.People {
		add(p)
	}
	return out
}

func matchImpactPerson(text string, people []string) (string, bool) {
	hay := impactTokens(text)
	for _, person := range people {
		tokens := impactTokens(person)
		matched := 0
		for token := range tokens {
			if len(token) < 3 || impactCommonToken(token) {
				continue
			}
			if hay[token] {
				matched++
			}
		}
		if matched > 0 {
			return person, true
		}
	}
	return "", false
}

func impactTokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range impactTokenRE.FindAllString(strings.ToLower(text), -1) {
		if token != "" {
			out[token] = true
		}
	}
	return out
}

func impactCommonToken(token string) bool {
	switch token {
	case "review", "approval", "task", "work", "leave", "tomorrow", "after", "the", "and", "for":
		return true
	default:
		return false
	}
}

func taskImpactHint(task *flowdb.Task, strength, reason, evidence string) TaskImpactHint {
	project := ""
	if task.ProjectSlug.Valid {
		project = task.ProjectSlug.String
	}
	return TaskImpactHint{
		TaskSlug: task.Slug, TaskName: task.Name, ProjectSlug: project,
		Status: task.Status, Priority: task.Priority, Strength: strength,
		Reason: reason, Evidence: evidence,
	}
}

func impactStrengthRank(s string) int {
	if s == "strong" {
		return 2
	}
	if s == "medium" {
		return 1
	}
	return 0
}

func impactPriorityRank(p string) int {
	switch p {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run:

```bash
go test ./internal/steering -run TestBuildTaskImpactHintsMatchesWaitingOnPerson
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/steering/task_impact.go internal/steering/task_impact_test.go
git commit -m "feat: add attention task impact hints"
```

---

### Task 2: Add Assignee, Task Name, Tags, and Weak-Match Suppression

**Files:**
- Modify: `internal/steering/task_impact.go`
- Modify: `internal/steering/task_impact_test.go`

- [ ] **Step 1: Add failing tests for assignee/name matches and weak-token suppression**

Append to `internal/steering/task_impact_test.go`:

```go
func TestBuildTaskImpactHintsMatchesAssigneeAndTaskName(t *testing.T) {
	db, _ := taskImpactDB(t)
	seedImpactTask(t, db, "anshul-review", "Anshul review on Facets-cloud/raptor#159", "in-progress", "high", "", "Anshul Sao")

	hints, err := BuildTaskImpactHints(db, TaskImpactInput{
		Source: "slack",
		People: []string{"Anshul Sao"},
		Text:   "I can review this after Monday.",
	})
	if err != nil {
		t.Fatalf("BuildTaskImpactHints: %v", err)
	}
	if len(hints) != 1 || hints[0].TaskSlug != "anshul-review" {
		t.Fatalf("hints = %+v, want anshul-review", hints)
	}
	if hints[0].Strength != "strong" {
		t.Fatalf("Strength = %q, want strong", hints[0].Strength)
	}
}

func TestBuildTaskImpactHintsIgnoresWeakCommonTokens(t *testing.T) {
	db, _ := taskImpactDB(t)
	seedImpactTask(t, db, "review-task", "Review task", "in-progress", "high", "external review", "")

	hints, err := BuildTaskImpactHints(db, TaskImpactInput{
		Source: "slack",
		People: []string{"Review"},
		Text:   "General office closure Friday.",
	})
	if err != nil {
		t.Fatalf("BuildTaskImpactHints: %v", err)
	}
	if len(hints) != 0 {
		t.Fatalf("hints = %+v, want none for weak common token", hints)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./internal/steering -run 'TestBuildTaskImpactHints(MatchesAssigneeAndTaskName|IgnoresWeakCommonTokens)'
```

Expected: assignee/name test FAILS because only `waiting_on` is implemented. Weak-token test passes because common-token filtering already exists.

- [ ] **Step 3: Implement assignee, task name, and tag matching**

Replace the entire `BuildTaskImpactHints` function in `internal/steering/task_impact.go`:

```go
func BuildTaskImpactHints(db *sql.DB, in TaskImpactInput) ([]TaskImpactHint, error) {
	if db == nil {
		return nil, fmt.Errorf("steering: task impact requires db")
	}
	people := impactNames(in)
	if len(people) == 0 {
		return nil, nil
	}
	tasks, err := flowdb.ListTasks(db, flowdb.TaskFilter{Kind: "", IncludeArchived: false})
	if err != nil {
		return nil, fmt.Errorf("steering: list tasks for impact hints: %w", err)
	}
	slugs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		slugs = append(slugs, task.Slug)
	}
	tagsBySlug, err := flowdb.GetTaskTagsBatch(db, slugs)
	if err != nil {
		return nil, fmt.Errorf("steering: task tags for impact hints: %w", err)
	}
	seenTasks := map[string]bool{}
	addHint := func(task *flowdb.Task, strength, reason, evidence string) {
		if seenTasks[task.Slug] {
			return
		}
		seenTasks[task.Slug] = true
		hints = append(hints, taskImpactHint(task, strength, reason, evidence))
	}
	for _, task := range tasks {
		if task.DeletedAt.Valid || task.Status == "done" {
			continue
		}
		if task.WaitingOn.Valid {
			if person, ok := matchImpactPerson(task.WaitingOn.String, people); ok {
				addHint(task, "strong", "waiting_on mentions "+person, strings.TrimSpace(task.WaitingOn.String))
				continue
			}
		}
		if task.Assignee.Valid {
			if person, ok := matchImpactPerson(task.Assignee.String, people); ok {
				addHint(task, "strong", "assignee matches "+person, strings.TrimSpace(task.Assignee.String))
				continue
			}
		}
		if person, ok := matchImpactPerson(task.Name, people); ok {
			addHint(task, "medium", "task name mentions "+person, task.Name)
			continue
		}
		for _, tag := range tagsBySlug[task.Slug] {
			if person, ok := matchImpactPerson(tag, people); ok {
				addHint(task, "medium", "task tag mentions "+person, tag)
				break
			}
		}
	}
	sort.SliceStable(hints, func(i, j int) bool {
		if impactStrengthRank(hints[i].Strength) != impactStrengthRank(hints[j].Strength) {
			return impactStrengthRank(hints[i].Strength) > impactStrengthRank(hints[j].Strength)
		}
		if hints[i].Priority != hints[j].Priority {
			return impactPriorityRank(hints[i].Priority) > impactPriorityRank(hints[j].Priority)
		}
		return hints[i].TaskSlug < hints[j].TaskSlug
	})
	if len(hints) > 3 {
		hints = hints[:3]
	}
	return hints, nil
}
```

- [ ] **Step 4: Run tests to verify pass**

Run:

```bash
go test ./internal/steering -run 'TestBuildTaskImpactHints'
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/steering/task_impact.go internal/steering/task_impact_test.go
git commit -m "feat: match task impact metadata"
```

---

### Task 3: Feed Task-Impact Hints Into Stage 3

**Files:**
- Modify: `internal/steering/triage.go`
- Modify: `internal/steering/triage_test.go`
- Modify: `internal/steering/cascade.go`

- [ ] **Step 1: Write failing prompt test**

Append to `internal/steering/triage_test.go`:

```go
func TestDeepTriagePromptIncludesTaskImpactHints(t *testing.T) {
	pack := ThreadContext{
		Source:      "slack",
		ThreadKey:   "C1:1.1",
		FetchStatus: "ok",
		Parent: &ContextMessage{
			Kind: "parent", Author: "Rohit Raveendran",
			Text: "I will be on leave tomorrow and the day after.", TS: "1.1",
		},
	}
	hints := []TaskImpactHint{{
		TaskSlug: "raptor-review", TaskName: "Raptor airgapped review",
		ProjectSlug: "raptor", Status: "in-progress", Priority: "high",
		Strength: "strong", Reason: "waiting_on mentions Rohit Raveendran",
		Evidence: "Rohit review on PR #159",
	}}
	prompt := deepTriagePromptWithContextAndHints(
		ClassifyInput{ThreadKey: "C1:1.1", Source: "slack", Author: "Rohit Raveendran", Text: "I will be on leave tomorrow and the day after."},
		"Tasks:\n- raptor-review (in-progress): Raptor airgapped review",
		pack,
		hints,
	)
	for _, want := range []string{
		"Task-impact hints (JSON):",
		`"task_slug":"raptor-review"`,
		`"waiting_on mentions Rohit Raveendran"`,
		"Availability/FYI events are not automatically actionable",
		"set matched_task to the strongest affected task",
		`Use "forward" when the affected task/session should know`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run:

```bash
go test ./internal/steering -run TestDeepTriagePromptIncludesTaskImpactHints
```

Expected: FAIL with `undefined: deepTriagePromptWithContextAndHints`.

- [ ] **Step 3: Add hints-aware prompt wrapper**

In `internal/steering/triage.go`, replace the existing `DeepTriageWithContext`
function with:

```go
func DeepTriageWithContextAndHints(ctx context.Context, in ClassifyInput, taskIndex string, pack ThreadContext, hints []TaskImpactHint) (Verdict, error) {
	raw, err := deepTriageRunner(ctx, deepTriagePromptWithContextAndHints(in, taskIndex, pack, hints))
	if err != nil {
		return Verdict{}, err
	}
	return parseVerdict(raw, in.Source, in.ThreadKey)
}

func DeepTriageWithContext(ctx context.Context, in ClassifyInput, taskIndex string, pack ThreadContext) (Verdict, error) {
	return DeepTriageWithContextAndHints(ctx, in, taskIndex, pack, nil)
}
```

Replace the body of `deepTriagePromptWithContext` with:

```go
func deepTriagePromptWithContext(in ClassifyInput, taskIndex string, pack ThreadContext) string {
	return deepTriagePromptWithContextAndHints(in, taskIndex, pack, nil)
}
```

Add this new function below it:

```go
func deepTriagePromptWithContextAndHints(in ClassifyInput, taskIndex string, pack ThreadContext, hints []TaskImpactHint) string {
	payload, _ := json.Marshal(modelFacingClassifyInput(in))
	contextPayload, _ := json.Marshal(modelFacingThreadContext(pack))
	hintsPayload, _ := json.Marshal(hints)
	return `MODE: stage3-deep

You are the deep-triage step of an operator's attention router. A cheap gate has already decided this message is worth a closer look. Go has already fetched the surrounding source context into the context pack below. Treat that context pack as the primary source of truth; do not rely on fetching Slack/GitHub context yourself. If fetch_status is "error" or "unavailable", proceed from the fallback event context and lower confidence when the missing context matters.

Do the following, then emit a single verdict:

1. Read the context pack's source permalink, parent message, replies/comments, participants, timestamps, and pre-summary.
2. Read the task-impact hints. Availability/FYI events are not automatically actionable. If task-impact hints show the sender or named participant is blocking, reviewing, assigned to, or otherwise affecting active work, set matched_task to the strongest affected task and explain the impact.
3. Decide whether this message belongs to an EXISTING task (set matched_task) or warrants a new one. Do NOT decide from the task name alone — for any plausibly related task (especially ones in the project this message seems to belong to), use your file tools to READ that task's brief.md AND the progress notes in its updates/ directory (paths are given in the index below) before judging. A message belongs to an existing task when it continues, follows up on, or is the next step of the work that task covers — even if it arrives in a different Slack thread/DM. Prefer matched_task to an existing active task in such cases; only treat it as net-new when, after reading, no active task actually covers it.
4. Use "forward" when the affected task/session should know about the update. Use "digest_only" when there is no affected task and no reply needed.
5. If a reply from the operator is appropriate, draft it in the operator's voice. DO NOT SEND ANYTHING — the draft is surfaced for the operator's approval only.

Always refer to people and channels by name; never output raw platform IDs (e.g. Slack user IDs like U0123, channel IDs like C0123).
Do not mention context fetch failures, API/token/channel access errors, fetch_status, fetch_error, or missing source context in summary, draft, or reason. Those fields are internal audit details; base the verdict on the visible fallback event context and lower confidence only when missing context materially changes the decision.

Respond with ONLY a minified JSON object (no prose, fences allowed but optional):
{"suggested_action":"make_task|forward|reply|afk_reply|digest_only|drop","matched_task":"<slug or empty>","suggested_project":"<slug or empty>","suggested_priority":"high|medium|low","urgency":"urgent|normal|low","confidence":0.0,"summary":"<= 140 chars","draft":"<reply text, if any>","reason":"<why>"}

Operator task/project index:
` + taskIndex + `

Task-impact hints (JSON):
` + string(hintsPayload) + `

Context pack (JSON):
` + string(contextPayload) + `

Message (JSON):
` + string(payload)
}
```

- [ ] **Step 4: Wire cascade to build and pass hints**

In `internal/steering/cascade.go`, replace the existing line
`pack := c.contextPack(ctx, ev)` in `finishItem` with:

```go
	pack := c.contextPack(ctx, ev)
	hints, hintErr := BuildTaskImpactHints(c.DB, TaskImpactInput{
		Source: in.Source,
		People: taskImpactPeopleFromContext(pack, ev),
		Text:   in.Text,
	})
	if hintErr != nil {
		tr.Error = strings.TrimSpace(tr.Error + "; task impact hints failed: " + hintErr.Error())
		hints = nil
	}
```

Replace:

```go
	v3, err := DeepTriageWithContext(ctx, in, taskIndex, pack)
```

with:

```go
	v3, err := DeepTriageWithContextAndHints(ctx, in, taskIndex, pack, hints)
```

Add helper in `internal/steering/task_impact.go`:

```go
func taskImpactPeopleFromContext(pack ThreadContext, ev monitor.InboundEvent) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			return
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	add(ev.UserID)
	if pack.Parent != nil {
		add(pack.Parent.Author)
	}
	for _, p := range pack.Participants {
		add(p)
	}
	return out
}
```

Also import `flow/internal/monitor` in `task_impact.go`.

- [ ] **Step 5: Run steering tests**

Run:

```bash
go test ./internal/steering
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/steering/triage.go internal/steering/triage_test.go internal/steering/cascade.go internal/steering/task_impact.go
git commit -m "feat: pass task impact hints to attention triage"
```

---

### Task 4: Bucket Attention FYI Correctly in Briefing

**Files:**
- Modify: `internal/briefing/briefing.go`
- Modify: `internal/briefing/briefing_test.go`

- [ ] **Step 1: Write failing briefing tests**

Append to `internal/briefing/briefing_test.go`:

```go
func TestBriefingBucketsDigestOnlyAttentionAsFYI(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "leave-fyi", Source: "slack", ThreadKey: "C1:1.1",
		Summary: "Rohit is out for two days", SuggestedAction: "digest_only",
		Reason: "No active task is waiting on Rohit. No action.",
		Urgency: "low", Confidence: 0.9, Status: "new", CreatedAt: "2026-06-07T08:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	got, err := Build(db, root, Options{Now: now, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	item := requireItem(t, got.FYI, "attention", "leave-fyi")
	if item.Action != "No action" {
		t.Fatalf("Action = %q, want No action", item.Action)
	}
	if _, ok := findItem(got.NeedsAction, "attention", "leave-fyi"); ok {
		t.Fatalf("digest_only FYI must not appear in NeedsAction: %+v", got.NeedsAction)
	}
}

func TestBriefingBucketsTaskImpactAttentionWithMatchedWaitingTask(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)
	seedBriefingProject(t, db, root, "raptor")
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "raptor-review", Name: "Raptor airgapped review", Status: "in-progress",
		Priority: "high", Project: "raptor", WaitingOn: "Rohit review on PR #159",
		UpdatedAt: "2026-06-07T07:00:00Z",
	})
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "leave-impact", Source: "slack", ThreadKey: "C1:1.1",
		Summary: "Rohit is out for two days", SuggestedAction: "forward",
		MatchedTask: "raptor-review", Reason: "May affect Raptor airgapped review: waiting on Rohit review on PR #159.",
		Urgency: "normal", Confidence: 0.9, Status: "new", CreatedAt: "2026-06-07T08:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	got, err := Build(db, root, Options{Now: now, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	item := requireItem(t, got.Waiting, "attention", "leave-impact")
	if item.Action != "Update waiting task" {
		t.Fatalf("Action = %q, want Update waiting task", item.Action)
	}
	if !strings.Contains(item.Detail, "May affect") {
		t.Fatalf("Detail = %q, want task-impact sentence", item.Detail)
	}
	requireLink(t, item, "task", "raptor-review")
}
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./internal/briefing -run 'TestBriefingBuckets(DigestOnlyAttentionAsFYI|TaskImpactAttentionWithMatchedWaitingTask)'
```

Expected: digest-only item FAILS because `attentionItems` currently returns all Attention items for NeedsAction.

- [ ] **Step 3: Split attention items by bucket**

In `internal/briefing/briefing.go`, replace the existing block that appends all
Attention rows to `out.NeedsAction`:

```go
	attention, err := attentionItems(db, taskBySlug)
	if err != nil {
		return Briefing{}, err
	}
	out.NeedsAction = append(out.NeedsAction, attention...)
```

with:

```go
	attention, err := attentionItems(db, taskBySlug)
	if err != nil {
		return Briefing{}, err
	}
	for _, item := range attention {
		switch attentionBriefingBucket(item, taskBySlug) {
		case "fyi":
			out.FYI = append(out.FYI, item)
		case "waiting":
			out.Waiting = append(out.Waiting, item)
		default:
			out.NeedsAction = append(out.NeedsAction, item)
		}
	}
```

Add helper functions in `internal/briefing/briefing.go`:

```go
func attentionBriefingBucket(item Item, tasks map[string]*flowdb.Task) string {
	action := strings.ToLower(strings.TrimSpace(item.Action))
	taskSlug := ""
	for _, link := range item.Links {
		if link.Kind == "task" {
			taskSlug = link.Target
			break
		}
	}
	if action == "digest_only" && taskSlug == "" {
		return "fyi"
	}
	if taskSlug != "" {
		if task := tasks[taskSlug]; task != nil && task.WaitingOn.Valid && strings.TrimSpace(task.WaitingOn.String) != "" {
			return "waiting"
		}
	}
	return "needs_action"
}

func attentionActionLabel(row flowdb.FeedItem, task *flowdb.Task) string {
	action := strings.ToLower(strings.TrimSpace(row.SuggestedAction))
	switch action {
	case "digest_only":
		if row.MatchedTask == "" {
			return "No action"
		}
		return "Review affected task"
	case "forward":
		if task != nil && task.WaitingOn.Valid && strings.TrimSpace(task.WaitingOn.String) != "" {
			return "Update waiting task"
		}
		return "Review affected task"
	case "reply", "afk_reply":
		return "Review reply"
	case "make_task":
		return "Make task"
	default:
		return row.SuggestedAction
	}
}
```

In `attentionItems`, replace the `item := Item{...}` construction with:

```go
		matchedTask := tasks[row.MatchedTask]
		item := Item{
			Kind:    "attention",
			Ref:     row.ID,
			Source:  nonEmpty(row.Source, "attention"),
			Project: row.SuggestedProject,
			Urgency: row.Urgency,
			Title:   nonEmpty(row.Summary, "Attention item "+row.ID),
			Detail:  attentionDetail(row, matchedTask),
			Action:  attentionActionLabel(row, matchedTask),
			Links:   []Link{{Kind: "attention", Target: row.ID}},
		}
```

Add helper:

```go
func attentionDetail(row flowdb.FeedItem, task *flowdb.Task) string {
	reason := strings.TrimSpace(row.Reason)
	if reason != "" {
		return reason
	}
	if strings.ToLower(strings.TrimSpace(row.SuggestedAction)) == "digest_only" && row.MatchedTask == "" {
		return "No affected active task found. No action."
	}
	if task != nil {
		return "May affect " + task.Name + "."
	}
	return ""
}
```

- [ ] **Step 4: Run briefing tests**

Run:

```bash
go test ./internal/briefing
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/briefing/briefing.go internal/briefing/briefing_test.go
git commit -m "feat: bucket attention fyi by task impact"
```

---

### Task 5: Clarify Overview Briefing Metadata

**Files:**
- Modify: `internal/server/ui/src/screens/Overview.tsx`

- [ ] **Step 1: Update BriefingRow metadata ordering**

Replace `BriefingRow` in `internal/server/ui/src/screens/Overview.tsx` so the
action phrase appears before project/source/urgency and is not prefixed with
`action:`:

```tsx
function BriefingRow({ item, onOpen }: { item: BriefingItem; onOpen: (href: string) => void }) {
  const primary = primaryBriefingHref(item)
  const meta = [
    item.action || '',
    item.project,
    item.source,
    item.urgency,
  ].filter(Boolean).join(' · ')
  return (
    <div className="briefing-row" {...(primary ? clickable(() => onOpen(primary)) : {})}>
      <div className="briefing-row-top">
        <span className={`briefing-kind ${item.kind}`}>{item.kind}</span>
        <span className="briefing-title clip">{item.title}</span>
      </div>
      {meta && <div className="briefing-meta clip">{meta}</div>}
      {item.detail && <div className="briefing-detail clip">{item.detail}</div>}
      {item.links?.length ? (
        <div className="briefing-links">
          {item.links.slice(0, 4).map((link) => {
            const href = briefingLinkHref(link)
            const label = link.kind === 'source' ? 'source' : link.kind
            if (!href) return <span key={`${link.kind}:${link.target}`} className="briefing-link">{label}</span>
            if (href.startsWith('http')) {
              return <a key={`${link.kind}:${link.target}`} className="briefing-link" href={href} target="_blank" rel="noreferrer">{label}</a>
            }
            return <button key={`${link.kind}:${link.target}`} className="briefing-link" type="button" onClick={(e) => { e.stopPropagation(); onOpen(href) }}>{label}</button>
          })}
        </div>
      ) : null}
    </div>
  )
}
```

- [ ] **Step 2: Build UI**

Run:

```bash
make ui
```

Expected: Vite build succeeds and embedded assets are regenerated.

- [ ] **Step 3: Build API smoke payload**

After `make build` in Task 6, inspect `/api/overview` payload:

Run:

```bash
./flow ui serve --host 127.0.0.1 --port 8791
curl -s 'http://127.0.0.1:8791/api/overview' | jq '.briefing | {needs_action, waiting, fyi}'
```

Expected: pure digest-only item appears under `fyi` with `action: "No action"`; task-impact item appears under `waiting` or `needs_action` with a task link.

- [ ] **Step 4: Commit**

```bash
git add internal/server/ui/src/screens/Overview.tsx internal/server/static
git commit -m "fix: clarify briefing card metadata"
```

---

### Task 6: Full Verification and Smoke Test

**Files:**
- No new source files unless verification reveals a failing test.

- [ ] **Step 1: Run package tests**

Run:

```bash
go test ./internal/steering ./internal/briefing ./internal/server
```

Expected: PASS for all three packages.

- [ ] **Step 2: Run full Go suite**

Run:

```bash
go test ./...
```

Expected: PASS for every package.

- [ ] **Step 3: Build binary**

Run:

```bash
make build
```

Expected output includes:

```text
go build -ldflags '-X main.version=dev' -o flow .
```

- [ ] **Step 4: Check whitespace**

Run:

```bash
git diff --check
```

Expected: no output and exit code 0.

- [ ] **Step 5: Smoke `/api/overview` on a temporary port**

Run:

```bash
./flow ui serve --host 127.0.0.1 --port 8791
```

In another shell:

```bash
curl -s 'http://127.0.0.1:8791/api/overview' | jq '.briefing | {needs_action_count: (.needs_action | length), waiting_count: (.waiting | length), fyi_count: (.fyi | length), fyi: .fyi[0:3]}'
```

Expected: server starts, API returns briefing JSON, and FYI examples include readable `title`, `detail`, and `action`. Stop the temporary server with `Ctrl-C`. If another Flow server owns Slack Socket Mode, the temporary server may log that it is not taking the Slack connection; that is acceptable for read-only smoke.

- [ ] **Step 6: Final status**

Run:

```bash
git status --short --branch
```

Expected: only intentional committed changes, or a clean tree if no unrelated work remains. Do not revert unrelated user changes.
