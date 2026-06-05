# Attention Router — P1.1 Backend Spine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> ⚠️ **Repo rule:** the operator works on `main` and does **not** want commits without explicit approval. Treat each **Commit** step as "stage the files and pause for the operator's go-ahead" unless they've said to commit freely this session.

**Goal:** Build the pure-Go foundation of the attention-router steerer — shared triage types, the Stage 0 deterministic filter, the Attention feed store, and the surface-only autonomy gate — all validated by `go test ./...` with no network or model calls.

**Architecture:** A new `internal/steering` package holds connector-blind triage types, the Stage 0 free-filter, and the autonomy gate. The Attention feed is a new `attention_feed` table added to the existing idempotent `schemaDDL`, with CRUD in `internal/flowdb`. Nothing here talks to Slack, GitHub, or any model — those land in P1.2. This slice is the dependency root every later slice imports.

**Tech Stack:** Go (no CGO), `modernc.org/sqlite` pure-Go driver, `database/sql`, table-driven tests with real SQLite in `t.TempDir()`. Reuses `internal/monitor` (`InboundEvent`, `SelfUserIDs`, `ThreadKey`).

**Spec:** `docs/superpowers/specs/2026-06-04-attention-router-steerer-design.md` (§5 connector types, §6.3 verdict schema, §6 Stage 0, §7 feed table, §8 autonomy engine).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/steering/types.go` (create) | Shared triage types: `Action`, `Urgency`, `Verdict`, `ThreadContext`, `OperatorIdentity`, `Connector` interface. The connector-blind vocabulary every later slice imports. |
| `internal/steering/types_test.go` (create) | `ParseAction` validation + `Verdict` JSON round-trip. |
| `internal/steering/stage0.go` (create) | `WatchConfig` + `Stage0()` — the free deterministic filter (scope/self/bot/mute) and the coalescing thread key. Pure functions, no I/O. |
| `internal/steering/stage0_test.go` (create) | Table-driven Stage 0 drop/pass cases. |
| `internal/steering/autonomy.go` (create) | `ActionPolicy`, `AutonomyPolicy`, `DefaultAutonomy()` (all off), `Allow(action, confidence)`. |
| `internal/steering/autonomy_test.go` (create) | `Allow` truth table. |
| `internal/flowdb/attention.go` (create) | `FeedItem` model + `UpsertFeedItem` (coalesce by thread_key), `ListFeedItems`, `SetFeedItemStatus`. |
| `internal/flowdb/attention_test.go` (create) | Feed CRUD + coalescing against real SQLite. |
| `internal/flowdb/db.go` (modify: `schemaDDL` const, ~line 134) | Add the `attention_feed` table + index to the idempotent DDL. |

---

## Task 1: Shared triage types

**Files:**
- Create: `internal/steering/types.go`
- Test: `internal/steering/types_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/steering/types_test.go
package steering

import (
	"encoding/json"
	"testing"
)

