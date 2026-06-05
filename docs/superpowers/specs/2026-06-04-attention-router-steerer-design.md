# Attention Router (the "steerer") — design spec

**Date:** 2026-06-04
**Status:** Approved design; P1 spec detailed below. P2–P4 are documented as
follow-on specs.
**Repo:** flow-manager (`flow` Go CLI + Mission Control UI)

---

## 1. Summary

Today flow reacts to **deliberate human signals**: you add a `:claude:`
reaction to a Slack message and flow spins up a task + session for that thread
(the reaction-trigger pipeline, skill §10b). That stays exactly as-is.

This spec adds a second, **continuous** lane: an always-on **attention router**
("the steerer") that *passively observes incoming messages across your watched
sources*, triages them with a cheap-to-expensive model cascade, and surfaces the
few that matter — each with full context and a suggested action — for you to
decide. When you've explicitly authorized it and confidence is high, it can act
on its own (forward to an existing task, send an AFK holding-reply); otherwise it
asks.

It is **not** a Slack feature. It is a connector-agnostic attention layer:
Slack is connector #1, GitHub and email (Gmail) are first-class follow-ons, and
any future source plugs into the same machinery.

### The reframe

A steerer that only "reads Slack" is a notifier. A steerer that *closes loops in
your task system* — clears `waiting_on` when the person you were blocked on
replies, turns a message into a fully-formed project-routed task, forwards
context into the right existing task's inbox — is an **operations layer**. We
design toward the operations layer.

---

## 2. Goals / non-goals

### Goals
- Observe a **bounded** set of sources (DMs + @mentions + channels you tick in
  settings) continuously, not just on reaction.
- Triage affordably at firehose volume via a **cheap→expensive cascade** so the
  expensive model sees ~0.1–0.5% of raw events.
- Surface candidates to a **single Attention feed** (Mission Control) + push
  notifications, with full context and a suggested action.
- Let the steerer **act autonomously only under an explicit, per-action,
  confidence-gated policy**; default is surface-only (ask for everything).
- Be **connector-agnostic from day one** — Slack, GitHub, email, future sources
  all normalize into one pipeline.
- **Close loops** in flow's existing task model (`waiting_on`, projects,
  `flow search`, `flow tell`).

### Non-goals (v1)
- Replacing or changing the reaction-trigger pipeline.
- Monitoring *every* channel by default (opt-in allowlist only).
- Autonomous substantive replies by default (must be explicitly enabled).
- Scheduled/cron triage — the cascade runs on the live event stream.
- A separate web app — this lives inside Mission Control.

---

## 3. Locked design decisions

| Lever | Decision |
|---|---|
| **Watch scope** | DMs + @mentions + channels selected via a multi-select checklist in MC settings (populated from `conversations.list`). |
| **Triage model** | Hybrid: cheap funnel gate → deep agent only on survivors. |
| **Cheap stages** | 2-stage funnel (batched relevance gate → context-aware scorer/router). |
| **Surface** | Persistent steerer session (the `flow-attention` task) as the brain + Mission Control "Attention" feed (cards + buttons) + push notifications for urgent/AFK. |
| **Triage execution** | Hybrid: ephemeral **headless** triage per survivor fills the feed; the persistent session executes approved actions + handles conversational follow-ups. The firehose never touches the persistent session's context. |
| **Autonomy** | Surface-only by default; opt-in **per action**, each with its own confidence threshold. |
| **Connectors** | Connector-agnostic spine from day 1; Slack first, GitHub + Gmail follow-ons. |
| **v1 feature set** | Unified cross-connector feed · `waiting_on` auto-resolution · pre-assembled context packs · forward-as-handoff · adaptive feedback loop · presence-aware AFK · urgency/VIP escalation · digest/morning briefing — **sequenced into phases (§9), not cut**. |
| **Inter-task comms** | flow already has async one-way messaging (`flow tell` + same-session wake) and `spawn`/`wait`. P1 uses these as-is. A thin **confirm-handoff** channel (receiver accepts/declines; steerer consults the owning task before forwarding, and re-routes/escalates on decline) is added as a **flow-wide enabler in P2** (§8.8) — *not* a general inter-agent RPC bus. |

---

## 4. Architecture

