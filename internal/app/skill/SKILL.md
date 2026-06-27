---
name: flow
description: |
  Personal task, project, playbook, and agent-session manager. CLI binary is
  `flow` (assumed on PATH) and stores metadata in ~/.flow/flow.db (SQLite).
  Use this skill when the user asks about their work, tasks, projects,
  playbooks, in-flight Claude/Codex sessions, or natural phrasing around
  "what's left", "what's pending", "what should I work on", "status",
  "start my day", "add a task", "resume work", "pick up where I left off",
  "save a note", "write an update", "waiting", "blocked", "mark done",
  "archive", "delete", "restore", "weekly review", "inspect transcripts",
  or when the user invokes any `flow <subcommand>` directly.
---

# flow - task and session manager skill

This is the always-loaded router for Flow. Keep this file small. Load the
focused files under `references/` only when their workflow applies.

Do not edit `flow.db` directly. Use the `flow` CLI and markdown files printed by
`flow show ...` as the source of truth.

Every `~/.flow/...` path means the active `$FLOW_ROOT` when set. Do not rebuild
paths from memory when `flow show task` or `flow show project` prints the exact
brief, updates, kb, artifacts, or worktree paths.

## Reference Index

- `references/bootstrap.md` - execution-session bootstrap, deferred sections,
  transcript context, field edits, task artifacts, and resume discipline.
- `references/task-intake.md` - start-the-day, standup, task/project intake,
  save updates, waiting, archive/delete, weekly review, memory search, upgrade,
  tagging, binding, work_dir, brief format, and anti-patterns.
- `references/command-reference.md` - command cheat sheet and update/tag flags.
- `references/kb-closeout.md` - progress notes, waiting, done, close-out value,
  KB scoop mode, close-out KB upgrade, and backup recovery.
- `references/connectors.md` - task identity, same-session inbox monitor, Slack
  reply tasks, Slack command DMs, GitHub PR/issue tasks, and Attention Router.
- `references/playbooks.md` - add/run/schedule playbooks, run snapshots,
  first-run capture, and persisting run adjustments back to the playbook.
- `references/orchestration.md` - scope-creep detection, substantive-unrelated
  work checks, child-task delegation, owners, and "when in doubt" guidance.

If a referenced file is missing, continue from this file and the live CLI help,
but report the missing reference as a repo bug before finishing the task.

## 1. What flow is

Flow tracks work as projects, tasks, playbooks, updates, and sessions:

- Projects group tasks and carry a `work_dir`, priority, status, and brief.
- Tasks are executable work with a slug, `work_dir`, status, optional project,
  optional dependencies/subtask links, optional assignee/due date, provider,
  model, session id, brief, and updates.
- Playbooks are reusable runnable definitions; each run is a task snapshot.
- Workdirs map familiar repo names to local paths.
- Updates are append-only markdown history; brief "Current state" is the latest
  snapshot and can be changed with `flow update ... --brief-status`.

Status values are `backlog`, `in-progress`, and `done`. There is no `blocked`
status; waiting belongs in `waiting_on`.

Archive means "hide but keep as intentional history." Delete/remove/trash means
soft-delete and restore remains possible. See `references/task-intake.md`.

## 1a. When invoked explicitly with no intent

If the user only invokes `/flow` or asks to load this skill, DO NOT auto-run any workflow
and do not enter §4.1 or the start-the-day flow. Briefly say what you can do:
capture work as briefs, log progress, resume sessions, and track waiting items.
Then use `AskUserQuestion` with header `What now?` and options like:

- "Show me what's on my plate"
- "Add a new task"
- "Add a project"
- "Just exploring"

If the user chooses "Just exploring" or does not answer, stop.

## 2. The model

- **Projects** group related tasks.
- **Tasks** are executable units of work.
- **Playbooks** are reusable runnable definitions; each run is a task snapshot.
- **Workdirs** map familiar repo names to local paths.
- **Updates** are append-only markdown history.

Load `references/playbooks.md` for the full playbook model and
`references/task-intake.md` for the full task/project model.

## 3. Session Bootstrap

The first time you are about to run a `flow` command in a session, probe with
`flow list tasks` or `flow list projects`. If Flow is not initialized, ask via
`AskUserQuestion` before running `flow init`.

For a Flow-bound execution session, the bootstrap order is mandatory:

1. Load this skill/manual first.
2. Run `flow show task` (or `flow show task <slug>` if needed).
3. Read the task brief and every file listed under `updates:`.
4. If a project is listed, run `flow show project <slug>` and read its brief and
   every project update listed under `updates:`.
5. Read repo convention files: `AGENTS.md`, `CLAUDE.md`, and nested convention
   files under directories you will modify.
6. Files listed under `other:` are sidecar references; load on demand when
   relevant, not eagerly.
