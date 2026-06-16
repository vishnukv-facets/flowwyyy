# Per-Channel Steerer Session Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the stateless per-event `claude -p` deep-triage with one long-running, per-channel Claude session (modeled as a `chat`) that holds conversation memory, decides routing/grouping/drafting, and surfaces attention cards via a tool — collapsing the clubbing/thread-state/incremental machinery.

**Architecture:** Per-channel session = a `chats` row + a detached floating Claude session (default Opus), keyed by a deterministic `chat-steer-<key>` slug, reusing `chat_sink.go`/`kbDistiller`/the token panel/`wakeTask`. The cascade runs Stage 0, resolves the session key, and delivers survivors into the session through an injected `SteererSessionSink` (steering→server boundary). The session calls `flow attention surface` → `steering.SurfaceCard` (validate thread_key, autonomy gate, write feed). A `/compact` worker and Claude→Codex provider fork bound context/usage. Shipped behind `FLOW_STEERING_SESSIONS`, with the old cold-call path as the live fallback until the new path proves out.

**Tech Stack:** Go (`modernc.org/sqlite`, no CGO), `flag.FlagSet`, Vite+React+TS UI, Claude Code / Codex CLIs via floating PTY sessions.

**Spec:** `docs/superpowers/specs/2026-06-16-attention-steerer-per-channel-session-design.md` (LOCKED).

---

## Plan structure (multi-phase — read this first)

The spec spans 6 subsystems and 14 work items (GAP-1..14). The rollout defines 6
independently shippable, behavior-safe increments. Per the writing-plans scope rule,
**each phase is its own plan**; this file fully details **Phase 1** and maps the rest.
Subsequent phases get their own `docs/superpowers/plans/` file when picked up, because
their step-level code depends on symbols earlier phases create (writing it all now would
be speculative). Each phase ends green (`go build ./... && go test ./...`) and adds no
default behavior until Phase 2 flips `FLOW_STEERING_SESSIONS`.

| Phase | Rollout step | Gaps | Ships |
|-------|--------------|------|-------|
| **1** | Surface tool | GAP-3 (+4 validation, +7 trace seed) | `flow attention surface` → `SurfaceCard`; no behavior change |
| **2** | Session model + wiring | GAP-1,5,8,10 | `SteererSessionSink`, launcher, `Cascade.Observe` delivery, self-authored feed — behind `FLOW_STEERING_SESSIONS` |
| **3** | Context bound | GAP-2,6 | `--thread-ts` send; `/compact` occupancy worker |
| **4** | Slack validation | — | Enable for Slack; validate coinswitch cases in Trace |
| **5** | GitHub + provider + UI + accounting | GAP-9,11,12,13,14 | PR/issue grain (canonical), provider fork, settings, token/cost, rename, deletion |
| **6** | Cleanup | (Code cleanup §) | Delete clubbing.go etc. once default-on |

## File map

**Phase 1 (this plan):**
- Create: `internal/steering/surface.go` — `SurfaceCard` (validate + autonomy gate + write feed).
- Create: `internal/steering/surface_test.go` — unit tests.
- Modify: `internal/app/attention.go` — add `surface` subcommand + `cmdAttentionSurface`.
- Modify: `internal/app/attention_test.go` (or create) — CLI test.

**Later phases (mapped, detailed at pickup):**
- `internal/steering/session_sink.go` — `SteererSessionSink` interface + key resolution (GAP-1,4,8).
- `internal/server/steerer_session.go` — sink impl: launcher, lifecycle/idle-sleep, slot state, fork (GAP-1,5,9).
- `internal/server/steerer_compact.go` — `/compact` occupancy worker (GAP-6).
- `internal/monitor/dispatcher.go` — self-authored feed-only route (GAP-10).
- `internal/app/slack.go` + `internal/server/slack_send.go` — `--thread-ts` (GAP-2).
- `internal/flowdb/chats.go` — `SetChatTitle`; `origin="steerer"`; provider override storage (GAP-11,13).
- `internal/server/ui_data.go` + `internal/server/ui/src/screens/Chats.tsx`,`Overview.tsx`,`Settings.tsx` — token/cost, rename, naming, provider setting, card→chat link (GAP-11,12,13).

---

## Phase 1: `SurfaceCard` + `flow attention surface`

The structured exit a per-channel session uses to surface/update a card. Standalone and
side-effect-isolated: it writes a feed row exactly like `Cascade.writeFeed` does today,
but callable from the agent's CLI. No session, no wiring — fully testable now.

