### 4.11 Scope-creep detection (passive — surface via AskUserQuestion)

This is a **passive** workflow like §5.10 — you watch the session as it
unfolds and intervene only when the evidence is strong. When you
intervene, the surfacing mechanism is `AskUserQuestion` (never a
prose "want me to...?" question). Its purpose is to keep a task's
transcript and update log focused, instead of letting unrelated work
pile up under whichever task happens to own the current terminal tab.

**When to consider firing:**

Fire when the *work itself* (not a single question) has clearly moved
off the bootstrapped task. Concretely, any of these is sufficient
evidence:

- You've made Edit/Write calls to files in a directory tree that isn't
  under the bound task's `work_dir` and isn't covered by its brief.
- You've spent two or more turns debugging a product, service, or
  repo that the brief doesn't mention.
- The user has introduced a new, named line of investigation ("while
  we're here, can you also look at <unrelated thing>") and begun
  giving it more than a single turn's attention.

**When NOT to fire (false positives to avoid):**

- A one-off tangential question answered in a single turn ("btw what
  does X mean?") — not drift, just curiosity.
- Reading/Read-tool usage outside the work_dir — reading is research,
  not work-migration. The trigger is write-side evidence.
- Natural debugging that requires touching nearby infrastructure the
  brief reasonably implies (e.g. a test helper in a sibling dir).
- The very first turn after session start — you don't yet know what
  "normal" looks like for this task.

**Recipe:**

1. When you notice drift per the signals above, pause your current
   work and use `AskUserQuestion`:

   ```
   AskUserQuestion({
     questions: [{
       question: "This looks unrelated to `<current-slug>` (<one-line drift description>). Want me to create a new flow task for it?",
       header: "New task?",
       options: [
         { label: "Yes, new task",  description: "Pause this work and run the §5.2 intake interview for <derived-name>" },
         { label: "No, stay here",  description: "Keep the work under <current-slug> — I understand this is still in scope" },
         { label: "Later",          description: "Note it in an update on <current-slug> and carry on for now" }
       ],
       multiSelect: false
     }]
   }))
   ```

2. **On "Yes, new task":** enter the §5.2 task-intake interview.
   Derive a task name from what you just observed (e.g. "Fix
   rate-limiter bug I stumbled on while reviewing PRs").
   Use the bound task's project only if the new work
   genuinely belongs there; otherwise leave it floating or attach to
   a different project per the user's answer during intake. After
   the new task is saved, use `AskUserQuestion` (header:
   "Open it now?", options: "Yes, open it" / "No, keep in backlog")
   to offer `flow do <new-slug>` so the follow-on work gets its own
   transcript.

3. **On "No, stay here":** accept the user's judgement and continue.
   Consider this a signal to update your mental model of what the
   bootstrapped task includes — don't re-ask on the same thread of
   work in the same session.

4. **On "Later":** use `AskUserQuestion` (header: "Drop a note?",
   options: "Yes, save a drift note" / "No, just continue") to offer
   writing a short progress note on the current task capturing the
   drift observation ("noticed X while doing Y; may need its own
   task"), then continue with the original work.

**Why this lives in the skill, not the hook:** the hook's only
guaranteed side-effect is injecting text at session start. Detection
requires inspecting what you've done this session (edits, debugging
topics) — that state only exists inside the running conversation. The
hook's job is to make sure the skill is loaded; the skill is what
runs the check.

**Note:** "the bootstrapped task" includes playbook-run tasks. The
triggers and recipe are identical for playbook-run sessions —
edits/debugging that drift outside the playbook's scope warrant the
same prompt.

### 4.14 Substantive-unrelated-work check (passive, ongoing)

This is a **passive workflow** that runs alongside every other workflow.
It fires when substantive work emerges that doesn't belong to the
current task binding.

**Triggers (any one is enough):**

- In a **dispatch session** (`flow show task` reports unbound):
  - You've been in active design / brainstorming / debugging
    discussion for ≥ 2 turns about a concrete topic, OR
  - You've made any Edit/Write tool calls, OR
  - You've invoked a process skill (`superpowers:brainstorming`,
    `superpowers:writing-plans`, `superpowers:executing-plans`,
    `superpowers:systematic-debugging`,
    `superpowers:test-driven-development`) — a process-skill invocation
    is itself a substantive-work signal.
- In a **bound session** (`flow show task` resolves a task): same triggers as §4.11
  (work moved off the bootstrapped task's scope).

**NOT a trigger:**

- One-off question answered in a single turn.
- Reading files / running queries to inform an answer.
- The very first message after session start (you don't yet know if
  this is one-off or substantive).

**Recipe:**

1. Pause current work.
2. Run `flow list tasks --status in-progress` and
   `flow list tasks --status backlog --priority high` to see candidates.
3. Use AskUserQuestion to offer three options:
   - **Create a new flow task** for this work — run §4.2 intake.
     §4.2's "Open now?" tail will offer **Continue here (bind this
     session)** alongside new-tab and backlog (since this is a
     dispatch session). Continue-here is usually the right pick
     from this path: substantive work has already started in this
     conversation, and binding preserves the transcript.
   - **Switch to an existing task** — list candidates as options. On
     selection, by default spawn `flow do <slug>` (new tab). Note:
     if THIS session is already bound to another task, do NOT offer
     `flow do --here <existing-slug>` as a sub-option — the binary
     would refuse (session_id uniqueness invariant), and even
     `--force` doesn't override it.
   - **Proceed ad-hoc** (user accepts no resumability, no context
     accumulation).

**Process-skill ordering:** when a process skill triggers this check,
load the skill first (so the user sees the right tool engage), then
**before** taking the skill's first concrete action, run the check.
If the user picks "create new task" or "switch to existing task," the
process skill resumes inside the new session, not this one.

**Important: this is an ongoing check, not one-shot.** Re-evaluate the
triggers each turn — especially when transitioning into design /
implementation / debugging work. The SessionStart hook gets you the
first check; you are responsible for every subsequent check.
Re-evaluate on every turn.

### 4.17 Delegate to a child task (orchestration)

**Triggers:** "spawn a child to do X", "have another agent handle X
while I keep going", "split this off into a parallel task", "delegate
this part", "kick off Y in the background and tell me when it's done",
"send a message to <task>", "wait for <task> to finish before I
continue", or a bare `flow spawn|tell|wait` invocation.

flow supports a small orchestration vocabulary so a parent agent can
delegate self-contained work to a child agent in another tab,
coordinate with a sibling task you're not actively driving, or block
until something finishes. There is **no in-session pane sharing** the
way herdr has — each agent gets its own tab, talks to siblings via
durable filesystem channels, and signals state through the same
`/ws/events` stream the UI consumes.

**The three primitives:**

| Command | Purpose | Synchronous? |
|---|---|---|
| `flow spawn <name> --parent <slug> --prompt "..."` | Create a subtask under `<slug>` in the organizational hierarchy (non-blocking) and open it immediately. Add `--depends-on <slug>` to also make it a blocking dependency. The prompt becomes the child's brief What section. | No — fires the child off in its own tab |
| `flow tell <slug> "..."` | Append a message to the receiving task's inbox.md and inbox.jsonl; a live Flow terminal wakes through the inbox monitor. | No — receiver handles it in its own session |
| `flow wait <slug> --until <state>` | Block the caller's terminal until the named task reaches the requested state. | Yes — blocks via WS subscription |

**When to use which:**

- **`spawn`** when the work is genuinely independent — a self-contained
  investigation, a build, a long-running script, a code review. The
  child gets its own brief, transcript, updates, and inbox.
- **`tell`** when a sibling task already exists and you want to nudge
  it with a follow-up instruction without spawning a new conversation.
  The receiver doesn't get the message immediately — it lands on the
  next session start (often after the receiver's current turn ends).
- **`wait`** when you need to gate parent progress on a child's
  completion. Prefer this over polling `flow show task` in a loop —
  `wait` subscribes to the live event stream and exits the moment the
  state changes.

**Recipe — typical orchestration pattern:**

```
# (1) Delegate a self-contained subtask
flow spawn "review-coverage" --parent oauth-budget \
    --prompt "Audit test coverage in src/api/ and report gaps as an update on this task before closing"

# (2) Continue parent work; later, block until child is done
flow wait review-coverage --until done --timeout 30m

# (3) Read what the child accomplished
flow transcript review-coverage --compact
# (the parent agent can also inspect ~/.flow/tasks/review-coverage/updates/)

# (4) Mid-flight nudge — change scope before the child is done
flow tell review-coverage "also check the legacy auth shim in src/legacy/"
```

**Triggers and proposals — what the parent agent should offer:**

When the user describes a workflow that fits the spawn/tell/wait
shape, surface the orchestration option via AskUserQuestion. Examples:

- *"Can you also review the test coverage while you're at it?"* —
  ambiguous between "do it inline" and "delegate to a child."
  Use AskUserQuestion (header: "How to handle?", options:
  "Do it here" / "Spawn a child task").
- *"Run the migration script and tell me when it's done."* — long
  blocking work the parent doesn't need to babysit. Propose
  `flow spawn migration-run --prompt "..."` + `flow wait migration-run
  --until done`.
- *"Pass this note to the budget-task agent."* — explicit sibling
  message. Run `flow tell budget-task "<note>"` directly; this is one
  of the rare cases where the user has named the action AND the
  target, so no further AskUserQuestion is needed.

**Anti-patterns (orchestration-specific):**

- **Do not spawn for tiny work.** If the subtask is one tool call or
  one short answer, do it inline. Spawning creates a permanent task
  row, a brief, and an entire transcript — overhead the user pays
  forever in their list view. Spawn for work that has its own
  meaningful "done" definition.
- **Do not poll `flow show task` in a loop instead of using `flow
  wait`.** Every poll is a DB hit and a token in your context;
  `flow wait` is event-driven and costs nothing while idle.
- **Do not `tell` an agent you expect to act immediately.** Inbox
  delivery is async — the receiver sees the message at its next
  SessionStart, which may not be for minutes. If you need a real-time
  hand-off, use `flow spawn` for a fresh agent or wait for a turn
  boundary on the sibling.
- **Do not bypass the user's confirmation when proposing orchestration.**
  If the work could plausibly be done inline OR delegated, ask via
  AskUserQuestion. The user often prefers inline for quick tasks.
- **Do not edit `parent_slug` or dependency rows by hand.** Hierarchy
  (organizational, non-blocking) is set by `flow spawn --parent` at
  creation and can be changed with `flow update task <child> --subtask-of
  <parent>` or cleared with `--unparent`. Blocking dependencies are set
  with `--depends-on <slug>` (repeatable), removed with `--remove-dep
  <slug>`, or cleared with `--clear-deps`. (The deprecated `--parent` /
  `--remove-parent` / `--clear-parent` flags in `flow update task` are
  aliases for the blocking-dependency flags, not for hierarchy — don't
  use them for re-parenting; use `--subtask-of` instead.)

**Reading the dependency graph:**

`flow show task` renders four relationship fields:
- `subtask of:` — organizational hierarchy parent (set via `--parent` in
  spawn or `--subtask-of` in update; non-blocking)
- `subtasks:` — organizational children (the inverse)
- `depends on:` — tasks THIS task is BLOCKED by; must complete before
  this one can be considered startable
- `blocks:` — tasks that are waiting on THIS task (the reverse of depends-on)

When the current bound task has subtasks (visible in `flow show task`
output under a `subtasks:` section, or in the UI as a tree), and the
user asks something like "what's status on the review-coverage task?",
prefer one of:

- `flow show task review-coverage` for the brief/updates/status
- `flow transcript review-coverage --compact` for the recent dialog
- `flow list tasks --project <project-slug>` to see sibling tasks in the same project

**Inbox housekeeping:**

When THIS task has unseen inbox entries, the SessionStart hook
prepends an `INBOX UPDATED: <slug> has new message(s) ...` notice with
the path to inbox.md. Read it BEFORE proceeding — those messages are
typically the parent's adjustments to scope or instructions you
haven't acted on yet. After reading, you don't need to do anything
special; the column `tasks.inbox_seen_at` is already bumped to the
inbox mtime by the hook, so the same messages won't re-fire on the
next session start.

## 10e. Owners

Owners are durable, repo-scoped controllers for ongoing outcomes. An owner is
not one long agent session: it is an `owners/<slug>/charter.md`, an
`owners/<slug>/updates/` journal, a row in `owners`, and short ticks that wake,
review state, dispatch work, self-pace, and exit.

Core commands:

- `flow add owner "<name>" --work-dir <path> [--project <slug>] [--every 24h] [--agent claude|codex]`
- `flow owner list` or `flow list owners`
- `flow owner show <slug>` or `flow show owner <slug>`
- `flow owner start|pause|retire <slug>`
- `flow owner tick <slug>` for a guided interactive tick; `--auto` for a headless tick now
- `flow owner next <slug> --in <duration>` or `--at <RFC3339>`
- `flow owner tick-due` is the scheduler entry point; do not run it manually unless you are checking the scheduler.

Owner rules:

- Owners orchestrate; they do not perform substantive work inline. A tick is
  sessionless and gets no task close-out sweep. Real work must be dispatched as
  tasks or playbook runs that can self-close.
- One-time work should be created with `flow add task "<what>" --agent <owner-agent> --tag owner:<slug>` and then run with `flow do --auto <task>` when safe.
- Human decisions should be parked as question tasks tagged both
  `question` and `owner:<slug>`. Do not run `question` tasks with `--auto`.
- Every tick should read the charter, read recent owner journal notes, run
  `flow owner show <slug>` to avoid duplicating in-flight work, dispatch only
  what is needed, write a concise journal note under `owners/<slug>/updates/`,
  set its next wake with `flow owner next`, and exit.
- The first tick should usually be interactive so the operator can tune the
  charter. Later ticks can run headlessly through `flow owner tick --auto` or
  the host scheduler calling `flow owner tick-due`.

## 11. When in doubt

Ask. The worst outcome is writing a bad brief or silently
mis-attributing a progress note. The second-worst outcome is running
`flow do` on the wrong task. Both are avoided by one clarifying
question. The user's time budget for a clarifying question is vastly
lower than their budget for fixing a wrong save after the fact.

In a dispatch session (no task bound to this session), also re-check
§4.14 (substantive-unrelated-work) on every turn. The skill is
responsible for ongoing detection; the SessionStart hook is only a
one-shot trigger.
