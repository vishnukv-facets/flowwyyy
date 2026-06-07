# Attention FYI task-impact design

**Date:** 2026-06-07
**Status:** Draft for operator review
**Repo:** flow-manager (`flow` Go CLI + Mission Control UI)

---

## 1. Summary

Attention and Mission Control currently treat some Slack FYI messages as
operator attention, but the cards do not explain why the operator should care.
The visible symptom is an availability notice such as "Rohit is out" showing in
Needs action with a generic digest label, even when the important question is
whether Rohit's absence affects active work.

This phase makes FYI routing task-aware and makes briefing cards
self-explanatory. A pure FYI should land in FYI and say "no action". An FYI that
affects active work should name the affected task and explain the impact.

The implementation stays metadata-first: use Flow's task database, task tags,
`waiting_on`, `assignee`, task names, and bounded brief/update snippets. Do not
add live Slack/GitHub lookup to the first version.

---

## 2. Goals

- Detect when a Slack sender or named participant is associated with an active
  Flow task that may be affected by an availability/FYI message.
- Promote task-impacting FYI messages to the right action bucket with
  `matched_task` and task links.
- Keep unrelated FYI messages out of Needs action.
- Make Mission Control FYI cards understandable at a glance: what happened, why
  it matters, and what action is expected.
- Preserve explainability through Attention, trace, task, and source links.
- Keep the first implementation deterministic and testable without relying on
  live connector reads.

---

## 3. Non-goals

- Replacing the Attention Router cascade or WorkEvent read model.
- Adding a new canonical table for people or task ownership.
- Performing live Slack or GitHub API inference to discover reviewers,
  assignees, or channel membership.
- Auto-sending replies or notifying affected people.
- Reclassifying historical rows in SQLite with a migration. Existing rows can
  be rendered more clearly at read time.

---

## 4. Current Behavior

Attention Stage 3 already receives a compact task/project index plus paths to
each task's `brief.md` and `updates/` directory. The model can theoretically
read task files and infer whether an event belongs to a task.

In practice, the compact index does not expose obvious task-impact metadata:

- `waiting_on`
- `assignee`
- source tags such as `gh-pr:` or `slack-thread:`
- recent brief/update snippets that mention a reviewer or blocker

Mission Control's `internal/briefing` then places all new Attention feed rows in
`NeedsAction`, including rows whose `suggested_action` is `digest_only`. That
makes a pure FYI card compete with real operator decisions and leaves the FYI
column as a less useful activity dump.

---

## 5. Proposed Design

Add a deterministic task-impact hint layer for Attention and Briefing.

The layer should answer one question:

> Does this message affect active work because the sender or named participant
> is tied to a Flow task?

The output is a small list of task hints:

```go
type TaskImpactHint struct {
    TaskSlug    string
    TaskName    string
    ProjectSlug string
    Status      string
    Priority    string
    Reason      string // e.g. "waiting_on mentions Rohit review"
    Evidence    string // short snippet from task metadata, brief, or update
}
```

The hints are not a source of truth and do not mutate tasks. They are context
for classification and display.

---

## 6. Hint Sources

The first version should use these sources, in this order:

1. `tasks.waiting_on`
   - Strong signal.
   - Example: `waiting_on = "Rohit review"` and Slack text says Rohit is on
     leave.

2. `tasks.assignee`
   - Strong signal when the assignee name matches the sender or named person.

3. Task source tags
   - Useful when the message names a PR/issue/thread that a task is tracking.
   - Existing tags include `gh-pr:owner/repo#N`, `gh-issue:...`, and
     `slack-thread:channel:ts`.

4. Task name and project name
   - Medium signal.
   - Useful for named reviewer/requester tasks such as "Anshul review on PR
     #159".

5. Bounded brief/update snippets
   - Medium signal.
   - Read only small snippets from active task brief/current-state and recent
     update files. Avoid indexing entire histories on the hot path.

Matching should be conservative:

- Normalize case and punctuation.
- Use full name tokens and known display names when available.
- Ignore one-letter and overly common words.
- Require at least one strong signal or two medium signals before promoting a
  pure FYI.

---

## 7. Attention Classification Rules

Stage 3 should receive the task-impact hints alongside the existing task index.
The prompt should say:

- Availability/FYI events are not automatically actionable.
- If task-impact hints show the sender/person is blocking or reviewing active
  work, set `matched_task` to the strongest affected task.
- Use `forward` when the task owner/session should know about the availability
  update.
- Use `digest_only` when there is no affected task and no reply needed.
- The reason must explain the task impact in operator language.

Examples:

| Message | Hints | Expected result |
|---|---|---|
| "Rohit is on leave tomorrow" | no matching active task | `digest_only`, FYI |
| "Rohit is on leave tomorrow" | task waiting on "Rohit review" | `forward`, `matched_task=<task>` |
| "Anshul can review after Monday" | task waiting on Anshul review | `forward`, `matched_task=<task>` |
| "General office closure Friday" | no task/person match | `digest_only`, FYI |