**Files:**
- Create: `internal/steering/surface.go`
- Create: `internal/steering/surface_test.go`
- Modify: `internal/app/attention.go` (add `case "surface"` + `cmdAttentionSurface`)

Grounded signatures (verified): `ApplyAction(ctx, db, item, action, autonomy, manual)` (`actions.go:772`), `DefaultAutonomy()` (`autonomy.go:135`), `flowdb.ListOpenClubCandidates(db, channel, excludeThreadKey, since, limit)`, `flowdb.UpsertFeedItemSurfaced(db, item) (id string, surfaced bool, err error)`, `flowdb.RecordThreadDecision(db, ThreadDecision{...})`, `anchorIndex(anchors, threadKey)` (`clubbing.go`), `flowdb.NowISO()`.

- [ ] **Step 1: Write the failing test for thread_key validation**

Create `internal/steering/surface_test.go`:

```go
package steering

import (
	"context"
	"testing"

	"flow/internal/flowdb"
)

// A proposed thread_key that does NOT match any open card in the channel must NOT
// merge — SurfaceCard falls back to the message's own raw key (opens a fresh card).
func TestSurfaceCardRejectsForeignThreadKey(t *testing.T) {
	db := newTestDB(t) // existing steering test helper (see triage_test.go / actions_test.go)
	ctx := context.Background()

	id, surfaced, err := SurfaceCard(ctx, db, SurfaceCardParams{
		Source: "slack", Channel: "C1", ChannelType: "channel",
		TS: "100.1", ThreadKey: "C1:999.9", // proposes a key with no open card
		Action: "digest_only", Summary: "hello", Confidence: 0.5,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced || id == "" {
		t.Fatalf("want a surfaced card, got surfaced=%v id=%q", surfaced, id)
	}
	got, err := flowdb.GetFeedItem(db, id)
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.ThreadKey != "C1:100.1" {
		t.Errorf("foreign key should fall back to raw C1:100.1, got %q", got.ThreadKey)
	}
}

// A proposed thread_key that DOES match an open card merges into it (the coinswitch case).
func TestSurfaceCardMergesIntoOpenCard(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// Seed an open card under C1:50.0.
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "a1", Source: "slack", ThreadKey: "C1:50.0", SuggestedAction: "digest_only",
		Summary: "repo access for dynamodb", Channel: "C1", ChannelType: "channel",
		Author: "U1", TS: "50.0", Status: "new", CreatedAt: flowdb.NowISO(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, surfaced, err := SurfaceCard(ctx, db, SurfaceCardParams{
		Source: "slack", Channel: "C1", ChannelType: "channel",
		TS: "60.0", ThreadKey: "C1:50.0", // proposes the open card's key
		Action: "digest_only", Summary: "list the repo names", Confidence: 0.5,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced {
		t.Fatalf("want surfaced")
	}
	if id != "a1" {
		t.Errorf("want merge into existing card a1, got new id %q", id)
	}
}

// context_only (operator-self / bot-echo) updates memory only — never surfaces a card.
func TestSurfaceCardContextOnlyDoesNotSurface(t *testing.T) {
	db := newTestDB(t)
	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source: "slack", Channel: "C1", ChannelType: "channel",
		TS: "70.0", Action: "digest_only", Summary: "operator's own note", ContextOnly: true,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if surfaced || id != "" {
		t.Errorf("context_only must not surface, got surfaced=%v id=%q", surfaced, id)
	}
}
```

- [ ] **Step 2: Run the test — verify it fails to compile**

Run: `go test ./internal/steering/ -run TestSurfaceCard -v`
Expected: FAIL — `undefined: SurfaceCard`, `undefined: SurfaceCardParams` (and confirm `newTestDB`/`flowdb.GetFeedItem` exist; if `GetFeedItem` is absent, use `flowdb.ListFeedItems(db,"new")` and find by id in the test).

- [ ] **Step 3: Implement `SurfaceCard`**

Create `internal/steering/surface.go`:

