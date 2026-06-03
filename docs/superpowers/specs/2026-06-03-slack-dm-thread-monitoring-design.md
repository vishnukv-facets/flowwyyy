# Slack DM thread monitoring — design

Date: 2026-06-03
Scope: flow-manager (`flow` CLI · `internal/monitor` · embedded skill)
Status: implemented (2026-06-03). All items shipped with tests, including item 5
(DM backfill parity) via a user-token conversations.history path.

## Problem

A flow task can be bound to a Slack **thread** (tag `slack-thread:<channel>:<thread_ts>`).
The Socket Mode listener routes every new message in that thread into the task's
`inbox.jsonl`, which wakes the live Claude/Codex session.

When the operator asks the agent to **reply to someone via DM** instead of in the
public thread, that reply goes out through the claude.ai Slack MCP **as the
operator** (user token). The recipient's later replies land in a DM channel the
task is *not* watching, so they are silently dropped and the session never wakes.
The conversation effectively falls off flow's radar the moment it moves to a DM.

## Goal

When an agent working a task starts a DM in the course of that task, the task
should **also monitor that DM channel** — the recipient's replies stream into the
same `inbox.jsonl` and wake the session, exactly like thread replies do today.
A single task may monitor its origin thread **plus any number of DM/group-DM
channels** at once.

## Non-goals (YAGNI)

- **No auto-capture from raw events.** flow will not try to infer "the operator
  just sent a DM, attach it to some task" by fingerprinting outbound `message.im`
  events. That cannot reliably attribute a DM to a task and would vacuum in the
  operator's *personal* DMs. Attribution comes from the agent, which knows for
  certain it just sent the DM (see Registration).
- **No new outbound send path.** The agent keeps sending via the claude.ai Slack
  MCP as it does today. flow's `SlackWriter` is unchanged and stays unused.
- **No DB schema change.** Monitoring stays tag-based, consistent with
  `slack-thread:`.

## Prerequisite: Slack app configuration (operator, one-time — DONE)

For DM messages to arrive over the existing Socket Mode connection, the Slack app
must subscribe to user-scoped DM events. Under **Event Subscriptions → "Subscribe
to events on behalf of users"**, add:

- `message.im`   (scope `im:history`)   — direct messages
- `message.mpim` (scope `mpim:history`) — group direct messages

The operator has already added these (plus `message.channels`, `message.groups`,
`reaction_added`, `reaction_removed` on the user side) and granted the matching
user-token scopes. After editing subscriptions, the app must be **reinstalled** to
the workspace for them to take effect.

No flow connection change is required: Socket Mode delivers user-scoped events over
the same WebSocket (app-level token), and the bot token continues to drive Web API
calls. `FLOW_SLACK_USER_TOKEN` / `SLACK_USER_TOKEN` is used for any user-scoped Web
API reads (already supported by `SlackUserToken()`).

### Note on duplicate delivery

The operator also subscribed user-scoped `message.channels` / `message.groups`,
which overlap with the bot's existing channel subscriptions. Whether Slack delivers
an event visible to both the bot and the user once or twice over the socket is not
something we want to depend on. We therefore add append-time dedup (below) so flow
is correct regardless. (The operator may optionally remove the user-scoped
`message.channels` / `message.groups` rows, since the DM feature only needs
`message.im` / `message.mpim`; this is an optimization, not a requirement.)

## Design

### 1. Data model — the `slack-dm:` tag

New tag prefix, parallel to `slack-thread:`:

```
slack-dm:<channel-id>
```

- `<channel-id>` is a DM (`D…`) or group-DM (`G…`/mpim) channel ID, normalized via
  `flowdb.NormalizeTag` (lowercased) on both store and lookup — the same invariant
  the `slack-thread:` lookup already relies on (`db.go:1948`, `dispatcher.go:132`).
- A task may carry many `slack-dm:` tags. `task_tags` is many-to-many, so a task can
  hold `slack-thread:c…:ts` + `slack-dm:d_alice` + `slack-dm:d_bob` at once.
- Unlike `slack-thread:`, the tag carries **no `thread_ts`** — DMs are not threaded
  (each top-level DM message gets its own `thread_ts`), so the channel ID alone is
  the partition key.

