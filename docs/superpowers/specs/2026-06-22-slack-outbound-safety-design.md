# Slack outbound safety — design

Date: 2026-06-22
Status: approved, in implementation

## Problem

Three related failures let outbound Slack messages escape without the operator's
input. They are one theme: **nothing goes out, and no question gets answered,
without the operator.**

1. **Auto-answered prompts (root cause of the incident).** When a Claude/Codex
   session is blocked on a question it asked (`AskUserQuestion`) or a permission
   prompt, an incoming inbox event "wakes" the session by injecting a
   bracketed-paste prompt **and an unconditional `\r` (Enter)**
   (`terminal_wake.go:submitAfterPaste`). That Enter lands on the open selector
   and submits the highlighted (first) option — answering a question the
   operator never saw. In the incident this fired off a Slack reply with no
   operator input.

2. **No external-channel send gate.** flow has *zero* detection of channels that
   include people outside the operator's org (Slack Connect / cross-workspace),
   and the send path (`flow slack send` → `/api/slack/send` → `SendAsThread` →
   `SlackWriter.PostMessage`) has no operator-approval requirement for them.

3. **Bot-sounding messages.** Outbound replies read like a bot, not the operator.
   There is no configured voice/persona.

## Part A — Wake fix (ship first; active incident)

**Rule (operator-specified):** wake is safe when the agent is `running`, `idle`,
or waiting on something that *isn't the operator* (e.g. an outbound/network
request). Wake must be **held only** when the agent is waiting on **operator
input** — a question it asked or a permission prompt.

flow already records *why* a session is waiting (`agent_hooks.go`): the
`ask_user_question` tool → `elicitation`; `permission_request` /
`permission_prompt`; versus `idle_prompt` (really idle) and `running`. Today all
of these collapse into a coarse `"waiting"` status that no wake path consults.

Design:

- **`awaitingHumanInput(state)` predicate** — true iff the recorded runtime
  reason is `elicitation`, `permission_request`, or `permission_prompt`.
  Explicitly **false** for `idle_prompt`, `idle`, `running`, `released`, and any
  non-operator wait.
- **Gate every wake path** before injection — `inbox_notify.wakeTaskForInboxNotify`,
  `task_monitor` inbox delivery, and `steerer_session.postApprovedReplyViaChat`
  (plus the other steerer chat injectors). When `awaitingHumanInput` is true:
  **inject nothing** (no paste, no `\r`) and **buffer the wake** for that slug.
  Otherwise wake exactly as today.
- **Flush on transition** — when the session's next hook event reports it left
  the human-input state (operator answered → `PostToolUse`/`Stop`), deliver the
  buffered wake. Events also stay durable in `inbox.jsonl`, so a missed flush is
  never a lost event (it re-delivers on the next inbox event / session start).
- **Reclassify `idle_prompt`** so it is treated as wakeable, not human-input.
- **Persisted across restarts (operator-required).** The buffer is the new
  `pending_wakes` SQLite table (FIFO by id), not in-memory — a `flow ui serve`
  restart never loses a withheld wake (incl. an operator-approved reply). peek is
  non-destructive; a row is acked only after a confirmed inject, and an
  undeliverable wake is left queued for the next attach. A boot-time
  `resumeBufferedWakes` sweep re-attempts delivery.
- **Visibility** — surface "waiting on your input" more loudly (the floating-tray
  already has a "Waiting" badge) so a pending question is not missed. *(deferred —
  the gate + persistence are the load-bearing fix)*

**Status: DONE + verified.** Full suite green (2107 tests, 29 packages). New
tests: `flowdb.AwaitingHumanInput` predicate (idle_prompt/codex-stop stay
wakeable), `pending_wakes` CRUD/FIFO persistence, DB-backed `wakeQueue`
round-trip + flush guard, and a server-level regression guard
(`TestWakeTaskBuffersWhileAwaitingHumanInput`) proving a wake to a session with
an open AskUserQuestion is buffered, not injected.

Touch-points: `internal/server/terminal_wake.go`, `agent_hooks.go`,
`inbox_notify.go`, `task_monitor.go`, `steerer_session.go`,
`terminal_bridge.go`/`terminal_session.go`, `internal/flowdb` agent-runtime-state.

## Part B — External-channel send gate (backstop)

**Detection** — a conversation is external if it is Connect/externally-shared
(`is_ext_shared` / `is_org_shared`, captured from `conversations.info` and
cached) **OR** a participant/channel `team_id` is not one of the operator's
workspaces. This is literally "people outside the org are present in the channel
or group."

- Extend `SlackConversation` (`slack_title.go`) to capture `is_ext_shared`,
  `is_org_shared`, `is_shared`, and `context_team_id`/`team_id`.