7. If any brief section is blank or unclear, ASK; do not infer.

After bootstrap, use the task brief, project brief, updates, and repo
conventions as the live contract. If they conflict, repo/user instructions win
over this generic skill.

For the full bootstrap contract, including deferred-section prompts, transcript
context, and field edits, read `references/bootstrap.md`.

## 4. AskUserQuestion Discipline

Use `AskUserQuestion` for real choices. Do not bury options in prose when the
user is choosing scope, priority, due date, owner, start/skip, closeout, archive,
delete, persist-to-playbook, or route-to-child-task.

The rule is "always AskUserQuestion" when a workflow branches. Keep options
short, concrete, and mutually exclusive. If a choice can safely default, make
the recommended option first and explain the tradeoff in one sentence.

Do not put literal `flow ...` commands in user-facing option labels. The user
describes intent; you run the CLI.

## 5. Core Workflow Routing

Load the relevant reference before executing a workflow beyond the short rules
here.

- "What should I work on", "start my day", "status": use
  `references/task-intake.md` for start-the-day and standup routing.
- "Add a task": interview first; never solve during intake. Ask required
  sections in order, then optional sections. See `references/task-intake.md`.
- "Add a project": capture project intent and work_dir. See
  `references/task-intake.md`.
- "Start/resume work": use `flow do <task>` only after confirming the task is
  the right one and prerequisites are loaded. See `references/bootstrap.md`.
- "Save note", "write update", "waiting on": write append-only updates and/or
  set `waiting_on`. See `references/kb-closeout.md`.
- "Done", "shipped", "merged", "deployed", "that's working": close-out is a
  durable-learning moment, not just bookkeeping. See `references/kb-closeout.md`.
- "Archive", "delete", "restore", "weekly review", "search memory": use
  `references/task-intake.md`.
- "Add/run/schedule a playbook": use `references/playbooks.md`.
- Slack, GitHub, Attention Router, inbox monitor, or connector wakeups: use
  `references/connectors.md`.
- Owner ticks, child tasks, autonomous delegation, or scope drift: use
  `references/orchestration.md`.

## 6. Knowledge-Base Scoop Mode

Capture KB facts only when they are durable, surprising, and future-relevant to
the user, org, products, processes, or business. Default to writing nothing.

When capturing, dedupe first, then add one dated bullet to the right file under
`$FLOW_ROOT/kb/` (`user.md`, `org.md`, `products.md`, `processes.md`,
`business.md`). Real-time scoop is append-only unless the current conversation
makes an existing entry stale; then update/upgrade in place.

At task close-out, run the stricter close-out sweep from
`references/kb-closeout.md`: promote only durable learnings, update stale
provisional entries, write a project/task update when appropriate, and avoid
announcing no-op KB work.

## 7. Scope And Drift

Scope-creep detection is passive and ongoing. Re-evaluate on every turn. If the
user asks for substantive unrelated work while inside a task, pause and offer a
choice: continue current task, create/switch to a new Flow task, or explicitly
expand scope. Use `AskUserQuestion`.

If the unrelated work is creative/product-shaping, load the appropriate process
skill first. See `references/orchestration.md` for process-skill ordering.

## 8. Task Artifacts Directory

Reports, generated data, screenshots, and other deliverables for a task go under
`$FLOW_ROOT/tasks/<task-slug>/artifacts/`. Mission Control's task Artifacts tab
reads that directory.

Do not write new deliverables as top-level files next to the task brief;
top-level `.md` files are `other:` context, not UI artifacts.

## 9. Operator Voice

Outbound Slack, GitHub, PR, or customer-facing text must sound like the operator
personally typed it. Match their tone, formality, capitalization, and brevity.
Do not add signatures, disclaimers, AI-assistant phrasing, or "Sent via ..."
footers unless the user explicitly asks.

For Slack writes, direct send requests are direct sends unless the user asks for
a draft. Keep messages short and operational.

## 10. Closeout

When work is genuinely complete, prompt for closure if the user has not already
asked. `flow done <slug>` triggers the close-out sweep and may schedule session
cleanup. Do not let completed work end with durable knowledge trapped only in
the transcript.

Before closeout, verify the requested work, record important updates, capture
only strict-bar KB facts, and mention any tests or checks that could not run.

## 11. When In Doubt

- Prefer live `flow show ...`, repo files, and CLI help over memory.
- Prefer lazy loading: read reference files when the workflow needs them.
- Do not infer missing brief sections.
- Do not modify unrelated user changes.
- Do not invent commands, connector behavior, or external state.
- If the user asks a simple status question, answer from live Flow state first.
- If a command fails because Flow is uninitialized, ask before initializing.
- If `flow search` is only a locator, open the source it points to before
  treating it as authority.