Constant added in `dispatcher.go`:

```go
const SlackDMTagPrefix = "slack-dm:"
```

### 2. Registration — the agent tags its own task (automatic, via the skill)

When the agent DMs someone for a task, it records the DM channel on its task using
the **existing** tagging CLI — no new command:

```bash
flow update task <slug> --tag slack-dm:<channel-id>
```

The agent already knows both inputs: its own task slug (it is running as that task)
and the DM channel ID (returned by the Slack MCP `slack_send_message` /
`conversations.open` call). This is the single deterministic attribution point —
only DMs the agent actually initiated get monitored.

This is wired in two source-of-truth places:

- **`internal/app/skill/SKILL.md`** — the embedded skill that governs how
  Claude/Codex interact with flow. Add a short "monitoring a DM" instruction.
- **`slackTaskBrief` in `dispatcher.go`** — the per-task brief handed to
  Slack-origin sessions. Add a "How to DM" subsection mirroring the "How to reply"
  block (lines ~348-354), instructing: *if you DM a participant for this task,
  immediately run the tag command above so their replies stream into your inbox.*

### 3. Inbound routing — match DM messages by channel

`dispatchMessage` (`dispatcher.go:112`) today keys only on
`ThreadKey(channel, thread_ts)` and drops untracked threads. Extend it so DM/group-DM
messages are also matched by channel-only `slack-dm:` lookup:

```go
func (d *Dispatcher) dispatchMessage(ctx context.Context, ev InboundEvent) error {
    // 1. Thread match (unchanged): channel + thread_ts.
    if key := ThreadKey(ev.Channel, ev.ThreadTS); key != "" {
        if slug, found, err := d.findTaskByThreadKey(key); err != nil {
            return err
        } else if found {
            return AppendInboxEvent(slug, ev)
        }
    }
    // 2. DM match: for im/mpim, route by channel ID alone.
    if ev.ChannelType == "im" || ev.ChannelType == "mpim" {
        if slug, found, err := d.findTaskByDMChannel(ev.Channel); err != nil {
            return err
        } else if found {
            return AppendInboxEvent(slug, ev)
        }
    }
    return nil // untracked — drop, as before
}
```

New helper mirroring `findTaskByThreadKey`:

```go
func (d *Dispatcher) findTaskByDMChannel(channel string) (slug string, found bool, err error) {
    channel = strings.TrimSpace(channel)
    if channel == "" {
        return "", false, nil
    }
    tag := flowdb.NormalizeTag(SlackDMTagPrefix + channel)
    tasks, err := flowdb.ListTasks(d.DB, flowdb.TaskFilter{Tag: tag})
    // prefer non-done task; fall back to first — identical policy to findTaskByThreadKey
}
```

`ClassifyInboxEvent` already marks `Kind == "message"` as `actionable` regardless of
channel type (`inbox.go:60-64`), so the wake path needs no change — a routed DM
message wakes the session like any thread message.

### 4. Append-time dedup by `(channel, ts)`

`AppendInboxEvent` (`inbox.go:131`) currently appends unconditionally. Add a guard:
before writing, skip if an entry with the same Slack `(channel, ts)` already exists
in `inbox.jsonl`. This makes flow correct under:

- dual bot+user event delivery for the same channel message (the overlap above),
- live-listener vs. backfill races,
- Socket Mode reconnect replays.

Implementation: reuse the ts-indexing approach already proven in
`slack_backfill.go` (`inboxSlackTSIndex`). For typical inbox sizes (tens of
entries) a per-append scan is acceptable; if it ever matters, the dispatcher can
hold an in-memory `seen` set. The dedup key is `channel + ":" + ts`; reactions use
their event ts, which is likewise unique. Non-Slack events (no ts) are never deduped.

### 5. Backfill parity (optional, include if low-cost)

