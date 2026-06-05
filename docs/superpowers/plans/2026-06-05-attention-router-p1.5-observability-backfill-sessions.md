# Attention Router P1.5 — Observability, Steerer Backfill, Haiku Session Reuse

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the three gaps that made the steerer a black box: (1) no visibility into why a message was/wasn't surfaced, (2) no catch-up so messages arriving while the listener was down are lost forever, (3) a fresh `claude -p` process per classifier call with the heavy framing re-sent every time.

**Architecture:** Three independent-but-related pillars on the existing `internal/steering` cascade:
- **A. Observability** — a `steering_trace` table records every observed event's full journey (stage-by-stage verdicts, drop reason, latency, origin). Surfaced via `/api/attention/trace`, `flow attention trace`, and a Trace panel in Mission Control. Nothing is lost to stderr.
- **B. Steerer backfill** — a `SteeringBackfill` runner that, on boot and on an interval, pulls recent messages in watched channels + DMs from the Slack Web API (`conversations.history`) since a per-channel watermark, and feeds them through the SAME cascade (origin=`backfill`). Mirrors the reaction pipeline's `SlackBackfill`, but for the steerer's firehose rather than tracked threads. A bounded cold-start lookback catches the gap before the steerer first ran.
- **C. Haiku session reuse + batching** — a per-mode primed `claude` session: the heavy instructions (+ task index for Stage 2) are sent once at session creation (`--session-id`), then each subsequent classifier call resumes (`--resume`) sending only the compact payload. Sessions rotate after N turns / a TTL / when the primed context changes. The backfill path additionally batches Stage 1 relevance over a whole pass in one call. Stage 3 (deep) stays one-shot.

**Tech Stack:** Go (pure, no CGO; `modernc.org/sqlite`), `slack-go/slack`, `claude -p` CLI shell-out behind mockable package-level vars, React+TS Mission Control UI.

**Validated assumptions (checked against the live `claude` CLI on 2026-06-05):**
- `claude -p --model claude-haiku-4-5 --dangerously-skip-permissions` resolves and returns clean JSON.
- `claude -p "<prompt>" --session-id <uuid> ...` then `claude -p "<payload>" --resume <uuid> ...` — the resumed turn recalls the first turn's context. Priming works in print mode.
- `schemaDDL` runs on every `OpenDB` and is fully `IF NOT EXISTS`; new tables go there, no migration needed.

---

## Build order (dependency-ordered)

1. **C1** — primed Haiku session pool (pure, testable in isolation)
2. **C2** — classifier refactor to prime/payload + `runClassifier` dispatch + `EnableClassifierSessions`
3. **A1** — `steering_trace` table + flowdb store
4. **A2** — cascade trace instrumentation + `ObserveBatch` (batched Stage 1)
5. **A3** — `/api/attention/trace` endpoint (funnel + recent decisions)
6. **A4** — `flow attention trace` CLI
7. **B2** — `steering_watermark` store
8. **B1** — `conversations.history` clients (bot + user) + IM enumeration
9. **B3** — `SteeringBackfill` runner (uses `ObserveBatch` + history clients + watermark)
10. **B4** — server wiring (start backfill goroutine + call `EnableClassifierSessions`)
11. **A5** — Mission Control Trace panel

Each task: write failing test → run (fail) → implement → run (pass) → `go build ./... && go test ./...` → commit. UI task (A5) verifies via `cd internal/server/ui && npm run build` (typecheck) — visual review by operator.

---

## Pillar C — Haiku session reuse + batching

### Task C1: Primed Haiku session pool

**Files:**
- Create: `internal/steering/session.go`
- Test: `internal/steering/session_test.go`

The pool owns reusable `claude` CLI sessions, one per *mode* ("stage1", "stage2"). First call to a mode creates a session (`--session-id <uuid>`) and sends `prime + "\n\n" + payload`. Subsequent calls resume (`--resume <uuid>`) and send only `payload`. A session rotates (next call re-creates + re-primes) when: it doesn't exist, its turn count hit `maxTurns`, it's older than `ttl`, or the caller's `primeKey` changed (e.g. the task index changed). Any exec error resets the session so a wedged session self-heals.