---

## 8. Briefing Classification Rules

`internal/briefing.attentionItems` should stop placing every new Attention item
into Needs action.

Rules:

- `reply`, `afk_reply`, `make_task`, and high-confidence manual approval items
  stay in `NeedsAction`.
- `forward` with `matched_task` becomes `NeedsAction` or `Waiting` depending on
  the matched task state:
  - If the matched task has `waiting_on`, show in `Waiting`.
  - Otherwise show in `NeedsAction` as task-impacting context.
- `digest_only` without `matched_task` goes to `FYI`.
- `digest_only` with `matched_task` should be treated as task-impacting context
  and displayed with the task link.

Each briefing item should carry an action phrase:

- Pure FYI: `No action`.
- Task-impacting FYI: `Review affected task`.
- Waiting impact: `Update waiting task`.
- Reply/draft: `Review reply`.

---

## 9. Mission Control Card Copy

Every briefing card should answer three things:

1. What happened?
2. Why does Flow think it matters?
3. What should the operator do?

For pure FYI:

- Title: `Rohit is out for two days`
- Detail: `No active task is waiting on Rohit. No action.`
- Links: `attention`, `trace`, `source` when available

For task-impacting FYI:

- Title: `Rohit is out for two days`
- Detail: `May affect <task name>: waiting on Rohit review.`
- Action: `Review affected task`
- Links: `task`, `attention`, `trace`, `source`

The UI should avoid relying on clipped titles for meaning. The visible detail
line must carry the impact sentence even when the title truncates.

---

## 10. Architecture

Add a pure-Go helper in `internal/steering` or a small shared package that can
be used by both Attention classification and briefing rendering.

Recommended shape:

- `internal/steering/task_impact.go`
  - Builds `TaskImpactHint` values from active tasks and the current event or
    context pack.
  - Used by the cascade before Stage 3.

- `internal/briefing`
  - Reuses stored `matched_task`, `suggested_action`, and reason/action text to
    bucket Attention rows correctly.
  - Does not rerun expensive matching during overview render unless needed for
    legacy rows.

- `internal/server/ui/src/screens/Overview.tsx`
  - Keeps the existing five-column briefing layout.
  - Improves row copy and labels without adding nested card structures.

No schema migration is required for the first version because
`attention_feed.matched_task`, `suggested_action`, `reason`, and existing links
are sufficient.

---

## 11. Data Flow

1. Slack/GitHub event enters the existing monitor/steering pipeline.
2. Cascade builds deterministic context pack.
3. Cascade builds task-impact hints from active Flow tasks.
4. Stage 3 receives:
   - context pack
   - task index
   - task-impact hints
5. Stage 3 returns verdict.
6. `writeFeed` stores `matched_task`, `suggested_action`, summary, reason, and
   source context.
7. `internal/briefing` buckets the Attention row by action and matched-task
   impact.
8. Mission Control renders the card with an explicit impact/action sentence.

---

## 12. Error Handling

- If hint building fails, continue without hints and record the failure in trace
  or logs. Do not block Attention ingestion.
- If brief/update snippets cannot be read, skip that evidence and use DB
  metadata.
- If multiple tasks match, choose the strongest hint for `matched_task` and
  mention only one task in the card. Future versions can show multiple affected
  tasks.
- If the match is weak, keep the item in FYI and explain `No active task match`
  rather than promoting it.

---

## 13. Testing Plan

Backend tests:

- Task-impact hint builder:
  - `waiting_on = "Rohit review"` matches an availability message from/about
    Rohit.
  - unrelated availability message returns no hints.
  - weak/common-token matches do not promote.

- Stage 3 prompt:
  - includes task-impact hints.
  - instructs `digest_only` for no affected task.
  - instructs `forward`/`matched_task` for affected active tasks.

- Briefing:
  - `digest_only` with no `matched_task` lands in FYI.
  - `forward` or task-impacting digest with `matched_task` lands in the
    appropriate action/waiting bucket.
  - card detail includes the impact/action phrase.

Frontend tests or smoke:

- Overview FYI card shows an understandable detail line.
- Needs action no longer contains pure FYI digest items.
- Task-impacting availability item links to the affected task.

Verification commands:

- `go test ./internal/steering ./internal/briefing ./internal/server`
- `go test ./...`
- `make build`
- `make ui` if Overview React code changes
- Browser/API smoke against `/api/overview`

---

## 14. Open Decisions

Resolved for v1:

- Association scope is metadata-first.
- No live-source lookup.
- No schema migration.

Deferred:

- Multiple affected tasks per Attention item.
- Durable person directory or alias table.
- Live GitHub reviewer lookup.
- Slack profile/team membership based matching.
