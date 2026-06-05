# Attention Router — P1.2b Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

> ⚠️ **Repo rule:** work happens on branch `flow/attention-router-p1.1`; per-task commits on this branch are pre-approved by the operator. Do not commit to `main`.

**Goal:** Make the P1.2a cascade *live* — route every untracked Slack message into `Cascade.Observe`, and construct + attach the cascade in `flow ui serve` — so a watched-channel message produces an Attention feed row end to end.

**Architecture:** `monitor.Dispatcher.dispatchMessage` already drops untracked messages. We add a `monitor.MessageObserver` interface (defined in `monitor` to avoid an import cycle — `steering` already imports `monitor`), give `Dispatcher` an optional `Steerer MessageObserver` field, and call it on the untracked branch. `*steering.Cascade` satisfies the interface structurally. `server.New` builds the cascade from env config and attaches it to the dispatcher. Stage 0 (inside the cascade) is the real scope filter, so the dispatcher can hand it *every* untracked message cheaply.

**Tech Stack:** Go, `database/sql`, table-driven tests with `dispatcherTestDB(t)` + fake observer. Server wiring is verified by `go build`/`go test ./...` plus a documented manual `flow ui serve` smoke test (live Slack + model can't run in CI).

**Spec:** `docs/superpowers/specs/2026-06-04-attention-router-steerer-design.md` §4.1, §6 (Stage 0 as the in-scope gate), §10 (config).

**Builds on (merged on this branch):** `internal/steering` (`Cascade`, `NewCascade`, `WatchConfig`, `Stage0`); `internal/monitor` (`Dispatcher{DB,Opener}`, `NewDispatcher`, `dispatchMessage`, `InboundEvent`, `ThreadKey`, `SelfUserIDs`, `SlackThreadTagPrefix`, test helpers `dispatcherTestDB`/`seedSlackTask`/`ReadInboxEntries`); `internal/server` (`server.New`).

---

## File Structure

| File | Change |
|---|---|
| `internal/monitor/dispatcher.go` (modify) | Add `MessageObserver` interface; add `Steerer MessageObserver` field to `Dispatcher`; call `d.Steerer.Observe` on the untracked branch of `dispatchMessage`. |
| `internal/monitor/dispatcher_test.go` (modify) | Add `fakeSteerer` + two tests (untracked routes to steerer; tracked does not). |
| `internal/steering/config.go` (create) | `WatchConfigFromEnv()` — builds `WatchConfig` from `FLOW_STEERING_*` + `monitor.SelfUserIDs()`. |
| `internal/steering/config_test.go` (create) | Env → `WatchConfig` parsing. |
| `internal/steering/cascade.go` (modify) | Remove the now-unused `Observer` interface (the dispatcher uses `monitor.MessageObserver` instead). |
| `internal/server/server.go` (modify) | Construct `steering.NewCascade(cfg.DB, steering.WatchConfigFromEnv())` and attach to the dispatcher; add the `steering` import. |

---

## Task 1: Dispatcher steerer hook

**Files:**
- Modify: `internal/monitor/dispatcher.go`
- Test: `internal/monitor/dispatcher_test.go`

- [ ] **Step 1: Write the failing tests** (append to `internal/monitor/dispatcher_test.go`)

```go
// fakeSteerer records the events handed to it, for asserting routing.
type fakeSteerer struct{ events []InboundEvent }

func (f *fakeSteerer) Observe(_ context.Context, ev InboundEvent) error {
	f.events = append(f.events, ev)
	return nil
}

func TestDispatcher_UntrackedMessageRoutesToSteerer(t *testing.T) {
	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	fs := &fakeSteerer{}
	d.Steerer = fs

	msg := InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_new", TS: "1.1", ThreadTS: "1.1", UserID: "U_other", Text: "anyone around?"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(fs.events) != 1 {
		t.Fatalf("steerer Observe should be called once for an untracked message, got %d", len(fs.events))
	}
}

func TestDispatcher_TrackedMessageNotSteered(t *testing.T) {
	db := dispatcherTestDB(t)
	seedSlackTask(t, db, "live-thread", "C_live:5.0")
	d := NewDispatcher(db, nil)
	fs := &fakeSteerer{}
	d.Steerer = fs

	msg := InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_live", TS: "5.1", ThreadTS: "5.0", UserID: "U_other", Text: "reply in tracked thread"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(fs.events) != 0 {
		t.Errorf("tracked-thread message must NOT be steered (it goes to inbox), steerer got %d", len(fs.events))
	}
	entries, _ := ReadInboxEntries("live-thread")
	if len(entries) != 1 {
		t.Errorf("tracked message should append to the task inbox, got %d entries", len(entries))
	}
}

func TestDispatcher_NilSteererDropsUntracked(t *testing.T) {
	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil) // Steerer left nil (CLI context)
	msg := InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_x", TS: "2.1", ThreadTS: "2.1", UserID: "U_other", Text: "hi"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch with nil steerer must be a no-op, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run 'TestDispatcher_UntrackedMessageRoutesToSteerer|TestDispatcher_TrackedMessageNotSteered|TestDispatcher_NilSteererDropsUntracked' -v`
Expected: build failure — `d.Steerer undefined (type *Dispatcher has no field or method Steerer)`.

- [ ] **Step 3: Add the interface** — in `internal/monitor/dispatcher.go`, immediately AFTER the `TaskOpener` interface (the block ending with `}` near line 37), insert:

```go
// MessageObserver observes messages the reaction pipeline does not own
// (untracked threads). The steering cascade implements it. It lives in this
// package — not steering — so Dispatcher can hold one without an import
// cycle (steering already imports monitor). *steering.Cascade satisfies it
// structurally.
type MessageObserver interface {
	Observe(ctx context.Context, ev InboundEvent) error
}
```

- [ ] **Step 4: Add the field** — change the `Dispatcher` struct from:

```go
type Dispatcher struct {
	DB     *sql.DB
	Opener TaskOpener
}
```

to:

```go
type Dispatcher struct {
	DB      *sql.DB
	Opener  TaskOpener
	Steerer MessageObserver // optional: routes untracked messages into the steering cascade
}
```

- [ ] **Step 5: Call the steerer on the untracked branch** — in `dispatchMessage`, replace the trailing comment block + `return nil` (the lines starting at `// DMs are monitored as threads too:` through the final `return nil`) with:

```go
	// DMs are monitored as threads too: when the agent opens or replies in a DM,
	// the tool-use hook registers slack-thread:<dm-channel>:<thread_ts> on the
	// task, so a DM message routes through the thread match above — scoped to the
	// specific conversation, not the whole DM channel.
	//
	// Untracked conversation — not owned by the reaction pipeline. Hand it to the
	// steerer (if wired) to triage; Stage 0 inside the cascade decides whether
	// it's even in scope. A nil steerer (CLI contexts) drops it as before.
	if d.Steerer != nil {
		return d.Steerer.Observe(ctx, ev)
	}
	return nil
```

- [ ] **Step 6: Run to verify it passes**

Run: `go test ./internal/monitor/ -run 'TestDispatcher_UntrackedMessageRoutesToSteerer|TestDispatcher_TrackedMessageNotSteered|TestDispatcher_NilSteererDropsUntracked' -v`
Expected: PASS (3). Then `go test ./internal/monitor/` (full package, no regression), `go build ./...`, `go vet ./internal/monitor/`, `gofmt -l internal/monitor/`.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/dispatcher.go internal/monitor/dispatcher_test.go
git commit -m "feat(monitor): route untracked messages to an optional steerer

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Steering watch-config from env (and remove dead Observer)

**Files:**
- Create: `internal/steering/config.go`
- Test: `internal/steering/config_test.go`
- Modify: `internal/steering/cascade.go` (remove unused `Observer` interface)

- [ ] **Step 1: Confirm `Observer` is unused, then remove it**

Run: `grep -rn "Observer" internal/` — the ONLY hit should be the definition in `cascade.go` (the dispatcher now uses `monitor.MessageObserver`). If anything else references `steering.Observer`, STOP and report. Otherwise delete this block from `internal/steering/cascade.go`:

```go
// Observer consumes an inbound event for steering. Implemented by *Cascade and
// consumed by the dispatcher (P1.2b).
type Observer interface {
	Observe(ctx context.Context, ev monitor.InboundEvent) error
}
```

Leave the `monitor` import in `cascade.go` — `Cascade.Observe`'s signature still uses `monitor.InboundEvent`, so the import is still required (verify with `go build` in Step 4).

- [ ] **Step 2: Write the failing test** — `internal/steering/config_test.go`

```go
package steering

import "testing"

func TestWatchConfigFromEnv(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me, U_alt")
	t.Setenv("FLOW_STEERING_WATCH_CHANNELS", "C1 C2,C3")
	t.Setenv("FLOW_STEERING_MUTED_CHANNELS", "C_mute")
	t.Setenv("FLOW_STEERING_MUTED_KEYWORDS", "lunch, standup")

	cfg := WatchConfigFromEnv()

	for _, c := range []string{"C1", "C2", "C3"} {
		if !cfg.WatchedChannels[c] {
			t.Errorf("watched channel %s missing: %v", c, cfg.WatchedChannels)
		}
	}
	if !cfg.MutedChannels["C_mute"] {
		t.Errorf("muted channels = %v", cfg.MutedChannels)
	}
	if len(cfg.MutedKeywords) != 2 || cfg.MutedKeywords[0] != "lunch" {
		t.Errorf("muted keywords = %v", cfg.MutedKeywords)
	}
	if len(cfg.Identity.UserIDs) != 2 || len(cfg.MentionUserIDs) != 2 {
		t.Errorf("identity/mention should both come from SelfUserIDs: %v / %v", cfg.Identity.UserIDs, cfg.MentionUserIDs)
	}
}

func TestWatchConfigFromEnvEmpty(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "")
	t.Setenv("FLOW_SLACK_SELF_USER_ID", "")
	t.Setenv("FLOW_SLACK_USER_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("FLOW_STEERING_WATCH_CHANNELS", "")
	cfg := WatchConfigFromEnv()
	if len(cfg.WatchedChannels) != 0 || len(cfg.Identity.UserIDs) != 0 {
		t.Errorf("empty env should yield empty config, got %+v", cfg)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/steering/ -run TestWatchConfigFromEnv -v`
Expected: build failure — `undefined: WatchConfigFromEnv`.

- [ ] **Step 4: Write the implementation** — `internal/steering/config.go`

```go
package steering

import (
	"os"
	"strings"

	"flow/internal/monitor"
)

// WatchConfigFromEnv builds a WatchConfig from environment configuration. The
// operator's identity and mention IDs come from the same FLOW_SLACK_SELF_*
// vars the reaction pipeline uses (monitor.SelfUserIDs). Watched/muted
// channels and muted keywords come from FLOW_STEERING_* vars. Channel
// selection via the Mission Control settings UI is P1.4; these env vars
// bridge until then.
func WatchConfigFromEnv() WatchConfig {
	self := monitor.SelfUserIDs()
	return WatchConfig{
		WatchedChannels: toSet(splitList(os.Getenv("FLOW_STEERING_WATCH_CHANNELS"))),
		MutedChannels:   toSet(splitList(os.Getenv("FLOW_STEERING_MUTED_CHANNELS"))),
		MutedKeywords:   splitList(os.Getenv("FLOW_STEERING_MUTED_KEYWORDS")),
		Identity:        OperatorIdentity{UserIDs: self},
		MentionUserIDs:  self,
	}
}

// splitList splits a comma/space/tab/newline-separated env value into trimmed,
// non-empty tokens.
func splitList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toSet builds a membership set, returning nil for an empty input so callers
// can range/lookup uniformly (a nil map reads as "contains nothing").
func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/steering/ -run TestWatchConfigFromEnv -v` → PASS (2). Then `go test ./internal/steering/` (full package — confirms removing `Observer` didn't break anything), `go build ./...`, `go vet ./internal/steering/`, `gofmt -l internal/steering/`.

- [ ] **Step 6: Commit**

```bash
git add internal/steering/config.go internal/steering/config_test.go internal/steering/cascade.go
git commit -m "feat(steering): WatchConfigFromEnv; drop dead Observer interface

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Wire the cascade into `flow ui serve`

**Files:**
- Modify: `internal/server/server.go`

No unit test (constructing `server.New` needs a full server); verified by `go build` + `go test ./...` + a documented manual smoke test.

- [ ] **Step 1: Add the import** — in `internal/server/server.go`, add `"flow/internal/steering"` to the import block (alongside `"flow/internal/monitor"`).

- [ ] **Step 2: Attach the cascade to the dispatcher** — in `server.New`, change the `if cfg.DB != nil {` block from:

```go
	if cfg.DB != nil {
		slackListener := monitor.NewSlackListener(
			monitor.NewDispatcher(cfg.DB, &slackTaskOpener{server: s}),
		)
		slackListener.SetChangeNotifier(func(kind string) {
			s.publishUIChange(kind)
		})
		s.slackListener = slackListener
		s.githubListener = monitor.NewGitHubListener(
			monitor.NewGitHubDispatcher(cfg.DB, &slackTaskOpener{server: s}),
		)
	}
```

to:

```go
	if cfg.DB != nil {
		dispatcher := monitor.NewDispatcher(cfg.DB, &slackTaskOpener{server: s})
		// Attach the steering cascade so untracked messages get triaged into the
		// Attention feed (surface-only in P1). Stage 0 inside the cascade is the
		// real scope gate, so handing it every untracked message is cheap.
		dispatcher.Steerer = steering.NewCascade(cfg.DB, steering.WatchConfigFromEnv())
		slackListener := monitor.NewSlackListener(dispatcher)
		slackListener.SetChangeNotifier(func(kind string) {
			s.publishUIChange(kind)
		})
		s.slackListener = slackListener
		s.githubListener = monitor.NewGitHubListener(
			monitor.NewGitHubDispatcher(cfg.DB, &slackTaskOpener{server: s}),
		)
	}
```

- [ ] **Step 3: Verify it compiles and nothing regresses**

Run: `go build ./...` → clean (this is the real check that `*steering.Cascade` satisfies `monitor.MessageObserver` and there's no import cycle).
Run: `go test ./...` → PASS across all packages.
Run: `go vet ./internal/server/` and `gofmt -l internal/server/` → clean.

- [ ] **Step 4: Build the binary**

Run: `go build -o flow .` → produces `./flow` with no error.

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go
git commit -m "feat(server): wire the steering cascade into flow ui serve

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 6: Document the manual smoke test (for the operator — not run by the implementer)**

The implementer cannot exercise a live Slack workspace + `claude` CLI in CI. Record this for the operator to run when they choose:

```
1. export FLOW_SLACK_APP_TOKEN / SLACK_BOT_TOKEN / FLOW_SLACK_USER_TOKEN / FLOW_SLACK_SELF_USER_IDS (existing Slack setup)
2. export FLOW_STEERING_WATCH_CHANNELS=<a channel ID you can post in>
3. ./flow ui serve
4. Post a message in that channel from another account (so it's not self-authored).
5. Confirm a row appears:  sqlite3 ~/.flow/flow.db "SELECT source,thread_key,suggested_action,confidence,summary FROM attention_feed ORDER BY created_at DESC LIMIT 5;"
   (No UI yet — the Attention feed panel is P1.4. This query is the P1.2b verification surface.)
```

The implementer reports the steps above as the manual-verification note; the operator runs it.

---

## Self-Review

**1. Spec coverage (P1.2b scope):**
- §4.1/§6 route untracked messages into triage → Task 1 (`dispatchMessage` → `Steerer.Observe`). ✅
- §10 env config (watched/muted channels, identity) → Task 2 (`WatchConfigFromEnv`). ✅
- Make it live in `flow ui serve` → Task 3 (`server.New` wiring). ✅
- Import-cycle avoidance → `MessageObserver` defined in `monitor`, satisfied structurally by `*steering.Cascade`. ✅
- *Deferred (correct):* settings-UI channel selection, feed UI, push (P1.4); autonomy-gated actions (P1.3). P1.2b remains surface-only.

**2. Placeholder scan:** No TBD/TODO. The "manual smoke test" (Task 3 Step 6) is intentionally operator-run and clearly marked — it is not a placeholder for missing code; the code path is fully implemented and compile/test-verified.

**3. Type consistency:**
- `MessageObserver.Observe(ctx context.Context, ev InboundEvent) error` (monitor) ↔ `Cascade.Observe(ctx context.Context, ev monitor.InboundEvent) error` (steering): identical shape; `*steering.Cascade` satisfies `monitor.MessageObserver` structurally. Verified by `go build` in Task 3 Step 3. ✅
- `Dispatcher.Steerer` field typed `MessageObserver`; `NewDispatcher` leaves it nil (backward compatible — existing `NewDispatcher(db, nil)` call sites and the nil-guard in `dispatchMessage` keep working; covered by `TestDispatcher_NilSteererDropsUntracked`). ✅
- `WatchConfigFromEnv` returns `steering.WatchConfig` (fields `WatchedChannels`, `MutedChannels`, `MutedKeywords`, `Identity`, `MentionUserIDs` — from stage0.go). ✅
- `monitor.SelfUserIDs() []string` reused (reaction_trigger.go:85). ✅
- `seedSlackTask`/`dispatcherTestDB`/`ReadInboxEntries`/`SlackThreadTagPrefix` are existing test infra (dispatcher_test.go:76/94, inbox.go). ✅
- Removing `steering.Observer` is safe: Task 2 Step 1 greps to confirm no references; `monitor` import in cascade.go stays (used by `Cascade.Observe`). ✅

No unresolved issues.

---

## After P1.2b

P1 is then **functionally live end-to-end** (observe → triage → feed row), minus the UI. Remaining P1: **P1.3** (autonomy-gated actions: make-task / forward / dismiss from a feed row) and **P1.4** (Mission Control settings + Attention feed panel + push). Those are their own plans.

## Execution Handoff

Plan complete. Execute subagent-driven (recommended) or inline?
