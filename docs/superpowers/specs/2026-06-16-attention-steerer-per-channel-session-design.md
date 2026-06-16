# Attention Steerer — per-channel live session (modeled as a chat)

**Date:** 2026-06-16
**Status:** LOCKED (2026-06-16). All decisions resolved to their defaults; implementation plan next.

## Problem

The attention steerer triages each inbound Slack/GitHub event with a **stateless**
`claude -p` deep-triage call (`steering/triage.go`, invoked at `cascade.go:597`).
Because every call is a cold single-message reasoner, an entire apparatus exists
*only to simulate a memory the model doesn't have*:

- `clubbing.go` — a separate Haiku matcher grouping same-conversation cards, working
  on **degraded summaries** (not raw text).
- `thread_state.go` + the `IncrementalContext` prompt scaffolding — reconstruct a
  "running understanding" and re-feed it every event.
- `retrieval.go` — pulls cross-conversation/KB history each call.

This is where the bug class breeds. Concrete failure (2026-06-16, `#facets-coinswitch`):
two **separate top-level channel posts** —

1. Rohit → Nayan: *"can we get superapp code repo access to evaluate the best alternative for dynamodb there"*
2. Nayan → Rudraksh: *"can you list the repo names for **this**"*

— surfaced as two unrelated cards. Message 2's `this` (= the superapp repo) was
resolved-away into "an unspecified item" by isolated triage **before** the clubbing
matcher ran, so the matcher had no concrete link and refused to merge.

### Root cause

Statelessness. The compensators all operate on lossy inputs. The fix is to give the
deciding reasoner **memory at the conversation grain**.

### Drivers (operator-confirmed)

1. Routing/grouping correctness (fragmentation, unresolved `this`, wrong-task forwards).
2. Collapse the compensating machinery.
3. Assistant-like memory ("watched the channel all day, just remembers").

Cost is **not** a primary driver — acceptable to trade for the above.

## Approach — per-channel live session, modeled as a `chat`

Each active channel (Slack channel/DM/MPDM) and each active GitHub PR/issue gets one
**long-running, detached Claude session**, recorded as a row in the existing `chats`
table and keyed by a deterministic slug. This reuses, wholesale, the stack flow
already has for the Slack command channel (`server/chat_sink.go`):

- **Slug:** `chat-steer-<sanitized-key>` (channel id for Slack; `gh-<repo>-<num>` for
  a PR/issue), via the existing `sanitizeSlugSegment`. Same key → same chat → reused
  across events.
- **Launch:** detached free-agent floating session (`registerFloatingLaunch` +
  `startFloatingDetached`), primed once with the triage brief (routing rubric +
  autonomy rules + how to surface cards). Resume from the stored `session_id` when the
  PTY is gone (`resumeSlackChat` pattern).
- **Model:** launched via `agentTerminalArgs` with **no `--model`** flag, so it uses
  the operator's default (Opus). We do NOT reuse `classifierPool` (which pins Haiku).
- **Feeding events:** Stage-0 survivors are delivered into the live session with
  `wakeTask(slug, payload)` — the same path `flow tell <slug>` uses; it is slug-keyed
  and task/chat-agnostic.

### Why this reuses the existing stack (the whole point)

| Requirement | Satisfied by |
|---|---|
| Channel = a session that holds context | A `chat` row + one live detached session per key |
| Default model (Opus), not pinned | `agentTerminalArgs` launches `claude` with no `--model` |
| Run KB like other long-running sessions | `kbDistiller.candidates()` (kb_distill.go:176) **already sweeps chats with a session_id** — free |
| Get per-session context usage (1M window) | The token panel already computes per-session occupancy from the transcript JSONL |
| Send `/compact` at 60% | `/compact` is interactive-only → a live PTY chat session takes it via `wakeTask(slug, "/compact")` |
| "It's a chat, not a task — how does tell work?" | `wakeTask` is slug-keyed; chats already use it (chat_sink.go:64). `flow tell` documents this for agents. |

## Event flow (replaces cascade Stage 2/3)