`SlackBackfill` (`slack_backfill.go`) reconciles `slack-thread:` tasks via
`conversations.replies`. Extend it to also reconcile `slack-dm:` channels via
`conversations.history` (DMs aren't threaded, so `replies` doesn't apply), using the
**user token** so the operator's DMs are readable. This is a safety net for messages
missed during a socket gap; the live path is the primary mechanism. If the
`conversations.history` plumbing is more than incremental, defer to a follow-up — the
live listener already satisfies the goal.

**Status: implemented.** `SlackConversationHistory` (a user-token
`conversations.history` client, `NewSlackDMHistoryClient`) is wired into
`SlackBackfill` via `SetDMHistoryClient`. `runOnce` reconciles each task's
`slack-dm:` channels alongside its thread. Cursors are now **per-channel**
(`inboxSlackTSIndexForChannel`) for both thread and DM reconcile, so a newer
message in one channel can't advance another channel's resume point past unseen
messages. Requires `FLOW_SLACK_USER_TOKEN` (or `SLACK_USER_TOKEN`) to be set —
the bot token can't read the operator's DMs; DM backfill no-ops without it.

## Error handling

- **Untracked DM** → dropped silently (existing behavior for untracked threads). No
  task means flow was never asked to watch that DM.
- **Tag command failure** (agent side) → surfaced to the agent as a non-zero
  `flow update task` exit; the agent reports it in-session. Monitoring simply won't
  start for that DM, matching today's failure mode for any tagging error.
- **Missing user token** → DM events still arrive over the socket (push), but
  backfill reads (pull) no-op, as the existing backfill client already guards on an
  empty token (`slack_backfill.go:28`).

## Testing

Pure-Go, no live Slack — consistent with the existing monitor tests (fakes via
package-level function vars; real SQLite in a temp dir).

- `dispatcher_test.go`: a `message` with `ChannelType=="im"` in a channel tagged
  `slack-dm:<chan>` routes to that task's inbox; an untracked DM is dropped; a task
  carrying **two** `slack-dm:` tags routes messages from **both** channels; a DM
  whose channel also has no thread match still routes via the DM tag.
- `inbox_test.go`: appending the same `(channel, ts)` twice yields one entry;
  distinct ts values both append; non-Slack/empty-ts events bypass dedup.
- `inbound_event_test.go`: already covers `im`/`mpim` parsing; add an assertion that
  `ChannelType` is preserved through to dispatch.
- Skill: extend `internal/app/skill_test.go` if it asserts on brief/skill content.

## Files touched

- `internal/monitor/dispatcher.go` — `SlackDMTagPrefix`, `findTaskByDMChannel`,
  `dispatchMessage` DM branch, brief "How to DM" section.
- `internal/monitor/inbox.go` — dedup in `AppendInboxEvent`.
- `internal/monitor/slack_backfill.go` — (optional) DM reconciliation.
- `internal/app/skill/SKILL.md` — DM-registration instruction (rebuild for
  `flow skill update` to pick it up — see CLAUDE.md "Skill embed path").
- Tests alongside the above.
- `README.md` — document `slack-dm:` and the user-scoped event-subscription
  requirement under the Slack section.

## Addendum (2026-06-03): socket auto-registration

Agent self-registration proved unreliable in practice — in-flight sessions have
**frozen briefs** that predate the feature, so the agent never gets the
instruction and DMs without tagging. Now that user-scoped `message.im` events
flow, flow auto-registers without agent cooperation:

- **Detection** (`hasAgentFooter` + operator authorship): when an `im`/`mpim`
  message arrives in an unregistered channel, is authored by an operator ID, and
  its text carries the agent footer (`Sent using @…`), it's an agent-sent DM.
  The footer is the deterministic "via the agent" signal — its absence means we
  treat the DM as personal and never auto-monitor it.
- **Attribution** (`attributeDMToTask`): resolve the DM's members via
  `conversations.members` (user token, `NewSlackDMMembersResolver`), drop
  operator IDs to get the recipient(s), and match against each active
  slack-reply task's thread participants (`inboxParticipantUserIDs`, scanned from
  inbox author IDs). Exactly one match → register `slack-dm:<channel>` and append
  the outbound as the backfill baseline. Zero or multiple → log and skip (manual
  fallback). Wired in `dispatchMessage`; resolver injected via
  `Dispatcher.SetDMMembersResolver` in the server.