```go
package steering

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// classifierExec runs `claude` with the given args and returns stdout. The one
// mockable seam for the session pool — tests inject a fake that records args.
type classifierExec func(ctx context.Context, args []string) (string, error)

func defaultClassifierExec(ctx context.Context, args []string) (string, error) {
	out, err := exec.CommandContext(ctx, "claude", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// classifierPool manages reusable, primed claude sessions for the cheap
// classifier stages. One session per mode; the heavy framing (instructions +
// task index) is sent once at creation and reused across turns via --resume,
// so each subsequent call sends only the compact payload.
type classifierPool struct {
	maxTurns int
	ttl      time.Duration
	now      func() time.Time
	newID    func() string
	exec     classifierExec

	mu       sync.Mutex
	sessions map[string]*sessionSlot
}

type sessionSlot struct {
	id        string
	turns     int
	startedAt time.Time
	primeKey  string
}

func newClassifierPool(maxTurns int, ttl time.Duration) *classifierPool {
	if maxTurns <= 0 {
		maxTurns = 40
	}
	if ttl <= 0 {
		ttl = 20 * time.Minute
	}
	return &classifierPool{
		maxTurns: maxTurns, ttl: ttl,
		now: time.Now, newID: randomUUID, exec: defaultClassifierExec,
		sessions: map[string]*sessionSlot{},
	}
}

// run executes one classifier turn for mode. prime is the heavy framing sent
// only when (re)creating the session; payload is the compact per-call text
// sent every turn. primeKey rotates the session when the primed context
// changes (pass a stable string for static primes).
func (p *classifierPool) run(ctx context.Context, mode, prime, payload, primeKey string) (string, error) {
	p.mu.Lock()
	slot := p.sessions[mode]
	fresh := slot == nil ||
		slot.turns >= p.maxTurns ||
		p.now().Sub(slot.startedAt) >= p.ttl ||
		slot.primeKey != primeKey
	var (
		args []string
		text string
	)
	if fresh {
		slot = &sessionSlot{id: p.newID(), startedAt: p.now(), primeKey: primeKey}
		p.sessions[mode] = slot
		text = prime + "\n\n" + payload
		args = []string{"-p", text, "--model", classifierModel(), "--dangerously-skip-permissions", "--session-id", slot.id}
	} else {
		text = payload
		args = []string{"-p", text, "--model", classifierModel(), "--dangerously-skip-permissions", "--resume", slot.id}
	}
	id := slot.id
	p.mu.Unlock()

	out, err := p.exec(ctx, args)
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.sessions[mode]
	if err != nil {
		// Reset only if this is still the session we used (avoid clobbering a
		// concurrent rotation).
		if cur != nil && cur.id == id {
			delete(p.sessions, mode)
		}
		return "", err
	}
	if cur != nil && cur.id == id {
		cur.turns++
	}
	return out, nil
}

// randomUUID returns a v4-ish UUID string for claude --session-id (must be a
// valid UUID). Uses the same crypto/rand source as randomID.
func randomUUID() string { /* 8-4-4-4-12 hex from crypto/rand, version/variant bits set */ }
```

- [ ] **Step 1 — Write failing tests** (`session_test.go`):
  - `TestPoolPrimesThenResumes`: pool with fake exec recording args. First `run(ctx,"stage1","PRIME","PAY1","k")` → asserts args contain `--session-id` (a uuid) and the sent text == `"PRIME\n\nPAY1"`. Second `run(...,"PAY2","k")` → args contain `--resume <same uuid>` and text == `"PAY2"` (no prime).
  - `TestPoolRotatesAfterMaxTurns`: maxTurns=2, fixed clock; 3rd call gets a NEW `--session-id` (different uuid) and re-includes the prime.
  - `TestPoolRotatesOnTTL`: ttl small, clock advanced past ttl → next call re-primes with new id.
  - `TestPoolRotatesOnPrimeKeyChange`: changing `primeKey` forces a new session + prime.
  - `TestPoolResetsOnError`: fake exec returns error once → session for that mode is dropped; next call re-creates (new `--session-id`).
  - Use injected `now`/`newID`/`exec` (set fields directly after `newClassifierPool`).
- [ ] **Step 2 — Run, verify fail.** `go test ./internal/steering/ -run TestPool -v`
- [ ] **Step 3 — Implement** `session.go` (including `randomUUID`).
- [ ] **Step 4 — Run, verify pass.** Then `go build ./... && go test ./internal/steering/`
- [ ] **Step 5 — Commit:** `feat(steering): primed reusable Haiku session pool (P1.5 C1)`