```
   ┌──────────┐   ┌──────────┐   ┌──────────┐
   │  Slack   │   │  GitHub  │   │  Gmail   │   ← CONNECTORS (pluggable)
   │ listener │   │ listener │   │   MCP    │     each → InboundEvent
   └────┬─────┘   └────┬─────┘   └────┬─────┘
        └──────────────┼──────────────┘
                       ▼
              ┌──────────────────┐
              │  TRIAGE CASCADE  │  Stage 0 rules → 1 Haiku relevance
              │ (connector-blind)│  → 2 Haiku scorer/router → 3 deep agent
              └────────┬─────────┘     + batching · caching · budget guard
                       ▼
              ┌──────────────────┐
              │  ATTENTION FEED   │  SQLite-backed; one row per candidate
              └────────┬─────────┘
            ┌──────────┼───────────┬──────────────┐
            ▼          ▼           ▼              ▼
        MC feed     push        steerer        autonomy
        (cards +   notify      session         engine
         buttons)  (urgent)   (flow-attention)  (per-action gates)
```

### 4.1 Why this is mostly *rewiring*, not new infrastructure

- The Slack Socket listener already emits **every** message
  (`internal/monitor/slack_listener.go:350`); `dispatchMessage()`
  (`internal/monitor/dispatcher.go:112`) merely *drops* messages that aren't
  part of a reaction-tracked thread. The steerer stops dropping them for watched
  sources and routes them into the cascade instead.
- The GitHub polling listener already exists (`internal/monitor/github_listener.go`).
- `inbox.jsonl` + `InboxMonitor` (`internal/monitor/inbox.go`,
  `internal/monitor/inbox_monitor.go`) already provide a durable event queue and
  a wake mechanism.
- `flow done`'s headless KB-distillation sweep already proves the
  ephemeral-deep-agent pattern (one-shot headless Claude emitting structured
  output).
- Operator-identity self-filtering already exists
  (`SelfUserIDs()` `reaction_trigger.go:85`; `FLOW_GH_SELF_LOGINS`).

**Genuinely new:** the cascade classifier, the Attention feed (table + UI), the
autonomy engine, and the connector interface.

---

## 5. Connector abstraction (the future-ready spine)

Everything hangs off one interface so sources are interchangeable. The cascade,
feed, and actions never know which connector an item came from.

```go
// internal/steering/connector.go
type Connector interface {
    Name() string                          // "slack" | "github" | "gmail"
    Events(ctx context.Context) <-chan monitor.InboundEvent // normalized stream
    FetchContext(ctx, ev) (ThreadContext, error) // pull full thread/PR/email on demand
    SendReply(ctx, ev, text string) error  // outward action — see autonomy gate
    Identity() OperatorIdentity            // self IDs/logins for the free self-drop
}
```

- **Slack adapter** wraps the existing `SlackListener` for `Events`, the Slack
  Web API / MCP for `FetchContext` + `SendReply`, and `SelfUserIDs()` for
  `Identity`.
- **GitHub adapter** wraps `GitHubListener`/`GitHubPoller` for `Events`,
  `gh`/GitHub API for context + replies, `FLOW_GH_SELF_LOGINS` for identity.
- **Gmail adapter** wraps the Gmail MCP.

**Invariant:** `SendReply()` is *only ever* invoked by the autonomy engine
(§8), never directly by triage code. This makes "an unwanted message went out as
me" structurally impossible unless the operator enabled that action and
confidence cleared the bar.

`ThreadContext` is a normalized bundle: parent + replies, participants (with
operator-annotation), links, timestamps, and a short pre-summary.

---

## 6. The triage cascade

Coarse→fine, cheap→expensive. Each stage cuts volume by roughly an order of
magnitude.

```
ALL events (Slack + GitHub + email)                       ~1000s/day
  │
  ▼ STAGE 0 — FREE deterministic drops (no LLM)            cuts ~70%
  │   • self-authored (Identity self IDs/logins) — exists
  │   • not in watched channels / not a DM / not a mention
  │   • dedup (channel+ts already seen) — exists in inbox.go
  │   • bot/system/join-leave subtypes; muted people/keywords
  │   • cross-pipeline: already :claude:-reacted or already a tracked task
  │   • THREAD COALESCING: collapse a burst in one thread → one unit
  ▼ STAGE 1 — CHEAP relevance gate (Haiku, BATCHED)        cuts ~80%
  │   "Is this plausibly something the operator needs?"
  │   • 10–20 events per call (structured array output)
  │   • static system prompt + operator profile = PROMPT-CACHED
  │   • output {relevant, rough_category, urgency_hint}
  ▼ STAGE 2 — CHEAP scorer w/ light context (Haiku)        cuts ~50%
  │   pulls thread summary + task/project index; scores
  │   urgency · maps-to-action · candidate matched-task · confidence
  │   routes 3 ways:  drop  |  digest-only  |  → deep agent
  ▼ STAGE 3 — DEEP agent (Sonnet/Opus, headless, per item) ~1–5/day
      full thread read (FetchContext) · KB · flow search · DRAFT reply
      · context pack · final confidence · decide action
      → emits the Attention-feed card
```

