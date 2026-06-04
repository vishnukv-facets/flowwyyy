# GitHub autonomous-agent acknowledgement + tag-coverage — design

**Date:** 2026-06-04
**Status:** Implemented (commits 50fc860 ack, fc7adfa discovery). Requires a
`flow ui serve` restart to take effect.

## Problem

Two gaps in flow's GitHub monitor pipeline:

1. **No acknowledgement.** When the monitor opens a GitHub-origin task (assigned
   issue, review-requested PR, …) or a tracked PR/issue receives a new review
   comment, the human counterpart on GitHub gets no signal that an autonomous
   agent has picked it up. Reviewers like `anujhydrabadi` leave a comment and
   hear nothing until the work lands.

2. **@-mentions on untracked items are invisible.** Discovery is two search
   queries — `is:open assignee:<login>` and `is:open is:pr review-requested:<login>`
   (`internal/monitor/github_client.go` `queriesForLogin`). Being **@-mentioned**
   in a comment on a PR/issue you don't own does not match either, and comments
   are only polled on already-tracked items. So a teammate writing
   "@you can you look at this?" on an untracked PR never opens a session.

## Goals

- A reviewer/assigner sees, promptly, that an **autonomous agent** is handling
  the item — posted **by the agent itself** (skill-driven), reactively, before
  the agent starts the substantive work and again when woken by new events.
- Being **@-mentioned** on any open issue/PR opens a session (same as
  assignment/review-request today).
- Being otherwise **involved** (author/assignee/commenter/mentioned →
  `involves:`) surfaces as a **notification** without flooding with sessions.
- No echo loop: an agent's own ack never re-wakes its session.
- No backfill flood when the broader discovery is first enabled.

## Non-goals

- A separate bot GitHub identity. Acks post as the operator's `gh` identity;
  the text states it's an autonomous agent.
- Backend/monitor-authored acks. The operator chose **agent-posted** acks.
- A new global notification store. `involves:` notifications reuse the existing
  task + inbox model (task created, session not auto-opened).

---

## Part A — Agent-posted acknowledgement (skill-driven)

Primarily a `internal/app/skill/SKILL.md` change (§10c, GitHub tasks), plus one
small deterministic backstop in the monitor.

### Behavior (skill rules)