```go
// internal/steering/surface.go
package steering

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"flow/internal/flowdb"
)

// SurfaceCardParams is the structured verdict a per-channel steerer session emits via
// `flow attention surface`. It carries what Verdict carried, minus the context-fetch
// internals the live session now owns.
type SurfaceCardParams struct {
	Source      string // "slack" | "github"
	Channel     string
	ChannelType string
	ThreadKey   string // the key the session PROPOSES to continue (validated below)
	TS          string
	ThreadTS    string
	Author      string
	Action      string // make_task|capture_kb|forward|reply|digest_only|drop
	MatchedTask string
	Summary     string
	Draft       string
	Confidence  float64
	Reason      string
	ContextOnly bool // operator-self / bot-echo: record memory, never surface a card
}

// surfaceClubWindow bounds the open-card lookup used to validate a proposed merge.
const surfaceClubWindow = 12 * time.Hour

// SurfaceCard validates the proposed thread_key against the channel's open cards, then
// (unless context_only) writes a surface-only feed row and records the thread decision.
// Returns the feed id and whether a card surfaced. context_only and drop never surface.
// Go owns the merge decision: a proposed key that matches no open card falls back to the
// message's own raw key (channel:ts) — a model slip can't merge into a foreign card.
func SurfaceCard(ctx context.Context, db *sql.DB, p SurfaceCardParams) (string, bool, error) {
	_ = ctx
	rawKey := p.Channel + ":" + strings.TrimSpace(firstNonEmpty(p.ThreadTS, p.TS))
	key := validateProposedThreadKey(db, p, rawKey)

	v := Verdict{
		Source: p.Source, ThreadKey: key, SuggestedAction: Action(p.Action),
		MatchedTask: p.MatchedTask, Summary: p.Summary, Draft: p.Draft,
		Confidence: p.Confidence, Reason: p.Reason,
	}
	now := flowdb.NowISO()

	// context_only / drop: record the running understanding, surface nothing.
	if p.ContextOnly || Action(p.Action) == ActionDrop {
		recordThreadDecisionStandalone(db, key, v, p.TS, now)
		return "", false, nil
	}

	item := flowdb.FeedItem{
		ID:              randomUUID(),
		Source:          p.Source,
		ThreadKey:       key,
		Summary:         SanitizeOperatorText(p.Summary),
		SuggestedAction: p.Action,
		MatchedTask:     p.MatchedTask,
		Confidence:      p.Confidence,
		Draft:           SanitizeOperatorText(p.Draft),
		Reason:          SanitizeOperatorText(p.Reason),
		Channel:         p.Channel,
		ChannelType:     p.ChannelType,
		Author:          p.Author,
		TS:              p.TS,
		Status:          "new",
		CreatedAt:       now,
	}
	if item.SuggestedAction == "" {
		item.SuggestedAction = string(ActionDrop)
	}
	id, surfaced, err := flowdb.UpsertFeedItemSurfaced(db, item)
	if err != nil {
		return "", false, err
	}
	recordThreadDecisionStandalone(db, key, v, p.TS, now)
	return id, surfaced, nil
}

// validateProposedThreadKey returns p.ThreadKey only when it matches an OPEN card in the
// channel; otherwise the message's own raw key. Empty/raw proposals pass straight through.
func validateProposedThreadKey(db *sql.DB, p SurfaceCardParams, rawKey string) string {
	proposed := strings.TrimSpace(p.ThreadKey)
	if proposed == "" || proposed == rawKey {
		return rawKey
	}
	since := time.Now().Add(-surfaceClubWindow).UTC().Format(time.RFC3339)
	cands, err := flowdb.ListOpenClubCandidates(db, p.Channel, "", since, 50)
	if err != nil {
		return rawKey // fail safe: never merge into an unverifiable card
	}
	if anchorIndex(cands, proposed) >= 0 {
		return proposed
	}
	return rawKey
}

func recordThreadDecisionStandalone(db *sql.DB, key string, v Verdict, ts, now string) {
	_ = flowdb.RecordThreadDecision(db, flowdb.ThreadDecision{
		ThreadKey: key, Source: v.Source, Action: string(v.SuggestedAction),
		Confidence: v.Confidence, Reason: v.Reason,
		Summary: SanitizeOperatorText(v.Summary), LastSeenTS: ts, At: now,
	})
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
```

Note: `anchorIndex` currently lives in `clubbing.go`; it stays callable here (same
package) and relocates into this file in Phase 6 when `clubbing.go` is deleted.

- [ ] **Step 4: Run the tests — verify they pass**

Run: `go test ./internal/steering/ -run TestSurfaceCard -v`
Expected: PASS (3 tests). If `flowdb.GetFeedItem` doesn't exist, adjust the test to read via `ListFeedItems`.

- [ ] **Step 5: Commit**

```bash
git add internal/steering/surface.go internal/steering/surface_test.go
git commit -m "feat(steering): SurfaceCard — validated, autonomy-gated card surfacing for session triage"
```

- [ ] **Step 6: Write the failing CLI test**