1. Inbound event → **Stage 0 gate** (Go, cheap, unchanged). Drops org-firehose noise.
2. Survivor → resolve session key → ensure that channel's steerer chat is live
   (start fresh, or resume from `session_id`; lifecycle below) → `wakeTask(slug,
   payload)`. Payload = the new message + a lean deterministic context pack
   (permalink, parent, participants — anchors the *specific* message).
3. The session (holding the channel's memory) decides routing/grouping/draft and
   **calls a flow tool** `surface_attention_card` with
   `{thread_key, action, matched_task, summary, draft, confidence, reason}`.
4. The tool (Go) **validates** the proposed `thread_key` against open cards for that
   key (`flowdb.ListOpenClubCandidates` + `anchorIndex`), applies the **`ApplyAction`
   autonomy gate**, then `writeFeed` + emits the Trace. Autonomy stays server-side.
5. A **context-occupancy monitor** sends `/compact` when the session crosses 60% of
   1M (idle-gated, below).
6. `kbDistiller` sweeps it on its own cadence (no new code).

## Inbound delivery & layering (how an event reaches the right session)

The dispatch tree is unchanged up to the steerer: `Dispatch` drops self-authored bot
echoes (`IsSelfAuthoredSlack`), routes reactions and the operator's bot-DM command
channel (`ChatSink.OpenOrContinueChat`) as today, and hands everything else to
`observeWithSteerer → Cascade.Observe(ev)` when `steererOwnsRouting()`.

**The layering constraint:** the per-channel session lives in `internal/server` (the
terminal hub / floating sessions / `wakeTask`), but the triage decision lives in
`internal/steering`, and `server` imports `steering` — not the reverse. So the cascade
**cannot** start/resume/wake a session directly. It crosses the boundary through an
**injected interface**, exactly as the command channel already does
(`monitor.ChatCommandSink` is implemented by `*server.Server` and injected as
`d.ChatSink`).

- **New interface** `SteererSessionSink` (defined in steering/monitor, implemented on
  `*Server`, injected at wiring): `DeliverToChannelSession(key string, payload …) error`.
- **Cascade side:** `Observe` runs Stage 0, resolves the session key, builds the
  payload, and calls the sink. No terminal-hub knowledge in steering.
- **Server side:** ensure the `chat-steer-<key>` session is live (running → `wakeTask`;
  PTY gone → resume from `session_id` → `wakeTask`; no row → start detached, no
  `--model`, record the chat row); idle sessions sleep on a TTL and resume on demand
  (no cap).

**Session-key resolution (reach the CORRECT chat — deterministic):**

- Slack channel / DM / MPDM → `chat-steer-<sanitized channel id>`.
- GitHub PR/issue → `chat-steer-gh-<repo>-<canonical-num>`. If the PR/issue is linked
  to a counterpart (closes/fixes refs, or flow's issue↔PR cross-linking), resolve to
  the shared **canonical** number BEFORE keying, so a PR and the issue it closes reach
  the same chat instead of forking a new one.
- SharedRef forward (a reply forwarded into another conversation) → key on the
  **origin** channel (`ref.Channel`), so it reaches the origin channel's session —
  mirrors the existing `routeViaSharedRef`.
- Self-authored bot echoes are already dropped at `Dispatch` top.

Same source → same slug → same chat, always.

### Self-authored messages: feed, don't drop (context-only)

Today `Dispatch` hard-drops `IsSelfAuthoredSlack` at the top (dispatcher.go:101) and
the steerer drops operator-authored messages from surfacing. In the session model that
starves the session's memory: if it never sees what the operator said in the channel,
it mis-reasons about every follow-up. So self-authored messages are **fed to the
channel session, never surfaced** — two cases, both context-only (no card, no reply,
no auto-act):

- **Operator-human self** (`SelfUserIDs()`/`operatorUserID()` — the operator's own
  typed messages): fills the context the agent lacks (decisions/answers the operator
  gave directly in the channel). The primary case.
- **Bot self-echo** (`IsSelfAuthoredSlack` — a reply flow sent via SendAsBot, echoed
  back): fed as a **delivery confirmation** so the session knows the thread advanced
  and stops re-nagging. Carries a marker (like `kbCheckpointMarker`) so it counts as a
  NON-genuine turn — it can never trigger a new verdict or a KB checkpoint (loop guard).

**Dispatcher change:** the top-of-`Dispatch` `return nil` for self-authored becomes a
route to a **feed-only path** that bypasses the command-channel decline logic (the
original reason for the early drop) and the surfacing path, delivering the event to the
channel session marked `context_only`. `Stage 0` likewise routes self-authored as
context-only feed rather than dropping. Backfill (`slack_backfill.go:274`) feeds too,
so post-restart replay includes operator-own messages and delivered replies. The
session is primed to treat `context_only` turns as memory updates only — absorb, never
surface or reply.

### Chat deletion (from the UI)

Deleting a steerer chat must (a) not orphan a running process and (b) have clear
semantics. Reusing the existing chat-deletion model (`DeleteChat` soft-deletes;
`GetChat` treats deleted as absent; `UpsertChat` reclaims the tombstone → reopen fresh):

- **Stop the live PTY.** The lifecycle manager reconciles with `chats`: a chat whose
  `deleted_at` is set has its floating session torn down (`stopFloating`) — never leave
  a `claude`/`codex` process running for a deleted chat. An in-flight delivery/fork on
  that slot is abandoned gracefully.
- **Semantics = reset, not stop (LOCKED).** Delete means "forget and start over": the
  next inbound event on that channel reopens a **fresh** steerer chat (new session,
  clean memory, tombstone reclaimed). Useful when a session's memory got polluted. It
  does NOT stop steering the channel — monitoring is configured separately, so **to stop
  steering, unmonitor or mute the channel**, don't delete the chat.
- **Open cards survive.** Attention cards live in `attention_feed` keyed by `thread_key`,
  not by the chat — deleting the chat resets the *session memory*, not the surfaced
  cards. The fresh session re-validates against the still-open cards. (GAP-14.)
- **Workers auto-skip.** `kbDistiller`/`compact` enumerate via `ListChats`, which
  already excludes deleted rows — so a deleted chat drops out of both with no extra code.

### Flow (ASCII)

```
Slack/GitHub ─▶ Dispatcher.Dispatch ─▶ (self-authored ▶ context-only feed [no card];
                  reaction▶task; operator bot-DM ▶ ChatSink) ─▶ observeWithSteerer
                                                      │
  steering │  Cascade.Observe: Stage0 ─drop▶ Trace
           │     resolve session key (chan / gh-PR / sharedref-origin)   [GAP-4]
           │     build payload {msg + ts/thread_ts + ctx pack}           [GAP-8]
           │        └─▶ sink.DeliverToChannelSession(key, payload)        [GAP-1]
  ═════════╪══════════ injected interface (mirror ChatSink) ═════════════
  server   │  SteererSessionSink: running▶wakeTask | gone▶resume | new▶start(no --model)
           │     idle-sleep PTY (TTL), keep row; resume on demand        [GAP-5]
           │        └─▶ per-channel live chat session (Opus, holds memory)
           │               └─ tool: surface_attention_card{key,action,draft,thread_ts} [GAP-3]
           │                    └─ validate key vs open cards · ApplyAction gate
           │                         └─ writeFeed▶card▶Trace(async)       [GAP-7]
           │                         └─ reply: slack send --thread-ts     [GAP-2]
           │  kbDistiller(chats)=free · compactMonitor ≥60%&idle▶/compact [GAP-6]
```

## Locked decisions

1. **Verdict path: agent calls a flow tool.** The session surfaces/updates cards via
   `surface_attention_card`; `ApplyAction` autonomy gating lives **in the tool**
   (server-side), matching the existing "agent sends replies via MCP" direction. The
   Trace is built from the agent's tool calls + its `reason`. (Rejected: parsing the
   verdict out of the transcript — async/timing-fragile, couples Go to transcript
   format.)
2. **Grouping authority: session proposes, Go validates.** The session passes the
   `thread_key` it's continuing; the tool validates it against the channel's open
   cards before trusting it. A model slip can't merge into a foreign/nonexistent card.
3. **No session cap — idle-sleep, not LRU.** A Claude Code session is just a resumable
   transcript + chat row, so the *number* of sessions is unbounded (one per channel).
   The only bound is live PTY processes: a session quiet for a TTL goes **cold** (PTY
   torn down via `stopFloating`; `chat` row + `session_id` survive) and **resumes**
   instantly on its next event. "Warm" = PTY running; "cold" = resume on demand. This
   is resource hygiene, not a cap — any channel can be live; idle ones just don't pin a
   process. (Always-warm is an opt-in if resume latency ever matters.)
4. **GitHub grain: per-PR/issue, steerer only for UN-OWNED activity; linked issue↔PR
   share ONE chat.**
   - **Owned routes directly to the work-session.** When a `gh-pr:`/`gh-issue:` task
     already owns the PR/issue (you're actively working it in a worktree), GitHub events
     route to **that task's session** (existing skill §10c behavior) — it has the
     worktree + code context. The dispatcher checks task-ownership BEFORE the steerer, so
     an actively-worked PR is never double-handled by a separate steerer chat. This is
     the same principle as Slack (owned thread → owning session; un-owned → steerer).
   - **Un-owned activity triages in a steerer session** (review request on someone's PR,
     a mention, an issue you haven't picked up): a per-PR/issue steerer session triages
     and surfaces a card (which may itself propose make_task → which then becomes the
     work-session).
   - **Linked issue↔PR share ONE chat.** A PR and the issue it closes are one
     conversation — never a duplicate. The session key resolves through the link to a
     **canonical key**: via GitHub closes/fixes/linked-issue refs or flow's issue↔PR
     cross-linking (`flow-github-pr-tracking`), key both to the canonical
     `chat-steer-gh-<repo>-<canonical-num>` BEFORE ensuring the session. Unlinked
     PRs/issues key on their own number.
5. **UI: link card → chat.** Each attention card links to its channel's chat session
   (open/inspect on the Chats page); add a "view session" affordance + a context-usage
   indicator on the card/trace. No full Attention+Chat merge now.

## Work items (gap index)

The complete new/changed surface, cross-referenced by the `GAP-N` tags used above:

| # | Item | Layer |
|---|------|-------|
| GAP-1 | `SteererSessionSink` interface (cascade → server), injected like `ChatSink` | steering + server |
| GAP-2 | `--thread-ts` on `flow slack send` + `/api/slack/send`; tool draft carries `thread_ts` | app + server |
| GAP-3 | `surface_attention_card` tool: verdict-out, `thread_key` validation, `ApplyAction` gate, `writeFeed` | server/steering |
| GAP-4 | Session-key resolution: channel/DM/MPDM, GitHub **canonical** PR↔issue, SharedRef origin | steering |
| GAP-5 | Per-channel session lifecycle: start / resume / idle-sleep (no cap) + slot state machine | server |
| GAP-6 | Context-occupancy `/compact` monitor worker | server |
| GAP-7 | Async Trace built from the agent's tool calls | server/steering |
| GAP-8 | Event payload `{channel, ts, thread_ts, ctx pack, context_only}` | steering |
| GAP-9 | Provider fork Claude→Codex: transcript hand-off, slot `forking` state | server |
| GAP-10 | Self-authored = feed-only (dispatcher rewire + Stage 0 + backfill) | monitor + steering |
| GAP-11 | Configured/per-key default provider + settings UI | flowdb + server + ui |
| GAP-12 | Chat token/cost on Chats page + "Steering" analytics slice | server + ui |
| GAP-13 | Chat rename (`SetChatTitle` + route + UI) + auto-naming convention (+ external-org) | flowdb + server + ui |
| GAP-14 | Chat deletion: stop PTY, reset-and-reopen semantics | server |

## Context-occupancy → `/compact` worker (new)

A small worker mirroring `kbDistiller`'s structure:

- Per steerer chat, compute context occupancy from the transcript (reuse the token
  panel's existing per-session occupancy calc) as a fraction of the 1M window.
- When occupancy ≥ **60%** AND the session is **idle** (transcript quiet ≥ idle, same
  jsonl-mtime signal `kbShouldWake` uses — never compact mid-turn), call
  `wakeTask(slug, "/compact")`.
- Cooldown after a compact so it doesn't re-fire while the post-compact transcript
  settles. Threshold + cooldown are env-tunable (`FLOW_STEERER_COMPACT_PCT`, default 60).

## Provider fork — Claude → Codex on token exhaustion (new)

A per-channel session normally runs Claude (Opus). When Claude token utilisation is
exhausted for that session, it **forks to Codex** and continues — seamless from the
triage point of view. This is the escalation rung above `/compact`: compact relieves
context pressure; fork handles *usage/quota* exhaustion that compact can't.

**Hand-off — rendered transcript, not native resume.** No literal cross-provider
resume exists, so the fork hands Codex a **rendered transcript** of the Claude session
as priming. flow already has this: `flow transcript <slug> --compact` renders any
session's JSONL — Claude OR Codex (`renderCodexRecord` / `renderAssistantRecord`) — to
a clean conversation log, omitting tool-result noise and thinking blocks. Codex reads
it and reconstructs what was done (the messages + which tools were called = the
actions). The render is **deterministic** (reads the file, no model call), which
matters because the Claude session may be the very thing that's out of budget — we
can't rely on asking it to summarize itself.

**Token caveat (the reason we fork).** We fork because token utilisation is exhausted —
the Claude context is already large. Dumping the entire history verbatim re-creates the
same pressure on Codex and is expensive to transfer. So:
- Small session → feed the whole `--compact` render.
- Large session → **layer it**: handoff summary (reuse the last `/compact` summary) +
  recent verbatim tail + the structured digest (open cards/decisions).
Either way the structured digest (open cards + decisions, also the restart-rehydration
record) rides along so the fork is grounded in concrete state, not just prose.

**Trigger (LOCKED).** The Claude session process returns a usage / quota / rate-limit
error (or occupancy stays high after `/compact` cannot help). A configurable per-period
cost/usage ceiling is a later optional add. Manual flip also supported. Gated by
`FLOW_STEERER_FORK_PROVIDER`; **one-way (Claude→Codex) for v1** — switching back when
Claude recovers is a later toggle.

**Mechanism (reuses existing machinery):**
1. The lifecycle manager marks the channel slot `forking` (delivery holds — below).
2. Ensure the durable digest is current (updated on every verdict anyway).
3. Tear down the Claude PTY (`stopFloating`).
4. Set `chats.provider = codex`, clear `session_id` (Codex assigns its own on launch —
   `NeedsCapture`, captured post-hoc via `SetChatSession`).
5. Relaunch via `agentTerminalArgs("codex", …)`, primed from the digest.
6. Mark the slot `live(codex)`; resume feeding.

**Inbound delivery understands the switch.** `SteererSessionSink.DeliverToChannelSession`
already reads `chats.provider`, so it branches start/resume on provider (claude
`--resume <id>` vs Codex resume) with no extra signal. The per-channel slot carries a
state — `{none, starting, live(claude|codex), forking}` — and during `forking`,
delivery **waits** (per-channel deliveries are already serialized) until the slot is
`live` again, then feeds the new session. No event lands in the dying Claude PTY or is
lost. KB distillation and the `/compact` monitor are provider-agnostic (they key on the
chat row + transcript), so they keep working across the fork.

**Configured default provider + per-key override (new setting).** Which provider a
channel/DM/PR spawns with is resolved as `override[key] ?? global default` — global
default `FLOW_STEERER_DEFAULT_PROVIDER` (default `claude`), with optional per-key
overrides. Both are editable in the settings UI (a provider dropdown alongside the
monitored-channels list). A **manual** provider change on an existing channel uses the
SAME switch path as an auto-fork (mark `forking` → teardown → flip `chats.provider` →
re-prime from the rendered transcript), just operator-triggered and in either
direction (Codex→Claude too). Resolution rule: the **configured default** applies when
a chat is first created; once switched (auto-fork or manual), `chats.provider` on the
row is authoritative for resume until changed again. So the auto-fork and the setting
are one mechanism with two triggers.

## Memory feed scope (default, reversible)

The session sees only **Stage-0 survivors** we feed it, not every raw channel message
— Stage 0 stays a real filter (it controls cost and keeps the firehose out). Reference
resolution for a non-survivor referent is covered by (a) the per-event context pack
and (b) the agent's own bounded fetch (the merged `triage.go` artifact-fetch rule).
If full-channel memory proves necessary, feeding all messages is a later toggle.

## Slack thread routing (session grain ≠ card grain ≠ reply target)

Slack events carry `ThreadTS` (defaults to `TS` for a top-level post; the parent's ts
for a threaded reply — `inbound_event.go:28`). Three grains must stay separate:

- **Session key = channel.** Every message in a channel — top-level OR any thread — is
  fed to that channel's one steerer chat. Thread-vs-top-level does NOT pick the
  session. (Per-channel, not per-thread, is what links the coinswitch case: two
  *separate top-level posts* have different `ThreadKey`s; a per-thread session would
  split them.)
- **Card key = thread (default), session-overridable.** The card's `thread_key`
  defaults to `ThreadKey(channel, ThreadTS)` so dedup / mutes / routing stay
  per-thread. The session may *propose* an existing card's `thread_key` to merge
  (the coinswitch merge); Go validates against the channel's open cards.
- **Reply target = the message's `ThreadTS`.** Each event is fed to the session WITH
  `{channel, ts, thread_ts}`; the session echoes `thread_ts` back in its tool call so
  the reply lands in the originating thread (threaded reply → that thread; top-level
  post → thread on that message). Because the session is async and multiplexes threads,
  the target travels per-event in the tool call — never "the last message."

**Gap to close:** `flow slack send` posts to channel root only (no thread arg). Add a
`--thread-ts` target on the CLI and `/api/slack/send`; the dispatcher already threads
agent replies via `thread_ts` (`dispatcher.go:503`), so this exposes that on the send
path the session uses. `surface_attention_card`'s draft likewise carries `thread_ts`.

## What is removed / shrinks (driver #2)

- **`clubbing.go` — deleted.** The session remembers msg 1, so msg 2 ("…for **this**")
  is a follow-up turn and the session reuses the existing card's `thread_key`. The
  `anchorIndex` helper relocates to the `surface_attention_card` validation path.
- **`IncrementalContext` + incremental prompt directives — deleted.** A live session
  is inherently incremental.
- **`thread_state.go` — shrinks** to a thin durable rehydration record (last N
  verdicts + open-card `thread_key`s per key) seeding a cold resume; no longer feeds a
  prompt.

## What stays (and why)

- **Stage 0 gate** — unchanged, load-bearing: never wake/feed a session for noise.
- **Deterministic context pack** (`context_fetch.go`) — still sent per event as the
  ground-truth anchor for the specific message; can be leaner now.
- **`retrieval.go`** — stays, scoped as **cross-channel** memory (session = within-
  channel; retrieval = across). Surfaced to the session via the brief / a tool, not a
  per-call prompt block.
- **`writeFeed`, `ApplyAction`, the Trace** — unchanged downstream; the verdict now
  arrives via the tool call instead of parsed stdout.

## Restart / crash

Boot with no live steerer sessions. First event per key resumes from the durable
`chats.session_id` (+ rehydration record). Memory survives as a session transcript +
seed, not a live process — the same persistence flow already has. `SlackBackfill`
still covers the restart gap for missed events.

## Error handling (fail-open, unchanged invariant)

If the session can't be started/resumed/fed, fall back to today's stateless
`DeepTriageIncremental` cold `claude -p` for that single event. A session failure must
never drop a card — same fail-open invariant as the clubbing fix and the existing
deep-triage fallback (`cascade.go:598-602`).

## Cost

Prime sent once per session; events are compact turns. Idle-sleep + `/compact` bound the live
footprint; `/compact` at 60% bounds per-session context. Feeding only Stage-0
survivors (not the raw firehose) keeps token spend proportional to operator-relevant
traffic.

## UI (folds in here, minimal)

- Attention card → "view session" link opening the channel's chat on the Chats page
  (reuse the existing chat view; the steerer chat is just a chat).
- Context-usage indicator on the card/trace (the same occupancy % the compact worker
  reads).
- No Attention+Chat page merge now (deferred).

## Chat naming & rename

Steerer chats need a human-readable name so the operator knows which channel is which
(the slug `chat-steer-<id>` is an internal key, not a display name). `Chat.Title`
already exists; add a convention + rename.

**Auto title (convention)** — derived at creation via the existing channel/user name
resolution (the same the prompts use to "refer to people and channels by name"):

| Source | Auto title |
|---|---|
| Slack channel | `#facets-coinswitch` |
| Slack DM (im) | `DM · Nayan Kalita` (internal) / `DM · Nayan Kalita (Peepalco)` (external) |
| Slack MPDM | `Group · Nayan, Rohit (Peepalco), …` |
| GitHub PR/issue | `flowwyyy#17 · ask for missing artifacts` |

**External-org tagging:** when a DM/MPDM partner is from outside the operator's
workspace (Slack Connect — their `team_id` ≠ the operator's workspace team), append
their org name in parens, e.g. `(Peepalco)` — resolved via Slack team lookup (the same
signal behind Slack's "external people from <org>" label). Internal contacts carry no
suffix. This applies per external participant in MPDMs.

**Slug stays the stable key, title carries the name.** The slug keys on the immutable
channel/PR id; a Slack channel rename must never orphan the session, so only the title
tracks the name — never the slug.

**Steerer vs other chats:** `origin = "steerer"` lets the UI badge/group these as
"Steering," distinct from UI ("Ask Flow") and Slack-command chats — no title prefix
needed.

**Rename:** add `flowdb.SetChatTitle(slug, title)` + a server route + a rename
affordance on `Chats.tsx`. A user-set title is **sticky**: auto-refresh the title only
while it still equals the auto-derived value (mirror `refreshSlackTaskTitleIfLegacy`),
so a Slack channel rename updates the auto title but never clobbers a custom one.
(GAP-13: chat rename + naming convention.)

## Token & cost accounting (chat page + analytics)

Steerer sessions are chats with a `session_id`, so the existing per-session pricing
(`pricing.go`: `transcriptTokenUsage → billedCostUSD`, dedup by `(msgid,reqid)`,
full-bill incl. cache) **already covers them** — unlike the OLD headless `claude -p`
steerer, which had no session id and was invisible to the panel. The work is surfacing
plus tagging, not new accounting. Bonus: KB-checkpoint and `/compact` turns happen
inside the session transcript, so that spend rolls into the chat's cost automatically.

- **Tag steerer chats** distinctly (`origin = "steerer"`, alongside the `chat-steer-`
  slug prefix) so analytics can slice them apart from UI/Slack chats.
- **Chat page:** attach `tokens` + `cost_usd` per chat in the chat ui_data (the same
  computation Sessions/tasks use) and render it on `Chats.tsx` / chat detail — "just
  like sessions."
- **Overview activity container:** include chat/steerer tokens in the existing weekly
  heatmap + totals (`tokensByWeek` / `TokenDay`), and add **"Steering"** as its own
  slice in the breakdown + per-provider totals. The always-on steerer is ongoing
  background spend the operator needs to watch — the analytics double as a cost-control
  feedback loop. The existing per-provider split naturally shows a channel's spend
  across Claude AND Codex after a fork.

**Coverage caveat:** the cold-call FALLBACK path (stateless `claude -p`, no session id)
stays uncounted — acceptable; it's the rare degraded path, and steady-state session
spend is fully covered. (GAP-12: chat token/cost surfacing + "Steering" analytics slice.)

## Rollout (incremental, safe on a live system)

1. Add the `surface_attention_card` tool with the `ApplyAction` gate inside it
   (server-side); no behavior change yet.
2. Add the steerer-chat launcher (deterministic slug, triage brief, no `--model`) and
   wire Stage-0 survivors → `wakeTask`, **behind `FLOW_STEERING_SESSIONS`** (default
   off). Keep `DeepTriageIncremental` as the live fallback.
3. Add the context-occupancy `/compact` worker.
4. Enable for **Slack channels first**; validate grouping/routing on coinswitch-style
   cases via the live Trace + chat view.
5. Extend to GitHub PR/issue grain.
6. Only after the session path proves out on real traffic, run the **Code cleanup**
   below. Both paths coexist until then.

## Code cleanup (staged — delete only when the path it serves is gone)

A first-class deliverable, not an afterthought. Staged so nothing load-bearing dies
early. After each deletion: `go build ./... && go test ./...` must stay green, and
`flowdb` migrations must drop any now-orphaned tables (per the repo's migration
drop-list convention) rather than leaving dead schema.

**Tier 1 — removable once the session path is default-on for a surface** (the clubbing
machinery has no remaining caller):
- `internal/steering/clubbing.go` + `clubbing_test.go` — entire file: `maybeClub`,
  `DedupeOpenFeedConversations`, `dedupeDMByGap`, `dedupeChannelByMatcher`,
  `toClubCandidates`, `feedTSWithinGap`, `defaultConversationMatcher`,
  `parseConversationMatch`, `clubPrime`, `clubPayload`, `clubbedThreadKeyForReply`.
  **Relocate** `anchorIndex` (and keep `flowdb.ListOpenClubCandidates`) into the
  `surface_attention_card` validation path.
- The `Cascade.MatchConversation` field + its wiring.
- The `FLOW_STEERING_DEDUPE` boot-dedupe path and its config.
- The `maybeClub` call site in `cascade.go:1289` (the raw-text fix lands here too).

**Tier 2 — removable only if/when the cold-call fallback is also retired** (these
still back the `DeepTriageIncremental` fallback while it exists; do NOT delete while
the fallback lives):
- Incremental scaffolding in `triage.go`: `IncrementalContext`, the incremental /
  corrections / prior / retrieved prompt blocks, `modelFacingIncremental`. Keep a
  minimal `DeepTriage`/`DeepTriageWithContext` cold path as the fallback.
- `thread_state.go`: collapse to the thin rehydration record; drop `priorUnderstanding`
  prompt projection and the prompt-feeding reads once nothing consumes them.
- `retrieval.go`: keep the retrieval data, drop the per-call prompt-block plumbing once
  the session consumes cross-channel context via its brief/tool instead.
- Re-evaluate `retriage.go` (re-triage of an existing card) — likely subsumed by the
  session re-deciding on the next turn.

Whatever ends up with zero references after the migration is deleted, not left dormant
— except the `brain_runs` ledger and other intentionally-dormant tables the repo
documents as load-bearing.



## Testing

The chat/floating machinery has existing seams; add:

1. Two messages, same channel → second `wakeTask`s the same live session (not a new
   one), one card, reused `thread_key` (the coinswitch regression).
2. Session absent/PTY gone → resume from `session_id`; an idle session sleeps (PTY
   torn down) and the next event resumes it. A deleted chat reopens fresh (new session,
   tombstone reclaimed); a linked issue↔PR event reaches the canonical chat, not a new one.
3. `surface_attention_card` validates a `thread_key` not among the channel's open
   cards → rejects the merge, opens a fresh card; and applies the autonomy gate.
4. Occupancy ≥ 60% AND idle → one `/compact` tell; not fired mid-turn; cooldown holds.
5. Session start/feed failure → cold `claude -p` fallback still surfaces a card.
6. Stage 0 gates before any session wake (no chat created for dropped events).

## Explicitly out of scope (YAGNI)

- A single global session (serial bottleneck, SPOF, unbounded context).
- Cross-channel memory beyond `retrieval.go`.
- Feeding the full raw channel (non-survivor messages) into sessions — later toggle.
- Auto-sending drafts — surface-only autonomy is unchanged.
- Full Attention+Chat UI merge.