1. **Bootstrap ack.** When the agent boots a GitHub-origin task (`github` tag +
   `gh-pr:`/`gh-issue:`), **before analysis**, it posts one acknowledgement on
   the item:
   - PR: `gh pr comment <n> --repo <owner>/<repo> --body "<ack>"`
   - Issue: `gh issue comment <n> --repo <owner>/<repo> --body "<ack>"`
   - Ack body (operator's voice, states autonomy), ending with the marker line:
     ```
     🤖 An autonomous flow agent has picked this up and is looking into it — I'll follow up here.

     <!-- flow-agent-ack -->
     ```
2. **Reactive ack on wake.** When woken by a new actionable event
   (`pr_review_comment`, `pr_review_changes_requested`, `pr_comment`,
   `issue_comment`), the agent posts a short reactive ack on that thread
   ("On it — reviewing this now.") with the same marker, before doing the work.
3. **Ack once per item, not per event.** Before posting the bootstrap ack, the
   agent checks the item's existing comments for the `flow-agent-ack` marker;
   if present, it skips re-acking and proceeds. This keeps a busy PR from
   accumulating an ack on every bot comment.
4. **Never re-action your own ack.** Reinforces the existing
   "don't re-action a comment you posted" rule, now anchored on the marker.

### Echo-loop backstop (monitor code)

`pollTrackedPRComments` / `pollTrackedIssueComments` deliberately deliver
self-authored top-level comments (they are the operator's instruction channel).
An agent ack is self-authored, so without a guard it would be re-delivered and
re-wake the session. Fix: **drop a comment when it is authored by a self login
AND its body contains `<!-- flow-agent-ack -->`.** Real instruction comments
(no marker) still flow through unchanged.

- Filter lives where issue/review comment records become events
  (`githubIssueCommentRecord.toGitHubEvent` / `githubReviewCommentRecord` and
  `githubReviewRecord`), or at the poll loop. It needs the self-login set, which
  the poller already holds (`GitHubPoller.SelfLogins`).
- Marker constant defined once in the monitor package and referenced by the
  skill text so the two never drift.

---

## Part B — Discovery: mentions (session) + involves (notify-only)

### Queries

Extend `queriesForLogin` with two tiers:

| Query | Tier | Action |
|---|---|---|
| `is:open assignee:<login>` | direct | create task **+ auto-open session** (today) |
| `is:open is:pr review-requested:<login>` | direct | create task **+ auto-open session** (today) |
| `is:open mentions:<login>` | direct | create task **+ auto-open session** (NEW) |
| `is:open involves:<login>` | involved | create task, **no auto-open** (NEW) |

### Event kinds and routing

- New kind `GitHubEventMentioned` (treated as an issue-or-PR "you should act"
  item, like the assigned kinds) — actionable, auto-opens. It flows through
  `dispatchGitHubItem` exactly like `*Assigned`.
- New kind `GitHubEventInvolved` — routed through `dispatchGitHubItem` too, but
  the dispatcher **skips `OpenInUI`** for it (task is created and appears in
  Tasks/Inbox; no session spawns). The agent/operator opens it manually if it
  matters.
- `toGitHubEvent(login, query)` selects the kind from the matched query string
  (it already inspects `review-requested:`): `mentions:` → `*Mentioned`,
  `involves:` → `*Involved`.

### Per-item dedup across tiers

An item can match several queries (e.g. both `assignee:` and `involves:`).
`Poll` already dedups events by `EventKeyValue` (which includes kind), so the
same item could otherwise produce two events with different kinds. Add a
collapse step: after gathering search-discovered events, **dedup by `LinkTag`,
keeping the highest-priority kind** (assigned / review-requested / mentioned >
involved). Only the winning event dispatches. This guarantees an assigned item
opens a session even if it also matches `involves:`.

### Backfill watermark

- Persist a watermark timestamp `gh_discovery_watermark` in `schema_meta`
  (needs a value-bearing get/set; existing helpers only store `'1'`).
- On the first poll where it is unset, set it to "now" and use it from then on.
- Apply `updated:>=<watermark>` **only to the two NEW queries** (`mentions:`,
  `involves:`). Leave `assignee:` / `review-requested:` unfiltered — those are
  low-volume direct ownership and backfilling them on first run is desirable.
- Effect: enabling the broader discovery never retroactively surfaces the
  historical involved-items backlog; only activity after enabling appears.
  Combined with `HasGitHubEvent` per-event dedup, the involved tier stays quiet.

---

## Data flow (end to end)

```
gh search (assignee|review-requested|mentions|involves, NEW two filtered by updated:>=watermark)
  → toGitHubEvent picks kind by matched query
  → Poll collapses to one event per item (highest-priority kind)
  → GitHubDispatcher.Dispatch
       *Assigned / *ReviewRequested / *Mentioned → dispatchGitHubItem → create task + OpenInUI
       *Involved                                 → dispatchGitHubItem → create task, skip OpenInUI
  → agent boots task → SKILL §10c → posts bootstrap ack (marker) on the item
  → reviewer comments → pollTrackedComments delivers it → agent woken
       → agent posts reactive ack (marker) → does the work
  → that ack is self-authored + marked → monitor drops it → no re-wake
```

## Error handling

- Ack posting failures (`gh` error, no write permission) are non-fatal for the
  agent: it logs/notes and proceeds with the work rather than blocking.
- Watermark get/set failures degrade safely with **skip-on-error**: if the
  watermark can't be read on a given cycle, the two NEW queries (`mentions:`,
  `involves:`) are skipped for that cycle and retried next cycle. Running them
  unfiltered on a read error is explicitly rejected — it risks the exact
  backfill flood the watermark exists to prevent. The two existing direct
  queries (`assignee:`, `review-requested:`) are unaffected and always run.
- Unknown/zero owner/repo/number from a search record is dropped as today.

## Testing

- `github_client_test.go`: `queriesForLogin` includes the four queries; new
  queries carry the `updated:>=` filter when a watermark is set and omit it when
  unset-then-set on first call.
- `toGitHubEvent`: `mentions:` query → `*Mentioned`; `involves:` query →
  `*Involved`; existing assigned/review-requested unchanged.
- Poll collapse: an item matching both `assignee:` and `involves:` dispatches
  once as the assigned kind.
- Dispatcher: `*Mentioned` opens session; `*Involved` creates task but does NOT
  call `OpenInUI` (assert via a fake `TaskOpener` recording calls).
- Echo backstop: a self-authored comment containing `flow-agent-ack` produces no
  event; a self-authored comment WITHOUT the marker still produces an event; a
  non-self comment with the marker still produces an event.
- Watermark store: get-after-set round-trips; first-unset read returns empty.

## Out of scope / follow-ups

- `👀` reactions (the operator chose agent-comment acks; reactions can be added
  later if comment acks feel heavy).
- Collapsing many `involves:` notifications into a single digest task.