Decide the autonomy gate placement first: surfacing a card is surface-only (no outward
action), so Phase 1 does NOT call `ApplyAction` — that runs only when the operator later
acts on the card (existing `flow attention act`). `SurfaceCard` just writes the card.
(If a future action like an auto-forward is surfaced, the gate is `ApplyAction`; out of
Phase 1 scope.)

Add to `internal/app/attention_test.go` (create if absent), mirroring existing
`cmdAttention*` tests (they set `$FLOW_ROOT` to a temp dir):

```go
func TestCmdAttentionSurface(t *testing.T) {
	withTempFlowRoot(t) // existing app-test helper that points $FLOW_ROOT at a temp dir + inits DB
	code := cmdAttention([]string{"surface",
		"--source", "slack", "--channel", "C1", "--channel-type", "channel",
		"--ts", "100.1", "--action", "digest_only", "--summary", "hi", "--confidence", "0.5"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	// The card exists under the raw key.
	db := openTestFlowDB(t)
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 || items[0].ThreadKey != "C1:100.1" {
		t.Fatalf("want 1 card under C1:100.1, got %+v", items)
	}
}
```

- [ ] **Step 7: Run it — verify failure**

Run: `go test ./internal/app/ -run TestCmdAttentionSurface -v`
Expected: FAIL — `unknown attention subcommand "surface"`.

- [ ] **Step 8: Implement `cmdAttentionSurface`**

In `internal/app/attention.go`, add `case "surface": return cmdAttentionSurface(rest)`
to the switch, update the usage/error strings to include `surface`, and add:

```go
func cmdAttentionSurface(args []string) int {
	fs := flagSet("attention surface")
	source := fs.String("source", "slack", "event source: slack|github")
	channel := fs.String("channel", "", "channel/DM/PR id")
	channelType := fs.String("channel-type", "", "channel|im|mpim|github")
	threadKey := fs.String("thread-key", "", "proposed thread_key to continue (validated)")
	ts := fs.String("ts", "", "message ts")
	threadTS := fs.String("thread-ts", "", "parent thread ts (defaults to ts)")
	author := fs.String("author", "", "author id")
	action := fs.String("action", "digest_only", "make_task|capture_kb|forward|reply|digest_only|drop")
	matchedTask := fs.String("matched-task", "", "task slug to forward to")
	summary := fs.String("summary", "", "<=140 char card summary")
	draft := fs.String("draft", "", "drafted reply, if any")
	reason := fs.String("reason", "", "why")
	confidence := fs.Float64("confidence", 0, "0..1")
	contextOnly := fs.Bool("context-only", false, "memory-only: never surface a card")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*channel) == "" || strings.TrimSpace(*ts) == "" {
		fmt.Fprintln(os.Stderr, "error: --channel and --ts are required")
		return 2
	}
	db, err := openFlowDB() // existing app DB opener
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()
	id, surfaced, err := steering.SurfaceCard(context.Background(), db, steering.SurfaceCardParams{
		Source: *source, Channel: *channel, ChannelType: *channelType, ThreadKey: *threadKey,
		TS: *ts, ThreadTS: *threadTS, Author: *author, Action: *action, MatchedTask: *matchedTask,
		Summary: *summary, Draft: *draft, Reason: *reason, Confidence: *confidence, ContextOnly: *contextOnly,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("surfaced=%v id=%s\n", surfaced, id)
	return 0
}
```

(Match the real DB-opener name used by other `cmdAttention*` funcs — check the top of
`attention.go`; it imports `flowdb` + `steering` already.)

- [ ] **Step 9: Run both test packages — verify pass**

Run: `go test ./internal/steering/ ./internal/app/ -run 'Surface' -v`
Expected: PASS.

- [ ] **Step 10: Full build + suite**

Run: `go build ./... && go test ./...`
Expected: green (the rtk wrapper may echo the pre-existing `go-m1cpu` CGO warnings — those are not errors).

- [ ] **Step 11: Commit**

```bash
git add internal/app/attention.go internal/app/attention_test.go
git commit -m "feat(app): flow attention surface — CLI verdict-out for per-channel sessions"
```

---

## Phase 2: session model + delivery wiring (mapped)

Ships behind `FLOW_STEERING_SESSIONS` (default off); old path remains default.