- Add `FLOW_SLACK_TEAM_ID(S)` config (operator's workspace ids), auto-seedable
  from `auth.test`.

**Enforcement at the send chokepoint** — in `SendAsThread` / `handleSlackSend`,
an external send **without an operator-approval token** is not sent. It is parked
as a **pending outbound card** surfaced in the inbox, and the caller receives
"queued for your approval." Only the operator's inbox approval mints the token,
so this catches *every* path: manual `flow slack send`, agent sessions,
auto-permit, and the steerer.

**Inbox gate** — the card shows the target flagged **EXTERNAL** (and who is
outside), the drafted message (editable), **Approve & send** (performs the real
send with the token, marks sent) and **Discard**.

Non-blocking: the send call returns immediately ("queued"); the queued card is
the operator's to action. The CLI prints `QUEUED (not sent): …` so a human or
agent never assumes delivery.

**Status: backend DONE + verified; UI (B3) remaining.**
- B1 (detection): `SlackConversation` now carries the Connect/shared flags + team
  ids; `IsExternalToOrg(operatorTeams)` predicate; `OperatorTeamIDs()` config
  (`FLOW_SLACK_TEAM_IDS`/`_ID`); `monitor.LookupConversation`. TDD'd.
- B2 (gate + store): `pending_sends` table + CRUD (TDD'd); the gate in
  `handleSlackSend` (external → 202 + parked row, never posts); `classifySlackChannel`
  with a 10-min verdict cache (fail-open on lookup error); `GET /api/slack/pending`
  + `POST /api/slack/pending/decide` (send posts directly, bypassing the gate;
  discard drops); CLI 202 handling. Gate integration test proves external is
  parked, internal posts, and approval posts the parked message.
- **B3 (DONE): the inbox UI panel.** `PendingSendsPanel` pinned atop the
  Attention feed (`screens/Attention.tsx`) — lists parked sends with an EXTERNAL
  badge, channel + reason, editable text, Approve & send / Discard. Hooks
  `usePendingSlackSends` / `useSlackPendingDecide` (query.ts), `.psend-*` styles
  (warn accent, matches `.att-*`), live-refreshed via `publishUIChange("slack-pending")`
  on enqueue/decide. UI builds (tsc + vite) and is embedded; Go binary rebuilt.
  Full suite green (2122). Not yet visually run by me — recommend a quick
  browser check.

**Known interaction (note):** the attention *send-reply* flow already has the
operator approving the reply text. When that reply targets an external channel,
the agent's `flow slack send` is now re-gated into the pending queue — a second
confirm. Safe (nothing escapes) but redundant. A future refinement can let an
attention-approved reply carry a one-time bypass token; for v1 the extra confirm
is acceptable and matches "always gated by my decision".

Touch-points: `internal/monitor/slack_send.go`, `slack_writer.go`,
`slack_title.go`; `internal/server/slack_send.go`, `attention.go`;
`internal/flowdb/attention.go` (pending-outbound rows); Mission Control attention
UI.

## Part C — Human persona

- A single global **voice** authored in Mission Control (Attention → config →
  **Voice**), persisted as `persona.md` under the flow root (versioned by
  `flowbackup` automatically — a root-level .md isn't gitignored; each edit is
  checkpointed). Seeded with a sensible human default on `flow init` so replies
  don't sound like a bot before customization. HTML comments in the file are
  editing guidance and are stripped before injection.
- Injected via `operatorVoiceDirective()` into the steerer's draft prompt
  (`triage.go`) and the approved-send prompt (`send_reply.go`) so drafts/sends
  read like the operator. Empty file ⇒ no-op.
- Per-audience voices deferred (YAGNI for v1).

**Status: DONE + verified.** `flowdb`/`steering` voice loader + directive (TDD),
init seeding (TDD), `GET/PUT /api/persona` + the Voice editor in the UI (builds).

## Part D — "Sent using @<app>" attribution footer (added per follow-up)

Outbound `flow slack` messages append a footer so recipients know flow sent it.
- Applied at the send chokepoint (`SendAsThread`/`ScheduleAsThread`), so every
  outward path gets exactly one footer and the composing agent never writes it
  (the persona/skill "no footer in body" rule stays correct — flow owns
  attribution).
- **Suppressed on the operator↔bot command DM** (a flow system message, not an
  outward reply).
- Uses the **real installed app name**, never a guess: `FLOW_SLACK_APP_NAME`
  (persisted by the connector at app-creation) → the bot's `auth.test` handle
  (cached) → `flow` only as a last resort.
- Configurable: `FLOW_SLACK_SEND_FOOTER` overrides the text outright; set it
  empty to disable. Default on. (File uploads' initial comment intentionally not
  footered for v1.)

**Status: DONE + verified.** Footer + app-handle resolution (TDD), connector
persists `FLOW_SLACK_APP_NAME`. Full suite green (2129).

## Sequencing

A (wake fix) → B (external gate) → C (persona), by urgency.
```