func TestParseAction(t *testing.T) {
	cases := []struct {
		in     string
		want   Action
		wantOK bool
	}{
		{"make_task", ActionMakeTask, true},
		{"forward", ActionForward, true},
		{"reply", ActionReply, true},
		{"afk_reply", ActionAFKReply, true},
		{"digest_only", ActionDigestOnly, true},
		{"drop", ActionDrop, true},
		{"MAKE_TASK", ActionMakeTask, true}, // case-insensitive
		{"  forward  ", ActionForward, true}, // trimmed
		{"nonsense", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ParseAction(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseAction(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestVerdictJSONRoundTrip(t *testing.T) {
	in := Verdict{
		Source:            "slack",
		ThreadKey:         "C123:1700000000.000100",
		SuggestedAction:   ActionMakeTask,
		MatchedTask:       "kong-split",
		SuggestedProject:  "goniyo",
		SuggestedPriority: "high",
		Urgency:           UrgencyUrgent,
		IsVIP:             true,
		Confidence:        0.91,
		Summary:           "Customer asks for rollout date",
		Draft:             "On it — targeting Friday.",
		Reason:            "names operator + question mark",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Verdict
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/steering/ -run 'TestParseAction|TestVerdictJSONRoundTrip' -v`
Expected: build failure — `undefined: Action`, `undefined: Verdict`, `undefined: ParseAction`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/steering/types.go

// Package steering holds the connector-blind attention-router ("steerer")
// triage layer: shared types, the Stage 0 deterministic filter, and the
// autonomy gate. It observes incoming events across pluggable connectors,
// triages them cheap-to-expensive, and surfaces candidates for the operator.
package steering

import (
	"context"
	"strings"

	"flow/internal/monitor"
)

// Action is a triage outcome the steerer can take or surface to the operator.
type Action string

const (
	ActionMakeTask   Action = "make_task"
	ActionForward    Action = "forward"
	ActionReply      Action = "reply"
	ActionAFKReply   Action = "afk_reply"
	ActionDigestOnly Action = "digest_only"
	ActionDrop       Action = "drop"
)

// ParseAction parses a triage action string (trimmed, case-insensitive).
// Returns ok=false for unknown values so classifier output can be validated
// before it drives any side effect.
func ParseAction(s string) (Action, bool) {
	switch Action(strings.ToLower(strings.TrimSpace(s))) {
	case ActionMakeTask:
		return ActionMakeTask, true
	case ActionForward:
		return ActionForward, true
	case ActionReply:
		return ActionReply, true
	case ActionAFKReply:
		return ActionAFKReply, true
	case ActionDigestOnly:
		return ActionDigestOnly, true
	case ActionDrop:
		return ActionDrop, true
	default:
		return "", false
	}
}

// Urgency is the steerer's coarse time-sensitivity bucket.
type Urgency string

const (
	UrgencyUrgent Urgency = "urgent"
	UrgencyNormal Urgency = "normal"
	UrgencyLow    Urgency = "low"
)

// Verdict is the structured triage output (Stage 2 router / Stage 3 deep
// agent). It is connector-blind and is what populates the Attention feed.
// See spec §6.3.
type Verdict struct {
	Source            string  `json:"source"`
	ThreadKey         string  `json:"thread_key"`
	SuggestedAction   Action  `json:"suggested_action"`
	MatchedTask       string  `json:"matched_task,omitempty"`
	SuggestedProject  string  `json:"suggested_project,omitempty"`
	SuggestedPriority string  `json:"suggested_priority,omitempty"`
	Urgency           Urgency `json:"urgency,omitempty"`
	IsVIP             bool    `json:"is_vip,omitempty"`
	Confidence        float64 `json:"confidence"`
	Summary           string  `json:"summary,omitempty"`
	Draft             string  `json:"draft,omitempty"`
	Reason            string  `json:"reason,omitempty"`
}

// OperatorIdentity is the set of identifiers that count as "the operator" on
// a connector (Slack user IDs, GitHub logins, email addresses). Stage 0 uses
// it to drop self-authored events.
type OperatorIdentity struct {
	UserIDs []string
}

// ContextMessage is one message inside a fetched thread.
type ContextMessage struct {
	Author string `json:"author"`
	Text   string `json:"text"`
	TS     string `json:"ts"`
}

// ThreadContext is a normalized bundle of richer context a connector fetches
// on demand for the deep triage stage.
type ThreadContext struct {
	Summary      string           `json:"summary,omitempty"`
	Participants []string         `json:"participants,omitempty"`
	Messages     []ContextMessage `json:"messages,omitempty"`
	Permalink    string           `json:"permalink,omitempty"`
}

// Connector abstracts a monitored source (slack, github, gmail). The cascade,
// feed, and actions never know which connector an item came from. SendReply
// is invoked ONLY by the autonomy gate — never by triage code (spec §5).
type Connector interface {
	Name() string
	Events(ctx context.Context) <-chan monitor.InboundEvent
	FetchContext(ctx context.Context, ev monitor.InboundEvent) (ThreadContext, error)
	SendReply(ctx context.Context, ev monitor.InboundEvent, text string) error
	Identity() OperatorIdentity
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/steering/ -run 'TestParseAction|TestVerdictJSONRoundTrip' -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit** (stage + pause per repo rule)

```bash
git add internal/steering/types.go internal/steering/types_test.go
git commit -m "feat(steering): connector-blind triage types (Action, Verdict, Connector)"
```

---

## Task 2: Stage 0 deterministic filter

The free, no-LLM gate (spec §6, Stage 0). Pure functions: scope check (DMs + mentions + watched channels), self/bot drop, mute rules, and the coalescing thread key. Cross-pipeline dedup against the DB is deferred to P1.2 where DB access exists; this task is the pure-rule core.

**Files:**
- Create: `internal/steering/stage0.go`
- Test: `internal/steering/stage0_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/steering/stage0_test.go
package steering

import (
	"testing"

	"flow/internal/monitor"
)

func baseCfg() WatchConfig {
	return WatchConfig{
		WatchedChannels: map[string]bool{"C_WATCHED": true},
		MutedChannels:   map[string]bool{"C_MUTED": true},
		MutedKeywords:   []string{"lunch"},
		Identity:        OperatorIdentity{UserIDs: []string{"U_ME"}},
		MentionUserIDs:  []string{"U_ME"},
	}
}

func TestStage0(t *testing.T) {
	cases := []struct {
		name     string
		ev       monitor.InboundEvent
		wantPass bool
		wantKey  string
	}{
		{
			name:     "dm passes",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", TS: "1.1", ThreadTS: "1.1", UserID: "U_OTHER", Text: "hey"},
			wantPass: true, wantKey: "D1:1.1",
		},
		{
			name:     "watched channel passes",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_WATCHED", TS: "2.1", ThreadTS: "2.0", UserID: "U_OTHER", Text: "ship it"},
			wantPass: true, wantKey: "C_WATCHED:2.0",
		},
		{
			name:     "app_mention in unwatched channel passes (mention)",
			ev:       monitor.InboundEvent{Kind: "app_mention", ChannelType: "channel", Channel: "C_OTHER", TS: "3.1", ThreadTS: "3.1", UserID: "U_OTHER", Text: "<@U_ME> ping"},
			wantPass: true, wantKey: "C_OTHER:3.1",
		},
		{
			name:     "text mention in unwatched channel passes",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_OTHER", TS: "3.5", ThreadTS: "3.5", UserID: "U_OTHER", Text: "cc <@U_ME> please look"},
			wantPass: true, wantKey: "C_OTHER:3.5",
		},
		{
			name:     "unwatched channel no mention drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_OTHER", TS: "4.1", ThreadTS: "4.0", UserID: "U_OTHER", Text: "random chatter"},
			wantPass: false,
		},
		{
			name:     "self-authored drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", TS: "5.1", ThreadTS: "5.1", UserID: "U_ME", Text: "note to self"},
			wantPass: false,
		},
		{
			name:     "empty user (bot/system) drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_WATCHED", TS: "6.1", ThreadTS: "6.1", UserID: "", Text: "deploy ok"},
			wantPass: false,
		},
		{
			name:     "muted channel drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_MUTED", TS: "7.1", ThreadTS: "7.1", UserID: "U_OTHER", Text: "hi"},
			wantPass: false,
		},
		{
			name:     "muted keyword drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", TS: "8.1", ThreadTS: "8.1", UserID: "U_OTHER", Text: "Going for LUNCH?"},
			wantPass: false,
		},
		{
			name:     "reaction kind drops (belongs to reaction pipeline)",
			ev:       monitor.InboundEvent{Kind: "reaction_added", Channel: "C_WATCHED", TS: "9.1", ThreadTS: "9.0", UserID: "U_OTHER", Reaction: "eyes"},
			wantPass: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Stage0(c.ev, baseCfg())
			if got.Pass != c.wantPass {
				t.Fatalf("Pass = %v (reason %q), want %v", got.Pass, got.DropReason, c.wantPass)
			}
			if c.wantPass && got.ThreadKey != c.wantKey {
				t.Errorf("ThreadKey = %q, want %q", got.ThreadKey, c.wantKey)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/steering/ -run TestStage0 -v`
Expected: build failure — `undefined: WatchConfig`, `undefined: Stage0`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/steering/stage0.go
package steering

import (
	"strings"

	"flow/internal/monitor"
)

// WatchConfig is the operator's Stage 0 configuration (spec §10). Channel sets
// are maps for O(1) membership. Identity drives the self-drop; MentionUserIDs
// drives "is this addressed to me" detection in otherwise-unwatched channels.
type WatchConfig struct {
	WatchedChannels map[string]bool
	MutedChannels   map[string]bool
	MutedKeywords   []string
	Identity        OperatorIdentity
	MentionUserIDs  []string
}

// Stage0Result is the outcome of the free deterministic filter.
type Stage0Result struct {
	Pass       bool
	DropReason string // non-empty when Pass == false (for the explainability log)
	ThreadKey  string // channel:thread_ts coalescing key, set when Pass == true
}

// Stage0 applies the no-LLM drop rules (spec §6, Stage 0). It only considers
// human chat events ("message"/"app_mention"); reactions belong to the
// existing reaction-trigger pipeline and are dropped here. Order: kind →
// self → bot → mute → scope. The returned ThreadKey is the coalescing key.
func Stage0(ev monitor.InboundEvent, cfg WatchConfig) Stage0Result {
	if ev.Kind != "message" && ev.Kind != "app_mention" {
		return Stage0Result{DropReason: "not a chat event"}
	}
	if containsFold(cfg.Identity.UserIDs, ev.UserID) {
		return Stage0Result{DropReason: "self-authored"}
	}
	if strings.TrimSpace(ev.UserID) == "" {
		return Stage0Result{DropReason: "system/bot (no user)"}
	}
	if cfg.MutedChannels[ev.Channel] {
		return Stage0Result{DropReason: "muted channel"}
	}
	if hasMutedKeyword(ev.Text, cfg.MutedKeywords) {
		return Stage0Result{DropReason: "muted keyword"}
	}
	if !inScope(ev, cfg) {
		return Stage0Result{DropReason: "out of scope"}
	}
	key := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	if key == "" {
		return Stage0Result{DropReason: "no thread key"}
	}
	return Stage0Result{Pass: true, ThreadKey: key}
}

// inScope passes DMs/MPIMs, anything that mentions the operator, and messages
// in watched channels.
func inScope(ev monitor.InboundEvent, cfg WatchConfig) bool {
	if ev.ChannelType == "im" || ev.ChannelType == "mpim" {
		return true
	}
	if ev.Kind == "app_mention" || mentionsOperator(ev.Text, cfg.MentionUserIDs) {
		return true
	}
	return cfg.WatchedChannels[ev.Channel]
}

// mentionsOperator reports whether text contains a Slack-style <@UID> mention
// for any of the operator's user IDs.
func mentionsOperator(text string, ids []string) bool {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && strings.Contains(text, "<@"+id+">") {
			return true
		}
	}
	return false
}

func hasMutedKeyword(text string, keywords []string) bool {
	lower := strings.ToLower(text)
	for _, k := range keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func containsFold(haystack []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, h := range haystack {
		if strings.EqualFold(strings.TrimSpace(h), needle) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/steering/ -run TestStage0 -v`
Expected: PASS (all sub-cases).

- [ ] **Step 5: Commit** (stage + pause per repo rule)

```bash
git add internal/steering/stage0.go internal/steering/stage0_test.go
git commit -m "feat(steering): Stage 0 deterministic filter (scope/self/bot/mute)"
```

---

## Task 3: Attention feed store

Adds the `attention_feed` table to the existing idempotent `schemaDDL` and the CRUD that the cascade (P1.2) and UI (P1.4) consume. `UpsertFeedItem` coalesces by `thread_key`: a new candidate for a thread that already has a `new` card updates that card instead of stacking duplicates (spec §7).

**Files:**
- Modify: `internal/flowdb/db.go` (`schemaDDL` const — insert the table before the closing backtick at ~line 206; add the index alongside the other `CREATE INDEX` lines)
- Create: `internal/flowdb/attention.go`
- Test: `internal/flowdb/attention_test.go`

- [ ] **Step 1: Add the table to `schemaDDL`**

In `internal/flowdb/db.go`, inside the `schemaDDL` raw string, add this block immediately after the `search_docs` table definition (after line 145, before the `CREATE VIRTUAL TABLE` block):

```sql
CREATE TABLE IF NOT EXISTS attention_feed (
    id                 TEXT PRIMARY KEY,
    source             TEXT NOT NULL,
    thread_key         TEXT NOT NULL,
    summary            TEXT NOT NULL DEFAULT '',
    suggested_action   TEXT NOT NULL,
    matched_task       TEXT,
    suggested_project  TEXT,
    suggested_priority TEXT,
    urgency            TEXT,
    is_vip             INTEGER NOT NULL DEFAULT 0,
    confidence         REAL NOT NULL DEFAULT 0,
    draft              TEXT,
    reason             TEXT,
    context_json       TEXT,
    status             TEXT NOT NULL DEFAULT 'new' CHECK (status IN ('new','acted','dismissed','snoozed','deferred')),
    snooze_until       TEXT,
    created_at         TEXT NOT NULL,
    acted_at           TEXT
);
```

Then add these two indexes to the `CREATE INDEX` block (after line 206, before the closing backtick of `schemaDDL`):

```sql
CREATE INDEX IF NOT EXISTS idx_attention_feed_status ON attention_feed(status);
CREATE INDEX IF NOT EXISTS idx_attention_feed_thread ON attention_feed(thread_key);
```

- [ ] **Step 2: Write the failing test**

```go
// internal/flowdb/attention_test.go
package flowdb

import "testing"

func TestAttentionFeedInsertAndList(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	item := FeedItem{
		ID:              "f1",
		Source:          "slack",
		ThreadKey:       "C1:100.1",
		Summary:         "Customer asks for rollout date",
		SuggestedAction: "make_task",
		MatchedTask:     "kong-split",
		Urgency:         "urgent",
		IsVIP:           true,
		Confidence:      0.9,
		Draft:           "On it.",
		Reason:          "names operator",
		ContextJSON:     `{"k":"v"}`,
		Status:          "new",
		CreatedAt:       "2026-06-05T10:00:00Z",
	}
	id, err := UpsertFeedItem(db, item)
	if err != nil {
		t.Fatalf("UpsertFeedItem: %v", err)
	}
	if id != "f1" {
		t.Fatalf("id = %q, want f1", id)
	}

	got, err := ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].ID != "f1" || got[0].MatchedTask != "kong-split" || !got[0].IsVIP || got[0].Confidence != 0.9 {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
}

func TestAttentionFeedCoalescesByThreadKey(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	first := FeedItem{ID: "a", Source: "slack", ThreadKey: "C1:200.1", SuggestedAction: "reply", Summary: "first", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := UpsertFeedItem(db, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Second candidate for the SAME thread while the card is still 'new'
	// must update the existing row, not create a second one.
	second := FeedItem{ID: "b", Source: "slack", ThreadKey: "C1:200.1", SuggestedAction: "make_task", Summary: "updated", Status: "new", CreatedAt: "2026-06-05T10:05:00Z"}
	id, err := UpsertFeedItem(db, second)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id != "a" {
		t.Errorf("coalesced id = %q, want existing id a", id)
	}

	got, err := ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (coalesced)", len(got))
	}
	if got[0].Summary != "updated" || got[0].SuggestedAction != "make_task" {
		t.Errorf("expected coalesced row to carry new fields, got %+v", got[0])
	}
}

func TestAttentionFeedSetStatus(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	if _, err := UpsertFeedItem(db, FeedItem{ID: "x", Source: "slack", ThreadKey: "C1:300.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := SetFeedItemStatus(db, "x", "dismissed", "2026-06-05T11:00:00Z"); err != nil {
		t.Fatalf("SetFeedItemStatus: %v", err)
	}
	if n, _ := ListFeedItems(db, "new"); len(n) != 0 {
		t.Errorf("new count = %d, want 0", len(n))
	}
	d, _ := ListFeedItems(db, "dismissed")
	if len(d) != 1 || d[0].ActedAt != "2026-06-05T11:00:00Z" {
		t.Errorf("dismissed = %+v", d)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/flowdb/ -run TestAttentionFeed -v`
Expected: build failure — `undefined: FeedItem`, `undefined: UpsertFeedItem`, etc.

- [ ] **Step 4: Write minimal implementation**

```go
// internal/flowdb/attention.go
package flowdb

import (
	"database/sql"
	"fmt"
)

// FeedItem mirrors a row in the attention_feed table (spec §7). It is the
// durable record of one triage candidate surfaced to the operator.
type FeedItem struct {
	ID                string
	Source            string
	ThreadKey         string
	Summary           string
	SuggestedAction   string
	MatchedTask       string
	SuggestedProject  string
	SuggestedPriority string
	Urgency           string
	IsVIP             bool
	Confidence        float64
	Draft             string
	Reason            string
	ContextJSON       string
	Status            string // new|acted|dismissed|snoozed|deferred
	SnoozeUntil       string
	CreatedAt         string // RFC3339
	ActedAt           string // RFC3339, set when status leaves 'new'
}

// UpsertFeedItem inserts a feed item, coalescing by thread_key: if a row for
// the same thread_key already exists with status 'new', that row is updated
// in place (and its existing id is returned) instead of creating a duplicate
// card. Otherwise the item is inserted as given. Returns the id of the row
// written.
func UpsertFeedItem(db *sql.DB, item FeedItem) (string, error) {
	if item.ID == "" || item.ThreadKey == "" || item.SuggestedAction == "" {
		return "", fmt.Errorf("flowdb: feed item requires id, thread_key, suggested_action")
	}
	if item.Status == "" {
		item.Status = "new"
	}

	var existingID string
	err := db.QueryRow(
		`SELECT id FROM attention_feed WHERE thread_key = ? AND status = 'new' LIMIT 1`,
		item.ThreadKey,
	).Scan(&existingID)
	switch {
	case err == sql.ErrNoRows:
		// fall through to insert
	case err != nil:
		return "", fmt.Errorf("flowdb: lookup feed coalesce: %w", err)
	default:
		// Coalesce into the existing 'new' row.
		_, uerr := db.Exec(
			`UPDATE attention_feed SET
			   source=?, summary=?, suggested_action=?, matched_task=?,
			   suggested_project=?, suggested_priority=?, urgency=?, is_vip=?,
			   confidence=?, draft=?, reason=?, context_json=?
			 WHERE id=?`,
			item.Source, item.Summary, item.SuggestedAction, nullify(item.MatchedTask),
			nullify(item.SuggestedProject), nullify(item.SuggestedPriority), nullify(item.Urgency), boolToInt(item.IsVIP),
			item.Confidence, nullify(item.Draft), nullify(item.Reason), nullify(item.ContextJSON),
			existingID,
		)
		if uerr != nil {
			return "", fmt.Errorf("flowdb: coalesce feed item: %w", uerr)
		}
		return existingID, nil
	}

	_, err = db.Exec(
		`INSERT INTO attention_feed (
		   id, source, thread_key, summary, suggested_action, matched_task,
		   suggested_project, suggested_priority, urgency, is_vip, confidence,
		   draft, reason, context_json, status, snooze_until, created_at, acted_at
		 ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, item.Source, item.ThreadKey, item.Summary, item.SuggestedAction, nullify(item.MatchedTask),
		nullify(item.SuggestedProject), nullify(item.SuggestedPriority), nullify(item.Urgency), boolToInt(item.IsVIP), item.Confidence,
		nullify(item.Draft), nullify(item.Reason), nullify(item.ContextJSON), item.Status, nullify(item.SnoozeUntil), item.CreatedAt, nullify(item.ActedAt),
	)
	if err != nil {
		return "", fmt.Errorf("flowdb: insert feed item: %w", err)
	}
	return item.ID, nil
}

// ListFeedItems returns feed rows, newest first. An empty status returns all
// rows; otherwise it filters to that status.
func ListFeedItems(db *sql.DB, status string) ([]FeedItem, error) {
	q := `SELECT id, source, thread_key, summary, suggested_action, matched_task,
	             suggested_project, suggested_priority, urgency, is_vip, confidence,
	             draft, reason, context_json, status, snooze_until, created_at, acted_at
	      FROM attention_feed`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, id DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("flowdb: list feed items: %w", err)
	}
	defer rows.Close()

	var out []FeedItem
	for rows.Next() {
		var it FeedItem
		var matched, project, priority, urgency, draft, reason, ctx, snooze, acted sql.NullString
		var isVIP int
		if err := rows.Scan(
			&it.ID, &it.Source, &it.ThreadKey, &it.Summary, &it.SuggestedAction, &matched,
			&project, &priority, &urgency, &isVIP, &it.Confidence,
			&draft, &reason, &ctx, &it.Status, &snooze, &it.CreatedAt, &acted,
		); err != nil {
			return nil, fmt.Errorf("flowdb: scan feed item: %w", err)
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
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetFeedItemStatus moves a feed item to a new lifecycle status and stamps
// acted_at. Used when the operator (or an autonomous action) resolves a card.
func SetFeedItemStatus(db *sql.DB, id, status, actedAt string) error {
	res, err := db.Exec(
		`UPDATE attention_feed SET status = ?, acted_at = ? WHERE id = ?`,
		status, nullify(actedAt), id,
	)
	if err != nil {
		return fmt.Errorf("flowdb: set feed status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("flowdb: no feed item with id %q", id)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullify maps "" to a NULL so empty optional columns store as NULL rather
// than empty-string (keeps WHERE ... IS NULL semantics clean and matches the
// rest of the schema's optional-column convention).
func nullify(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

> **Note for the executor:** if `boolToInt` or `nullify` already exist in
> package `flowdb` (grep first: `grep -nE "func boolToInt|func nullify" internal/flowdb/*.go`),
> drop the duplicate definitions from `attention.go` and reuse the existing
> ones — a redeclaration won't compile.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/flowdb/ -run TestAttentionFeed -v`
Expected: PASS (all three tests).

- [ ] **Step 6: Run the full flowdb suite (schema change regression check)**

Run: `go test ./internal/flowdb/`
Expected: PASS — confirms the new table in `schemaDDL` didn't break `TestOpenDBCreatesSchema` / `TestOpenDBIdempotent`.

- [ ] **Step 7: Commit** (stage + pause per repo rule)

```bash
git add internal/flowdb/db.go internal/flowdb/attention.go internal/flowdb/attention_test.go
git commit -m "feat(flowdb): attention_feed table + coalescing CRUD"
```

---

## Task 4: Autonomy gate (surface-only default)

The per-action policy gate (spec §8). In P1 every action defaults **off** (surface-only); the engine exists so P1.2/P1.3 can route through it and P2 can flip actions on. `Allow` is the single chokepoint every outward effect must pass.

**Files:**
- Create: `internal/steering/autonomy.go`
- Test: `internal/steering/autonomy_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/steering/autonomy_test.go
package steering

import "testing"

func TestDefaultAutonomyIsSurfaceOnly(t *testing.T) {
	p := DefaultAutonomy()
	for _, a := range []Action{ActionMakeTask, ActionForward, ActionReply, ActionAFKReply} {
		if p.Allow(a, 1.0) {
			t.Errorf("DefaultAutonomy allowed %q at confidence 1.0; want surface-only (deny)", a)
		}
	}
}

func TestAutonomyAllow(t *testing.T) {
	p := AutonomyPolicy{
		ActionForward:  {Enabled: true, Threshold: 0.85},
		ActionAFKReply: {Enabled: false, Threshold: 0.90},
	}
	cases := []struct {
		action     Action
		confidence float64
		want       bool
	}{
		{ActionForward, 0.90, true},   // enabled + over threshold
		{ActionForward, 0.85, true},   // exactly at threshold passes
		{ActionForward, 0.80, false},  // under threshold
		{ActionAFKReply, 0.99, false}, // disabled regardless of confidence
		{ActionReply, 1.0, false},     // not in policy → deny
	}
	for _, c := range cases {
		if got := p.Allow(c.action, c.confidence); got != c.want {
			t.Errorf("Allow(%q, %.2f) = %v, want %v", c.action, c.confidence, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/steering/ -run 'TestDefaultAutonomyIsSurfaceOnly|TestAutonomyAllow' -v`
Expected: build failure — `undefined: DefaultAutonomy`, `undefined: AutonomyPolicy`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/steering/autonomy.go
package steering

// ActionPolicy is the operator's autonomy setting for one action: whether the
// steerer may perform it without asking, and the minimum confidence required.
type ActionPolicy struct {
	Enabled   bool    `json:"enabled"`
	Threshold float64 `json:"threshold"`
}

// AutonomyPolicy maps each action to its policy. A missing action is treated
// as disabled (deny). See spec §8.
type AutonomyPolicy map[Action]ActionPolicy

// DefaultAutonomy returns the P1 posture: every action surface-only (disabled).
// The thresholds are pre-seeded with the spec's defaults so the P2 settings UI
// has sensible starting values when an action is later enabled.
func DefaultAutonomy() AutonomyPolicy {
	return AutonomyPolicy{
		ActionForward:  {Enabled: false, Threshold: 0.85},
		ActionAFKReply: {Enabled: false, Threshold: 0.90},
		ActionMakeTask: {Enabled: false, Threshold: 0.80},
		ActionReply:    {Enabled: false, Threshold: 0.95},
	}
}

// Allow reports whether the steerer may perform action autonomously at the
// given confidence. This is the single chokepoint every outward effect must
// pass; an action that is absent or disabled is always denied, so triage code
// can never act on its own unless the operator opted in.
func (p AutonomyPolicy) Allow(action Action, confidence float64) bool {
	pol, ok := p[action]
	if !ok || !pol.Enabled {
		return false
	}
	return confidence >= pol.Threshold
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/steering/ -run 'TestDefaultAutonomyIsSurfaceOnly|TestAutonomyAllow' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole package + module suite**

Run: `go test ./internal/steering/ && go test ./...`
Expected: PASS across the board (new package green; nothing else regressed).

- [ ] **Step 6: Commit** (stage + pause per repo rule)

```bash
git add internal/steering/autonomy.go internal/steering/autonomy_test.go
git commit -m "feat(steering): autonomy gate (surface-only default + Allow chokepoint)"
```

---

## Self-Review

**1. Spec coverage (P1.1 scope only):**
- §5 connector types (`Connector`, `ThreadContext`, `OperatorIdentity`) → Task 1. ✅
- §6.3 verdict schema (`Verdict`) → Task 1. ✅
- §6 Stage 0 free filter (scope/self/bot/mute/coalescing key) → Task 2. ✅
- §7 Attention feed table + coalescing CRUD → Task 3. ✅
- §8 autonomy engine (policy table, `Allow`, surface-only default) → Task 4. ✅
- *Deferred to later P1 slices (correctly out of P1.1):* Stage 1/2 classifier, cascade orchestration, Slack adapter, dispatcher routing, deep triage worker (P1.2); action handlers (P1.3); settings + MC UI + push (P1.4); cross-pipeline DB dedup (P1.2, needs DB+cascade context).

**2. Placeholder scan:** No TBD/TODO/"handle edge cases"/"similar to Task N". Every code step contains complete, compilable code and every run step has an exact command + expected outcome.

**3. Type consistency:**
- `Action` constants (`ActionMakeTask`, `ActionForward`, `ActionReply`, `ActionAFKReply`, `ActionDigestOnly`, `ActionDrop`) defined in Task 1 and reused verbatim in Tasks 2/4. ✅
- `Verdict` fields (Task 1) ↔ `FeedItem` columns (Task 3): both carry source/thread_key/suggested_action/matched_task/suggested_project/suggested_priority/urgency/is_vip/confidence/draft/reason; `FeedItem` adds storage-only fields (id/context_json/status/snooze_until/created_at/acted_at). Consistent by design — the cascade in P1.2 maps `Verdict`→`FeedItem`. ✅
- `OperatorIdentity.UserIDs` used by `WatchConfig.Identity` (Task 2). ✅
- `AutonomyPolicy.Allow(Action, float64) bool` signature matches its tests. ✅
- Reused external symbols verified against source: `monitor.InboundEvent` (fields Kind/Channel/ChannelType/TS/ThreadTS/UserID/Text — `inbound_event.go:29`), `monitor.ThreadKey` (`reaction_trigger.go:117`), `openTempDB(t)` test helper (`db_test.go:10`), `schemaDDL` idempotent CREATE pattern (`db.go:26`). ✅
- Potential collision flagged: `boolToInt`/`nullify` helpers (Task 3 Step 4 note instructs grep-and-reuse if they already exist in `flowdb`). ✅

No issues found that require rework.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-06-05-attention-router-p1.1-backend-spine.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