**Tasks (own plan at pickup):**
1. `SteererSessionSink` interface in `internal/steering/session_sink.go` + `Cascade` field; `monitor`-side injection mirroring `ChatCommandSink`. Key resolution `sessionKeyForEvent(ev)` (channel/DM/MPDM; SharedRef origin; GitHub deferred to P5). **Test:** key resolution table; Observe calls sink for a survivor.
2. Server impl `internal/server/steerer_session.go`: `DeliverToChannelSession(key, payload)` → ensure live (running→`wakeTask`; gone→resume; new→`startFloatingDetached` with `agentTerminalArgs(provider, …)` **no `--model`**, record `chats` row `origin="steerer"`), idle-sleep TTL, slot state machine. Reuse `openNewSlackChat`/`resumeSlackChat` shapes. **Test:** two events same key reuse one session; idle→sleep→resume.
3. Steerer brief (prime): routing rubric + autonomy + "call `flow attention surface …`" + `context_only` handling.
4. `Cascade.Observe` (`cascade.go:422`): when `FLOW_STEERING_SESSIONS` on, after Stage 0 build payload + `sink.DeliverToChannelSession`; else current path. **Test:** flag on routes to sink; flag off unchanged.
5. Self-authored feed-only (GAP-10): `dispatcher.go:101` + `slack_backfill.go:274` route self-authored to the sink as `context_only` instead of dropping; bot-echo carries a non-genuine marker. **Test:** operator-self message feeds session, surfaces nothing; bot-echo doesn't loop.

## Phase 3: context bound (mapped)

1. `--thread-ts` on `flow slack send` (`internal/app/slack.go`) + `/api/slack/send` (`slack_send.go`) + threaded `SendAs`. **Test:** payload carries thread_ts.
2. `internal/server/steerer_compact.go`: occupancy worker mirroring `kbDistiller` (idle-gated, ≥`FLOW_STEERER_COMPACT_PCT` default 60, cooldown) → `wakeTask(slug,"/compact")`. **Test:** `shouldCompact` pure-gate table.

## Phase 4: enable for Slack + validate (mapped)

Flip `FLOW_STEERING_SESSIONS` for Slack; replay coinswitch-class cases; verify single
card + reused thread_key in the live Trace and the chat view. No code beyond config +
Trace assertions.

## Phase 5: GitHub + provider fork + UI + accounting (mapped)

1. GitHub PR/issue key with **canonical** PR↔issue resolution (GAP-4) in `sessionKeyForEvent`. **Test:** linked PR+issue → same key.
2. Provider fork (GAP-9): slot `forking` state, `flow transcript <slug> --compact` hand-off → re-prime Codex, `chats.provider=codex`. **Test:** exhaustion → provider flips, pending event lands on codex.
3. Configured/per-key default provider (GAP-11): `FLOW_STEERER_DEFAULT_PROVIDER` + override store + settings UI. **Test:** resolution `override ?? default`.
4. Token/cost (GAP-12): attach `tokens`/`cost_usd` per chat in `ui_data.go`; render on `Chats.tsx`; "Steering" slice in `Overview.tsx`. 
5. Rename + naming (GAP-13): `flowdb.SetChatTitle` + route + `Chats.tsx` control; auto-title convention + external-org tag; sticky-title refresh.
6. Deletion (GAP-14): lifecycle reconciles `deleted_at` → `stopFloating`; reset-and-reopen.
7. Card→chat link + context-usage indicator (GAP-5 UI).

## Phase 6: cleanup (mapped — see spec "Code cleanup")

Tier 1 (clubbing.go + `MatchConversation` + `FLOW_STEERING_DEDUPE`; relocate `anchorIndex`
into `surface.go`) once default-on; Tier 2 (incremental scaffolding, thread_state shrink,
retrieval re-route, retriage re-eval) only if the cold-call fallback is retired. After
each: `go build ./... && go test ./...` green; `flowdb` migration drops orphaned tables.

---

## Self-review

- **Spec coverage:** GAP-1..14 each map to a phase task in the table/sections above.
  GAP-3 fully detailed (Phase 1); GAP-1,5,8,10 → P2; GAP-2,6 → P3; GAP-4,9,11,12,13,14 → P5;
  cleanup → P6. No gap unassigned.
- **Phase 1 placeholders:** none — full code for `SurfaceCard`, validation, CLI command,
  and three tests. Two explicit "verify the real helper name" notes (`newTestDB`,
  `openFlowDB`, `GetFeedItem`) are codebase-lookup confirmations, not deferred logic.
- **Type consistency:** `SurfaceCardParams`/`SurfaceCard` signature identical across
  surface.go, the steering test, and the CLI command; `flowdb.UpsertFeedItemSurfaced`
  3-return form matches `cascade.go`/`clubbing.go` usage.
