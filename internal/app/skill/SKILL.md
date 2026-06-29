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

# flow

This file is only the router. Load the focused files under `references/` when
their workflow applies; do not keep all workflow detail in this top-level file.

Do not edit `flow.db` directly. Use `flow` commands plus the markdown paths
printed by `flow show ...` as the source of truth. Every `~/.flow/...` path
means the active `$FLOW_ROOT` when set.

## Reference Index

- `references/bootstrap.md`: Flow-bound session startup, transcript context,
  deferred brief sections, task artifacts, resume discipline.
- `references/task-intake.md`: start-the-day, standup, task/project intake,
  waiting, archive/delete/restore, weekly review, memory search, upgrade,
  tags, binding, work_dir, brief format.
- `references/command-reference.md`: terse command cheat sheet.
- `references/kb-closeout.md`: progress notes, close-out, KB scoop mode,
  close-out KB upgrade, and backup recovery.
- `references/connectors.md`: Slack, GitHub, inbox monitor, Attention Router,
  connector wakeups, external replies.
- `references/playbooks.md`: add/run/schedule playbooks, run snapshots,
  first-run capture, and persisting run changes.
- `references/orchestration.md`: scope drift, child-task delegation,
  dependencies, owners, and cross-task handoff.

If a referenced file is missing, continue from this router and live CLI help,
but report the missing reference as a repo bug.

## 1a. When invoked explicitly with no intent

If the user only invokes `/flow` or asks to load this skill, DO NOT auto-run any workflow
and do not enter §4.1. Briefly say Flow can capture work as briefs, log
progress, resume sessions, and track waiting items. Then ask the user's intent
with header `What now?` and options like "Show me what's on my plate", "Add a
new task", "Add a project", and "Just exploring". Stop if they choose "Just
exploring" or do not answer.

## 2. The model

- **Projects** group related tasks and carry `work_dir`, priority, status, and
  a brief.
- **Tasks** are executable work with slug, status, project/dependency links,
  provider/model/session fields, brief, updates, and optional WorkContext.
- **Playbooks** are reusable runnable definitions; each run is a task snapshot.
- **Workdirs** map familiar repo names to local paths.
- **Updates** are append-only markdown history; brief "Current state" is the
  latest snapshot.

Status values are `backlog`, `in-progress`, and `done`. There is no `blocked`
status; waiting belongs in `waiting_on`. Archive hides intentional history.
Delete/remove/trash means soft-delete with restore support.

## Bootstrap Contract

For a Flow-bound execution session, do this in order before code or planning:

1. Load this skill.
2. Run `flow show task` and read the printed task brief plus every update.
3. If a project is listed, run `flow show project <slug>` and read its brief
   plus updates.
4. Read repo conventions: `AGENTS.md`, `CLAUDE.md`, and nested files under
   directories you will modify.
5. Read copied fork/source context eagerly when the task brief names it.
6. Load KB files and `other:` sidecars only when needed.
7. If any brief section is blank or unclear, ASK; do not infer.

For full details, including playbook runs, deferred sections, and transcript
handling, read `references/bootstrap.md`.

## Routing

- Status, "what should I work on", standup: read
  `references/task-intake.md`.
- Add task/project, due dates, assignees, tags, archive/delete/restore:
  read `references/task-intake.md`.
- Save update, waiting, done/close-out, KB capture: read
  `references/kb-closeout.md`.
- Slack, GitHub, Attention Router, inbox monitor, external replies:
  read `references/connectors.md`.
- Playbook add/run/schedule/capture: read `references/playbooks.md`.
- Scope drift, owners, child tasks, dependencies, `flow tell` handoffs:
  read `references/orchestration.md`.
- Exact CLI syntax: read `references/command-reference.md` or run
  `flow <command> --help`.

## Interaction Rules

- For real choices, always AskUserQuestion. Keep labels user-intent oriented;
  do not put literal `flow ...` commands in user-facing option labels.
- Operator-facing Slack/GitHub/customer text must sound like the operator wrote
  it. Do not add AI footers or signatures unless explicitly asked.
- Capture KB facts only when they are durable, surprising, and future-relevant.
  Default to writing nothing; close-out has stricter upgrade rules.
- Reports, screenshots, and generated deliverables belong under
  `$FLOW_ROOT/tasks/<task-slug>/artifacts/`.
- Treat `flow search` as a locator, not authority. Open the source it points to.

## When In Doubt

- Prefer live `flow show ...`, repo files, PR/Slack source, and CLI help over
  memory.
- Prefer lazy loading: open reference files only when their workflow applies.
- Do not infer missing brief sections.
- Do not modify unrelated user changes.
- Do not invent commands, connector behavior, or external state.