### 6.1 Cost mechanics (what makes "watch everything" sane)
1. **Batching** — Haiku classifies 10–20 events per call (time-windowed
   micro-batches, e.g. flush every ~30s or every ~20 events), collapsing
   per-event cost.
2. **Prompt caching** — the classifier's system prompt + the static operator
   profile/task-index are identical every call, so Anthropic prompt caching
   means each batch pays only for the new events. (Follow the `claude-api`
   skill's caching guidance.)
3. **Thread coalescing** — N rapid replies in one thread become one triage unit.

### 6.2 Guardrails
- **Budget backpressure** — an hourly token/cost ceiling. On a spike, the
  cascade auto-tightens (raises the bar to reach Stage 3) and falls back to
  digest-only, **logging what it deferred** (no silent truncation).
- **Short-term verdict cache** — `(thread-key → verdict, TTL)` so Slack
  re-deliveries and the backfill poller don't re-run the cascade on the same
  thing.

### 6.3 Verdict schema (Stage 2 / Stage 3 output)
```jsonc
{
  "source": "slack|github|gmail",
  "ref": { "channel": "...", "thread_ts": "...", "ts": "..." },
  "suggested_action": "make_task|forward|reply|afk_reply|digest_only|drop",
  "matched_task": "kong-split|null",
  "suggested_project": "goniyo|null",
  "suggested_priority": "high|medium|low",
  "urgency": "urgent|normal|low",
  "is_vip": true,
  "confidence": 0.0,
  "summary": "2-line thread summary",
  "draft": "drafted reply text (if reply/afk_reply)",
  "reason": "why this was flagged (explainability)"
}
```

---

## 7. The Attention feed

Durable state, SQLite-backed (queryable, survives restarts, rendered by MC).

```sql
CREATE TABLE attention_feed (
  id            TEXT PRIMARY KEY,         -- uuid
  source        TEXT NOT NULL,            -- slack|github|gmail
  thread_key    TEXT NOT NULL,            -- connector ref, for dedup/coalesce
  summary       TEXT NOT NULL,
  suggested_action TEXT NOT NULL,
  matched_task  TEXT,                     -- slug or null
  suggested_project TEXT,
  suggested_priority TEXT,
  urgency       TEXT,                     -- urgent|normal|low
  is_vip        INTEGER DEFAULT 0,
  confidence    REAL,
  draft         TEXT,                     -- candidate reply, if any
  reason        TEXT,                     -- explainability
  context_json  TEXT,                     -- pre-assembled context pack
  status        TEXT NOT NULL,            -- new|acted|dismissed|snoozed|deferred
  snooze_until  TEXT,                     -- RFC3339, if snoozed
  created_at    TEXT NOT NULL,
  acted_at      TEXT
);
```

- One row per candidate (coalesced by `thread_key`; a new message in an existing
  card's thread updates the card rather than creating a new one).
- `status` drives the UI and the feedback loop (§8.4).

---

## 8. The autonomy engine

A policy table checked before **any** outward effect. Default: everything
surface-only.

```jsonc
// stored in settings (see §10)
{
  "forward_to_task":  { "enabled": false, "threshold": 0.85 },
  "afk_reply":        { "enabled": false, "threshold": 0.90 },
  "create_backlog_task": { "enabled": false, "threshold": 0.80 },
  "send_reply":       { "enabled": false, "threshold": 0.95 }
}
```

- `Connector.SendReply()` and task-mutating actions are **physically gated**
  behind `autonomy.Allow(action, confidence)`; a buggy or jailbroken agent
  cannot post as you unless the operator enabled that action AND confidence
  clears its threshold.
- When an action is *not* allowed, the candidate is surfaced to the feed for a
  manual click instead.

### 8.1 `waiting_on` auto-resolution
When an event arrives from a participant the operator is blocked on (any task
with `waiting_on` naming that person/thread), the cascade flags it and the feed
card offers *"X responded — unblock `kong-split`?"*. On approve (or autonomously
if enabled + confident), run `flow update task <slug> --clear-waiting`.

### 8.2 Pre-assembled context packs
`make_task` cards carry a *drafted brief*, not a raw message: thread summary,
participants, links, **suggested project** (e.g. `#goniyo` → goniyo project via
the project list), suggested priority, and related past tasks (via
`flow search`). One-click accept → fully-formed task via the existing
`flow spawn` + tag path used by the reaction pipeline (`dispatcher.go:617`).

### 8.3 Forward as summarized handoff
`forward` writes a **summarized context block** (not just a link) into the
matched task's inbox via `flow tell <task>` (skill §4.17). The receiving agent
picks it up at its next SessionStart (or in ~2s if its session is live, via the
same-session monitor, §10a).

**P1 behavior:** fire-and-forget. The steerer determines the matched task by
reading its brief/updates and `flow transcript <slug>` synchronously — no reply
needed. **P2 upgrade (via §8.8):** the forward becomes *confirmable* — the
steerer may consult the owning task before forwarding and re-route or escalate
to the operator if the receiver declines.

### 8.4 Adaptive confidence / feedback loop
Every approve/reject/edit/dismiss/snooze is a training signal. Aggregate
per-channel and per-person outcomes; tune that channel/person's effective
threshold over time ("you always dismiss `#random`" → stop surfacing it; "you
always action goniyo asks" → raise its confidence). Persist durable learnings to
`~/.flow/kb/processes.md` and a structured policy store.

### 8.5 Presence-aware AFK
The AFK auto-reply reads the operator's **Google Calendar** (MCP) + Slack
presence: *"He's in a meeting till 3pm — I've flagged this."* Only the
`afk_reply` action (a holding acknowledgement, not a substantive answer) is
eligible for low-friction autonomy; substantive `send_reply` stays stricter.

### 8.6 Urgency + VIP escalation
Stage-1/2 urgency hints + a VIP sender allowlist (settings) route urgent/VIP
items to **immediate push**, bypassing the digest. Everything else can batch.

### 8.7 Digest / morning briefing
A periodic roll-up ("while you were away: 3 DMs, 2 goniyo threads, 1 PR
review"), grouped by source/project, surfaced via the existing Mission Control
greeting infra and aligned with the `flow-standup` backlog idea.

### 8.8 Confirmed handoff (inter-task ack/reply enabler) — P2
A small, flow-wide capability that the steerer consumes; **deliberately scoped
below** a general inter-agent RPC bus.

**What exists today:** `flow tell` is one-way fire-and-forget; the sender never
learns whether the receiver got it, agreed it was relevant, or acted. Raw
messaging is already bidirectional (either task can `flow tell` the other), but
there is **no request→reply with correlation**.

**The addition:** a thin structured handoff with a correlation id and a return
path:
- The steerer (or any task) sends a handoff: *"does this belong to you?"* +
  context, tagged with a correlation id.
- The receiving task's agent replies **accept** / **decline** (with a reason),
  routed back to the sender (woken via the same-session monitor if live).
- The steerer acts on the verdict: **accept** → forward stands; **decline** →
  re-route to the next candidate or **escalate to the operator** in the feed.

**Why it improves the steerer:** turns "forward *only if very sure*" from a
self-estimated similarity score into a **confirmation from the agent that owns
the context** — and closes the loop so mis-routes surface instead of silently
mis-filing. Benefits all orchestration (spawn/tell/wait), not just the steerer,
which is why it's a flow-wide enabler rather than cascade-internal plumbing.

**Explicitly out of scope:** a general bidirectional message bus, multi-hop
agent conversations, or synchronous blocking RPC between live sessions. If
mis-routing proves rare in P1, this can slip further right.

---

## 9. Build order (phased; nothing cut)

Each phase is independently shippable and useful.

### Phase 1 — The spine (this spec's primary deliverable)
- `internal/steering/` package: `Connector` interface + **Slack adapter**.
- 2-stage cheap cascade (Stage 0 rules + Stage 1 + Stage 2) + Stage 3 headless
  deep triage emitting the verdict schema.
- Batching, prompt caching, budget backpressure, verdict cache.
- Attention feed table + Mission Control feed UI (cards + buttons).
- Core actions: `make_task` (context pack), `forward` (handoff),
  `reply` (**surfaced/drafted only — never auto-sent**), `dismiss`.
- Channel-select settings UI (multi-select from `conversations.list`).
- Push notifications.
- **Autonomy: surface-only** (engine present but every action defaults off).
- Cross-pipeline dedup with the reaction pipeline.

### Phase 2 — Autonomy + loop-closing
- Per-action autonomy engine + thresholds (settings UI).
- `waiting_on` auto-resolution.
- `send_reply` (gated) + `afk_reply` with presence awareness (Calendar + Slack
  presence).
- **Confirm-handoff enabler (§8.8)** — thin inter-task ack/reply so forwards are
  confirmable and mis-routes escalate instead of mis-filing. Flow-wide, consumed
  by the steerer.

### Phase 3 — Intelligence
- Adaptive confidence / feedback loop (→ KB + policy store).
- Urgency + VIP escalation.
- Digest / morning briefing.
- Draft-in-your-voice (learn from past Slack reply transcripts + `user.md`).

### Phase 4 — More connectors
- GitHub steering adapter (reuse `FLOW_GH_*` events into the cascade).
- Gmail adapter.
- Snooze/watch actions; explainability audit log ("what did you suppress?").

---

## 10. Configuration (settings)

New settings surfaced in Mission Control (and corresponding env/DB storage read
by `flow ui serve`):

- `steering.enabled` (master switch).
- `steering.watched_channels[]` (multi-select from `conversations.list`).
- `steering.muted_channels[]`, `steering.muted_keywords[]`.
- `steering.vip_senders[]` (per connector: Slack IDs, GitHub logins, emails).
- `steering.autonomy` (the per-action policy table, §8).
- `steering.budget.hourly_token_cap`.
- `steering.digest.interval` + quiet hours.

Operator identity continues to come from `FLOW_SLACK_SELF_USER_IDS` /
`FLOW_GH_SELF_LOGINS` (extended per connector).

---

## 11. UI (Mission Control)

- **Settings page**: channel multi-select; autonomy toggles + per-action
  threshold sliders; VIP list; mute rules; budget cap; digest cadence.
- **Attention feed panel**: candidate cards — source icon, 2-line summary,
  expandable context, suggested action, confidence, urgency/VIP badges, and
  buttons (Approve/Send · Make task · Forward · Snooze · Dismiss) + a "why
  flagged?" explainer. Deterministic actions execute server-side; reasoning
  actions (re-draft, explain) route to the `flow-attention` session.
- **Push**: urgent/VIP/AFK items push immediately.

---

## 12. Key files

**New (`internal/steering/`):**
- `connector.go` — `Connector` interface + `ThreadContext`, `OperatorIdentity`.
- `slack_connector.go` — Slack adapter over existing listener + Web API/MCP.
- `cascade.go` — Stage 0/1/2 orchestration + thread coalescing.
- `classifier.go` — batched, prompt-cached Haiku calls (relevance + scorer).
- `triage_worker.go` — Stage 3 headless deep-agent runner (structured output).
- `verdict.go` — verdict types.
- `budget.go` — token/cost backpressure + verdict cache.
- `feed.go` — Attention feed store (CRUD over the new table).
- `autonomy.go` — policy table + `Allow(action, confidence)` gate.

**Modify:**
- `internal/monitor/dispatcher.go` — route watched-source messages into the
  cascade instead of dropping (`dispatchMessage()` ~:112).
- `internal/flowdb/db.go` — `attention_feed` table + migration; settings storage.
- `internal/app/serve.go` — wire the steering cascade into `flow ui serve`.
- Mission Control React UI — settings page + Attention feed panel.

**Reuse unchanged:** `slack_listener.go`, `github_listener.go`,
`inbox.go`/`inbox_monitor.go`, `slack_backfill.go`, `spawner.go`.

---

## 13. Testing

- **Cascade unit tests** (table-driven): Stage 0 rules (self-drop, allowlist,
  dedup, coalescing) with no network; classifier calls behind a function-var
  mock (matches the repo's `iterm.Runner` mock convention — CLAUDE.md).
- **Verdict → feed**: a fake verdict produces the expected `attention_feed` row
  and card payload.
- **Autonomy gate**: `Allow()` truth table across enabled/threshold/confidence;
  assert `SendReply` is never reachable when disabled.
- **Cross-pipeline dedup**: an event already tracked by the reaction pipeline is
  dropped at Stage 0.
- **No real network / no real model** in tests — classifier + connector calls
  mocked via package-level function vars; real SQLite in a temp `$FLOW_ROOT`
  (per repo conventions).

---

## 14. Open questions (resolve at planning / P1)

- **Feed action execution split**: which actions run purely server-side (Go) vs
  must route through the `flow-attention` session? (Proposed: deterministic
  server-side; reasoning actions to the session.)
- **Settings storage**: new SQLite table vs a config file read by `serve` — pick
  one consistent with current MC settings handling.
- **Classifier model/cost calibration**: confirm Haiku batch size + prompt-cache
  layout against a real day of your traffic before locking thresholds.
- **Steerer session lifecycle**: keep `flow-attention` always-live, or
  spawn-on-demand when a card needs reasoning? (Leaning: live but lean, since the
  firehose never enters its context.)
- **Digest delivery channel**: MC panel only, or also a self-DM / email?
```