### Task C2: Classifier refactor — prime/payload split + session dispatch

**Files:**
- Modify: `internal/steering/classifier.go`
- Test: `internal/steering/classifier_test.go` (extend), `internal/steering/session_test.go`

Split the Stage 1/2 prompts into a `*Prime()` (static instructions, + task index for Stage 2) and a `*Payload()` (the compact per-call data). Route both through a new `runClassifier(ctx, mode, prime, payload, primeKey)` that uses the active pool when enabled, else falls back to the existing one-shot `classifierRunner` (preserving current test behavior). Combined `prime + "\n\n" + payload` must equal today's prompt text so existing classifier tests are unaffected.

```go
// package-global; nil unless EnableClassifierSessions() was called (production).
var activeClassifierPool *classifierPool

// EnableClassifierSessions turns on Haiku session reuse for the cheap stages.
// Idempotent. maxTurns/ttl come from FLOW_STEERING_SESSION_MAX_TURNS /
// FLOW_STEERING_SESSION_TTL (sensible defaults). No-op when
// FLOW_STEERING_SESSION_REUSE=0.
func EnableClassifierSessions() {
	if !envBoolDefault("FLOW_STEERING_SESSION_REUSE", true) { return }
	if activeClassifierPool == nil {
		activeClassifierPool = newClassifierPool(sessionMaxTurns(), sessionTTL())
	}
}

func runClassifier(ctx context.Context, mode, prime, payload, primeKey string) (string, error) {
	if activeClassifierPool != nil {
		return activeClassifierPool.run(ctx, mode, prime, payload, primeKey)
	}
	return classifierRunner(ctx, prime+"\n\n"+payload)
}
```