- **Self-diagnosing logs**: every early-out logs to stderr (footer absent / no
  single thread match / registered) so a live run reveals whether the event is
  delivered and whether the footer survives in `event.text` — the two
  assumptions that can't be unit-tested against real Slack.

Agent self-registration (skill step 7) stays as a secondary path for cases the
heuristic can't attribute (e.g. DMing someone not yet in the thread).

## Addendum (2026-06-03): DM backfill must read thread replies

Live debugging of a real DM (`D03LH2RCZMG`) showed every DM message was a
**thread reply** (`thread_ts` ≠ `ts`). `reconcileDM` used only
`conversations.history`, which returns top-level messages and is **blind to
thread replies** — so a threaded DM reply missed during a socket gap could never
be recovered. Fix: `SlackConversationHistory` now also exposes `Replies`
(conversations.replies, user token); `reconcileDM` pulls history **plus**
replies for every thread root the DM is known to use
(`inboxThreadRootsForChannel`, scanned from inbox `thread_ts`). Recovered events
preserve the real `thread_ts`. Per-thread fetch errors are logged and skipped,
not fatal.

Note: this was diagnosed alongside two **non-code** factors that also caused
"lost messages": (a) thread backfill failing on transient network errors
(`conversations.replies: operation timed out` in `~/.flow/logs/ui-serve.log`),
and (b) a captured message not waking an idle/just-restarted session — a
separate wake-path concern, not a capture or backfill bug.

## Addendum (2026-06-03, final): thread-scoped DMs via the tool-use hook

Live use exposed two fatal flaws in the channel-scoped + footer-auto-reg model,
so it was **replaced** (channel-scoped code removed entirely):

1. **Channel-scoping was the wrong unit.** A `slack-dm:<channel>` tag captures
   *every* message in a person's DM — all topics — so unrelated workstreams
   leaked into a task (the agent itself flagged this). A DM **conversation** is a
   thread, not a channel.
2. **"Sent via Claude" isn't in the event.** The footer is Slack UI app
   attribution rendered at display time; the Events API payload has no
   `app_id`/`bot_id`/footer (verified live: `agent-footer=false`). So a socket
   listener cannot tell an agent-sent DM from a hand-typed one — footer
   auto-detection is impossible.

**Final design — register at the agent's `PostToolUse` hook, thread-scoped:**
- `slackDMSendFromHook` (server/agent_hooks.go) detects a Slack *send* to a **DM
  channel** (provider-agnostic: matches `send`/`post_message` in the tool name +
  a `D…` channel, excluding drafts/reads — covers Claude's
  `slack_send_message` and Codex's send-message), reading `channel` + `thread_ts`
  from `tool_input` (or the posted ts from `tool_response` for a fresh DM).
- `maybeRegisterDMThread` tags the hook's task `slack-thread:<dm-channel>:<root>`
  — reusing the thread model. Deterministic, automatic, both providers, no
  footer, no footgun (only real agent sends fire it), no dependence on fresh
  briefs or agent self-tagging.
- **Routing**: unchanged — the existing thread branch routes
  `slack-thread:<dm-channel>:<root>` by `(channel, thread_ts)`. Only the DM
  thread the agent started routes in; unrelated DM topics are excluded.
- **Backfill**: `runOnce` reconciles *all* `slack-thread` tags per task (via
  `threadRefsFromTags`), and `reconcile` uses the **user-token** replies client
  (`NewSlackUserRepliesClient`) for DM-channel threads — the bot can't read the
  operator's DMs.
- **Removed**: `slack-dm:` tag/routing, `findTaskByDMChannel`, footer
  auto-registration, `attributeDMToTask`, `DMMembersResolver`, `reconcileDM`,
  the `conversations.history` DM path. DMs are now *only* thread-scoped.

## Out of scope

- Auto-detecting DMs without agent registration.
- Sending DMs through flow's own bot (`SlackWriter`) — would change the sender
  identity from "you" to the bot; rejected to preserve "posts go as YOU".
- Unsubscribing / un-monitoring a DM mid-task (a `--untag` already exists if needed;
  no dedicated UX planned).