- `Stage1Relevance`: build `prime := stage1Prime()`, `payload := stage1Payload(string(inputsJSON))`; call `runClassifier(ctx, "stage1", prime, payload, "stage1-v1")`. (`stage1Prime()+"\n\n"+stage1Payload(x)` must byte-equal the old `stage1Prompt(x)`.)
- `Stage2Score`: `prime := stage2Prime(taskIndex)`, `payload := stage2Payload(in)`; `primeKey := "stage2:" + shortHash(taskIndex)`; `runClassifier(ctx, "stage2", prime, payload, primeKey)`. (`stage2Prime(idx)+"\n\n"+stage2Payload(in)` must byte-equal old `stage2Prompt(in, idx)`.)
- Add `envBoolDefault` if not already shared in steering (mirror monitor's); `sessionMaxTurns()`/`sessionTTL()` env readers; `shortHash(s)` (fnv32a hex, first 12 chars).
- Keep `classifierRunner` and `classifierModel` exactly as they are (the pool's default exec produces equivalent args).

- [ ] **Step 1 — Write failing tests:**
  - `TestStage1PromptCompositionUnchanged`: assert `stage1Prime()+"\n\n"+stage1Payload(x) == stage1Prompt(x)` for a sample `x`. (Add a temporary reference to the old `stage1Prompt` or inline the expected text.)
  - `TestStage2PromptCompositionUnchanged`: same for stage2.
  - `TestRunClassifierUsesPoolWhenEnabled`: set `activeClassifierPool` to a pool with a fake exec; call `Stage1Relevance` with canned JSON; assert exec saw `--session-id`. Reset the global in a `defer`.
- [ ] **Step 2 — Run, verify fail.**
- [ ] **Step 3 — Implement** the split + dispatch.
- [ ] **Step 4 — Run, verify pass.** `go build ./... && go test ./internal/steering/`
- [ ] **Step 5 — Commit:** `feat(steering): prime/payload classifier dispatch via session pool (P1.5 C2)`

---

## Pillar A — Observability (cascade decision trace)

### Task A1: `steering_trace` table + flowdb store

**Files:**
- Modify: `internal/flowdb/db.go` (add table + indexes to `schemaDDL`)
- Create: `internal/flowdb/steering_trace.go`
- Test: `internal/flowdb/steering_trace_test.go`

Add to `schemaDDL` (right after the `attention_feed` table, before the index block; add the two indexes in the index block):

```sql
CREATE TABLE IF NOT EXISTS steering_trace (
    id                TEXT PRIMARY KEY,
    created_at        TEXT NOT NULL,
    origin            TEXT NOT NULL DEFAULT 'live',
    source            TEXT NOT NULL DEFAULT '',
    channel           TEXT,
    channel_type      TEXT,
    author            TEXT,
    thread_key        TEXT,
    text_preview      TEXT,
    disposition       TEXT NOT NULL,            -- dropped | surfaced | error
    stage_reached     TEXT NOT NULL,            -- stage0 | cache | stage1 | stage2 | stage3 | surfaced
    drop_reason       TEXT,
    stage1_relevant   INTEGER,                  -- NULL = not reached
    stage2_action     TEXT,
    stage2_confidence REAL,
    stage3_action     TEXT,
    stage3_confidence REAL,
    final_action      TEXT,
    final_confidence  REAL,
    feed_item_id      TEXT,
    error             TEXT,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    model             TEXT
);
```
Indexes (in the index block): `idx_steering_trace_created ON steering_trace(created_at)`, `idx_steering_trace_disposition ON steering_trace(disposition)`.

`steering_trace.go`:
```go
type SteeringTrace struct {
	ID, CreatedAt, Origin, Source, Channel, ChannelType, Author, ThreadKey, TextPreview string
	Disposition, StageReached, DropReason string
	Stage1Relevant *bool          // nil = not reached
	Stage2Action string; Stage2Confidence float64
	Stage3Action string; Stage3Confidence float64
	FinalAction string; FinalConfidence float64
	FeedItemID, Error string
	LatencyMS int64
	Model string
}

func InsertSteeringTrace(db *sql.DB, t SteeringTrace) error   // NullIfEmpty for optional text cols; stage1_relevant via sql.NullBool-ish (write NULL when nil)
func ListSteeringTrace(db *sql.DB, f TraceFilter) ([]SteeringTrace, error)

type TraceFilter struct {
	Disposition string // "" = all
	Since       string // RFC3339 lower bound on created_at; "" = no bound
	Limit       int    // <=0 → default 200
}

// SteeringFunnel is an aggregate over a time window for the funnel view.
type SteeringFunnel struct {
	Observed       int
	DroppedStage0  int
	DroppedCache   int
	DroppedStage1  int
	DroppedStage2  int
	Surfaced       int
	Errors         int
}
func SteeringFunnelSince(db *sql.DB, since string) (SteeringFunnel, error)  // COUNT(*) GROUP BY stage_reached/disposition
```
`SteeringFunnelSince` buckets: Observed = total rows in window; DroppedStage0 = disposition='dropped' AND stage_reached='stage0'; DroppedCache = stage_reached='cache'; DroppedStage1 = dropped AND stage_reached='stage1'; DroppedStage2 = dropped AND stage_reached='stage2'; Surfaced = disposition='surfaced'; Errors = disposition='error'.

- [ ] **Step 1 — Write failing tests** (`steering_trace_test.go`, real temp SQLite via existing test helper, mirror `attention` tests): insert a few traces with varied dispositions/stages; assert `ListSteeringTrace` filters by disposition + since + limit (newest first); assert `SteeringFunnelSince` buckets correctly; assert `Stage1Relevant` round-trips nil and non-nil.
- [ ] **Step 2 — Run, verify fail.**
- [ ] **Step 3 — Implement** (DDL + store).
- [ ] **Step 4 — Run, verify pass.** `go build ./... && go test ./internal/flowdb/`
- [ ] **Step 5 — Commit:** `feat(flowdb): steering_trace table + store (P1.5 A1)`

### Task A2: Cascade trace instrumentation + `ObserveBatch`

**Files:**
- Modify: `internal/steering/cascade.go`
- Test: `internal/steering/cascade_test.go` (extend)

Add a trace sink to `Cascade` and emit exactly one trace row per observed event covering its whole journey. Add an `origin`-aware internal path and a batched entry point.

```go
type Cascade struct {
	// ...existing...
	trace func(flowdb.SteeringTrace) // default: InsertSteeringTrace; test seam
}
// NewCascade sets: trace = func(t){ _ = flowdb.InsertSteeringTrace(db, t) }
```

- Rename the body of `Observe` to `func (c *Cascade) observe(ctx, ev, origin string) error`; keep `func (c *Cascade) Observe(ctx, ev) error { return c.observe(ctx, ev, "live") }` (satisfies `monitor.MessageObserver`). Add `func (c *Cascade) ObserveBackfill(ctx, ev) error { return c.observe(ctx, ev, "backfill") }`.
- In `observe`, build a `tr := flowdb.SteeringTrace{ID: c.newID(), CreatedAt: start.UTC()..., Origin: origin, Source: "slack", Channel: ev.Channel, ChannelType: ev.ChannelType, Author: ev.UserID, TextPreview: preview(ev.Text), Model: classifierModel()}` and a single deferred `emit` that stamps `LatencyMS` and calls `c.trace(tr)`. At each exit point set `tr.Disposition / StageReached / DropReason / stageN fields / FeedItemID / Error` before returning. `writeFeed` returns the feed id so `tr.FeedItemID` can be set.
  - stage0 fail → dropped/stage0/`s0.DropReason`.
  - cache hit → dropped/cache/`"duplicate within verdict TTL"`.
  - stage1 err → error/stage1/err. not relevant → dropped/stage1, `Stage1Relevant=&false`.
  - taskindex err → error/stage1.
  - stage2 err → error/stage2. stage2 drop → dropped/stage2, `Stage2Action="drop"`.
  - budget exhausted → surfaced/stage2, `DropReason="deep budget exhausted; surfaced stage2 verdict"`, set Stage2* + Final* + FeedItemID.
  - stage3 err → record `tr.Error` = stage3 failure, fall back to v2, still surfaced/stage2.
  - surfaced (stage3) → surfaced/stage3, set Stage1Relevant=&true, Stage2*/Stage3*/Final*, FeedItemID.
- `preview(s)`: trim + truncate to 200 runes (+ `…`). It's operator data, not a secret; safe to store.
- `writeFeed` change: return `(string, error)` (the feed item id). Update its one current caller.

`ObserveBatch` (used by backfill — batches Stage 1 in ONE call):
```go
func (c *Cascade) ObserveBatch(ctx context.Context, evs []monitor.InboundEvent) error
```
- Run Stage 0 + verdict-cache on each ev → collect survivors `[]ClassifyInput` (and emit dropped traces for the rest).
- One `Stage1Relevance(ctx, survivors)` call for the whole batch.
- For each survivor: if not relevant → trace dropped/stage1; else run the per-item Stage 2 (+budget/Stage 3) + writeFeed + trace, reusing the same logic as `observe`'s tail. Factor the shared per-item tail into a helper to avoid divergence.
- Returns the first error encountered (others logged); a single bad item never aborts the batch.

- [ ] **Step 1 — Write failing tests:** with a captured trace sink (`c.trace = func(t){ traces = append(traces, t) }`) and stubbed `classifierRunner`/`deepTriageRunner`:
  - self-authored event → one trace, dropped/stage0/self-authored.
  - relevant + stage2 make_task → one trace, surfaced/stage3 (or stage2 if deep stubbed to error), `FeedItemID != ""`, `FinalAction` set.
  - duplicate (second observe of same thread within TTL) → dropped/cache.
  - `ObserveBatch` with 3 events (1 self, 2 relevant) → 3 traces; Stage 1 invoked once (assert via a call-counting classifierRunner).
- [ ] **Step 2 — Run, verify fail.**
- [ ] **Step 3 — Implement.**
- [ ] **Step 4 — Run, verify pass.** `go build ./... && go test ./internal/steering/ ./internal/server/`
- [ ] **Step 5 — Commit:** `feat(steering): cascade decision trace + batched ObserveBatch (P1.5 A2)`

### Task A3: `/api/attention/trace` endpoint

**Files:**
- Modify: `internal/server/attention.go`, `internal/server/server.go` (register route), `internal/server/types.go` (view types)
- Test: `internal/server/attention_test.go` (extend, mirror existing handler tests)

`GET /api/attention/trace?since=&disposition=&limit=` → JSON `{ "funnel": {...}, "items": [...] }`. Default `since` = 24h ago (computed from `time.Now`); default disposition all; default limit 200. Add `SteeringTraceView` + `SteeringFunnelView` to types.go (snake_case JSON mirroring the structs). Register `mux.HandleFunc("/api/attention/trace", s.handleAttentionTrace)` next to `/api/attention`.

- [ ] **Step 1 — Write failing test:** seed traces in temp DB, GET the endpoint via the handler, assert funnel counts + items length + filter behavior.
- [ ] **Step 2 — Run, verify fail.**
- [ ] **Step 3 — Implement.**
- [ ] **Step 4 — Run, verify pass.** `go build ./... && go test ./internal/server/`
- [ ] **Step 5 — Commit:** `feat(server): /api/attention/trace funnel + decisions (P1.5 A3)`

### Task A4: `flow attention trace` CLI

**Files:**
- Modify: `internal/app/attention.go`
- Test: `internal/app/attention_test.go` (extend)

Add `trace` subcommand: `flow attention trace [--since 24h] [--disposition dropped|surfaced|error|all] [--limit 50]`. Parse `--since` as a Go duration ago (e.g. `1h`, `24h`) → RFC3339 lower bound. Print a funnel summary line (`observed N · stage0 a · stage1 b · stage2 c · surfaced d · errors e`) then a compact table (time, origin, disposition, stage, conf, channel, reason/summary). Add `renderTrace(funnel, items)` pure renderer + unit test. Update `printAttentionUsage`.

- [ ] **Step 1 — Write failing test:** `renderTrace` over sample data asserts funnel line + a row; `--since` parsing helper test.
- [ ] **Step 2 — Run, verify fail.**
- [ ] **Step 3 — Implement.**
- [ ] **Step 4 — Run, verify pass.** `go build ./... && go test ./internal/app/`
- [ ] **Step 5 — Commit:** `feat(cli): flow attention trace (P1.5 A4)`

---

## Pillar B — Steerer backfill

### Task B2: `steering_watermark` store

**Files:**
- Modify: `internal/flowdb/db.go` (add table to `schemaDDL`)
- Create: `internal/flowdb/steering_watermark.go`
- Test: `internal/flowdb/steering_watermark_test.go`

```sql
CREATE TABLE IF NOT EXISTS steering_watermark (
    channel    TEXT PRIMARY KEY,
    last_ts    TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
```
```go
func GetSteeringWatermark(db *sql.DB, channel string) (string, error)        // "" when none
func SetSteeringWatermark(db *sql.DB, channel, lastTS, updatedAt string) error // UPSERT (ON CONFLICT(channel))
```

- [ ] **Step 1 — Write failing test:** get on empty → ""; set then get → value; set again (newer) → overwrites.
- [ ] **Step 2 — Run, verify fail.**
- [ ] **Step 3 — Implement.**
- [ ] **Step 4 — Run, verify pass.** `go build ./... && go test ./internal/flowdb/`
- [ ] **Step 5 — Commit:** `feat(flowdb): steering_watermark store (P1.5 B2)`

### Task B1: `conversations.history` clients + IM enumeration

**Files:**
- Create: `internal/monitor/slack_history.go`
- Test: `internal/monitor/slack_history_test.go`

Mirror `slack_backfill.go`'s client pattern. Reuse the existing `SlackMessage` shape.

```go
// SlackHistory fetches a conversation's recent top-level messages for the
// steerer backfill. oldest is a Slack ts lower bound (exclusive).
type SlackHistory interface {
	History(ctx context.Context, channelID, oldest string, limit int) ([]SlackMessage, error)
}
// SlackIMLister enumerates the operator's DM channels (user token only).
type SlackIMLister interface {
	ListIMs(ctx context.Context) ([]string, error) // channel IDs
}

func NewSlackHistoryClient() SlackHistory        // bot token; nil when none — channels
func NewSlackUserHistoryClient() SlackHistory    // user token; nil when none — DMs
func NewSlackUserIMLister() SlackIMLister         // user token; nil when none
```
Implementations use `api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID, Oldest, Inclusive:false, Limit})` and `api.GetConversationsContext(... Types:["im"])`. Map results to `SlackMessage` (User, Text, TS, ThreadTS, SubType). Keep a mockable seam (`slackHistoryFn` / `slackIMListFn` function vars, like `slackConversationsFn`) so tests don't hit the network.

- [ ] **Step 1 — Write failing test:** swap the function-var seam to return canned messages/IDs; assert the interface methods map fields and pass `Oldest`/`Limit` through.
- [ ] **Step 2 — Run, verify fail.**
- [ ] **Step 3 — Implement.**
- [ ] **Step 4 — Run, verify pass.** `go build ./... && go test ./internal/monitor/`
- [ ] **Step 5 — Commit:** `feat(monitor): conversations.history + IM-list clients (P1.5 B1)`

### Task B3: `SteeringBackfill` runner

**Files:**
- Create: `internal/steering/backfill.go`
- Test: `internal/steering/backfill_test.go`

```go
type SteeringBackfill struct {
	db        *sql.DB
	observe   func(ctx context.Context, evs []monitor.InboundEvent) error // cascade.ObserveBatch
	channels  SlackHistory_iface   // bot history client (channels); nil → channel backfill skipped
	dms       SlackHistory_iface   // user history client (DMs); nil → DM backfill skipped
	ims       SlackIMLister_iface  // enumerates DM channels; nil → DM backfill skipped
	configFn  func() WatchConfig   // watched channel set
	interval  time.Duration        // default 60s
	lookback  time.Duration        // cold-start window, default 1h
	limit     int                  // per-channel cap, default 50
	now       func() time.Time
	logFn     func(string, ...any)
}
```
(Use `monitor.SlackHistory` / `monitor.SlackIMLister` for the interface types.)

`Run(ctx)`: immediate `runOnce` then ticker every `interval`.

`runOnce(ctx)`:
- `cfg := configFn()`. Build the channel set: watched channels (keys of `cfg.WatchedChannels`) via `channels` client (channel_type "channel"); plus DM channel IDs from `ims.ListIMs` via `dms` client (channel_type "im"). Skip a group when its client is nil; `logFn` that it was skipped (no silent caps).
- For each channel:
  - `wm := GetSteeringWatermark(db, channel)`.
  - `oldest := wm`; coldStart := wm == ""; if coldStart → `oldest := slackTSFromTime(now().Add(-lookback))`.
  - `msgs := client.History(ctx, channel, oldest, limit)`. If `len(msgs) == limit` and coldStart → `logFn("backfill %s: hit cap %d in cold-start lookback %s; older gap not covered", channel, limit, lookback)` (no silent truncation).
  - Build `[]InboundEvent` for messages with `ts > wm` (or all when coldStart), `Kind:"message"`, `ChannelType`, `ThreadTS: firstNonEmpty(threadTS, ts)`, `UserID`, `Text`. Skip non-acceptable subtypes (reuse `backfillAcceptMessage` logic — exported helper or re-impl).
  - `observe(ctx, evs)` (cascade.ObserveBatch — Stage 0 inside drops self/bot/out-of-scope and traces every one with origin=backfill).
  - Advance watermark to the newest ts seen this pass (`SetSteeringWatermark`), even if all were dropped (so we don't re-scan).
- Per-channel errors are logged and skipped; one bad channel never aborts the pass.

Notes / documented limitations (put in the file's doc comment): v1 backfills **top-level** channel/DM messages only — thread replies in watched channels during downtime are not swept (the reaction pipeline's `SlackBackfill` already covers replies for *tracked* threads). Mentions in *unwatched* channels during downtime are not discoverable without a full-workspace scan; `logFn` notes this is out of scope. These are deliberate bounds, logged, not silent.

`slackTSFromTime(t)` → `fmt.Sprintf("%d.000000", t.Unix())`. Dedup/idempotency comes from: watermark advancement + the cascade's verdict cache + `attention_feed` thread_key coalescing.

- [ ] **Step 1 — Write failing tests:** fakes for `SlackHistory`/`SlackIMLister`, a capturing `observe`, a real temp DB for watermarks, fixed `now`:
  - cold start (no watermark) with lookback → fetches with the lookback `oldest`, passes events to `observe`, advances watermark to newest ts.
  - warm (watermark set) → only messages newer than watermark are passed; watermark advances.
  - cap hit on cold start → `logFn` warns (capture log lines).
  - nil DM client → DM group skipped, channels still processed.
- [ ] **Step 2 — Run, verify fail.**
- [ ] **Step 3 — Implement.**
- [ ] **Step 4 — Run, verify pass.** `go build ./... && go test ./internal/steering/`
- [ ] **Step 5 — Commit:** `feat(steering): SteeringBackfill catch-up runner (P1.5 B3)`

### Task B4: Server wiring

**Files:**
- Modify: `internal/server/server.go` (in `ListenAndServe`, near the existing `SlackBackfill` block; and call `steering.EnableClassifierSessions()` where the cascade is built in `New`)
- Test: none new (wiring); rely on `go build` + existing server tests.

- In `New` (where `cascade := steering.NewCascade(...)` is created), call `steering.EnableClassifierSessions()` once (guarded by its own env check internally).
- In `ListenAndServe`, after the `SlackBackfill` block, add a `SteeringBackfill` block gated on `envBoolDefault("FLOW_STEERING_BACKFILL", true)` AND at least one history client being non-nil:
  ```go
  if s.cfg.DB != nil && steeringBackfillEnabled() {
      ch := monitor.NewSlackHistoryClient()
      dm := monitor.NewSlackUserHistoryClient()
      ims := monitor.NewSlackUserIMLister()
      if ch != nil || dm != nil {
          bf := steering.NewSteeringBackfill(s.cfg.DB, ch, dm, ims, steering.WatchConfigFromEnv, /*interval*/0, /*lookback*/0, /*limit*/0)
          bf.SetLogger(func(f string, a ...any){ fmt.Fprintf(os.Stderr, f+"\n", a...) })
          // observe wired to the SAME cascade instance the dispatcher uses, via ObserveBatch.
          ctx, cancel := context.WithCancel(context.Background()); defer cancel()
          go bf.Run(ctx)
      }
  }
  ```
  `NewSteeringBackfill` should accept the cascade's `ObserveBatch` (expose the cascade on the server or build the backfill in `New` and store it on `s`). Simplest: store the `*steering.Cascade` on `Server` in `New` (`s.cascade = cascade`) and pass `s.cascade.ObserveBatch` here. Env readers (`FLOW_STEERING_BACKFILL_INTERVAL/LOOKBACK/LIMIT`) live in steering with defaults; `NewSteeringBackfill` applies them when args are zero.
- [ ] **Step 1 — Implement wiring.** Add `s.cascade` field; `steeringBackfillEnabled()` helper.
- [ ] **Step 2 — Verify:** `go build ./... && go test ./internal/server/ ./...`
- [ ] **Step 3 — Commit:** `feat(server): start SteeringBackfill + enable Haiku sessions (P1.5 B4)`

---

## Pillar A (UI)

### Task A5: Mission Control Trace panel

**Files:**
- Modify: `internal/server/ui/src/lib/types.ts` (`SteeringTrace`, `SteeringFunnel`), `internal/server/ui/src/lib/query.ts` (`useAttentionTrace`), `internal/server/ui/src/screens/Attention.tsx` (Trace tab)
- Verify: `cd internal/server/ui && npm run build`

Add a "Trace" view to the Attention screen: a funnel strip (Observed → Stage 0 → Stage 1 → Stage 2 → Surfaced, with Errors called out) and a table of recent decisions (time, origin badge live/backfill, disposition, stage reached, confidence, channel, drop reason / summary). Reuse existing tokens.css / ui.tsx primitives — NO new design system, no glass/gradient (see `[[flow-manager-no-ai-slop-design]]`). `useAttentionTrace(since)` hits `/api/attention/trace`. A small "Trace"/"Feed" segmented toggle at the top of `Attention.tsx` switches views; default Feed.

- [ ] **Step 1 — Implement** types + query hook + Trace view.
- [ ] **Step 2 — Verify:** `npm run build` (typecheck + bundle). Restore tracked `internal/server/static/index.html` after (`git checkout` — static/assets is gitignored, commit source only).
- [ ] **Step 3 — Commit:** `feat(ui): attention Trace panel (funnel + decisions) (P1.5 A5)`

---

## Self-review checklist (run after all tasks)
- Funnel math: Observed == Stage0-drops + Cache-drops + Stage1-drops + Stage2-drops + Surfaced + Errors (no double counting; each event emits exactly one trace).
- Session reuse OFF (`FLOW_STEERING_SESSION_REUSE=0`) → behavior byte-identical to today (one-shot `classifierRunner`).
- Backfill is idempotent across restarts (watermark + verdict cache + feed coalescing) — re-running a pass surfaces nothing new.
- All token values still masked in any log/UI (no secret printing).
- `go build ./... && go test ./...` green; `npm run build` green.
