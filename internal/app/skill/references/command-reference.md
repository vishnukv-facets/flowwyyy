## 4. Command reference

This is a terse cheat sheet. Use `flow <command> --help` for up-to-date
flags.

```
Setup
  flow init                                 create ~/.flow/, init DB, install skill
  flow skill install [--force]              (re)install the skill file
  flow skill uninstall                      remove the skill
  flow skill update                         install --force after upgrading the binary

Create
  flow add project "<name>" --work-dir <path> [--slug <s>] [--priority h|m|l] [--mkdir]
  flow add task    "<name>" --agent claude|codex   (--agent is REQUIRED — no default)
                           [--slug <s>] [--project <slug>] [--work-dir <path>] [--mkdir]
                           [--priority high|medium|low] [--due <date>] [--assignee <name>]
                           [--permission-mode default|auto|bypass] [--model <m>]   (--codex / --claude are shortcuts)
                           [--depends-on <slug> ...]   (repeatable; blocked by these tasks until they are done)
                           [--subtask-of <slug>]       (organizational hierarchy parent, non-blocking)
  flow add playbook "<name>" --work-dir <path> [--slug <s>] [--project <slug>] [--mkdir]

Sessions
  flow do               <ref> [--agent claude|codex] [--fresh] [--dangerously-skip-permissions] [--force]
  flow do --here        <ref> [--force]   (bind THIS Claude/Codex session to the task — no new tab)
  flow done             <ref>

Playbook runs
  flow run playbook <slug> [--agent claude|codex]  spawn a fresh run session (new task with kind=playbook_run)
  flow list runs [<playbook-slug>]  list playbook runs (filter by playbook optional)

Read
  flow show task    [<ref>]     (no arg → $FLOW_TASK, then current-session reverse-lookup)
  flow show project [<ref>]     (no arg → project of current/bound task)
  flow show playbook    [<ref>]
  flow standup [--for today|monday|24h] [--clipboard]
                     generate a copyable briefing from Attention, waiting, stale, ready, and recent activity
  flow search "<query>" [--in briefs,updates,memories,transcripts] [--limit N] [--format table|json|tsv]
  flow transcript   [<ref>] [--compact]    (readable transcript from session jsonl)
  flow read ask "<question>" [--key <dedupe-key>]
                     publish a structured pending question from this session; replies should use flow tell
  flow read say "<note>" [--key <dedupe-key>]
                     publish a structured unread status/finding from this session
  flow read list [--status pending|unread|read|answered|all] [--format table|json]
                     list structured questions and notes with task/chat/context metadata
  flow read mark <id> (--read|--answered)
                     mark a question/note read or answered after handling it
  flow context bind [--context <id-or-slug>] [--title <title>] [--slug <slug>]
                    [--task <slug>] [--chat <slug>]
                    [--anchor-type <type>] [--external-id <id>] [--url <url>] [--label <text>]
                     bind tasks, chats, and source anchors to the same shared WorkContext
  flow context inspect <id-or-slug|task:<slug>|chat:<slug>>
                     inspect current WorkContext, anchors, edges, and recent provenance events
  flow list tasks    [--status backlog|in-progress|done] [--project <slug>]
                     [--priority high|medium|low] [--since today|monday|7d|YYYY-MM-DD]
                     [--include-archived] [--include-deleted|--deleted]
  flow list projects [--status active|done] [--include-archived] [--include-deleted|--deleted]
  flow list playbooks   [--project <slug>] [--include-archived] [--include-deleted|--deleted]

Attention
  flow attention list [--status new|acted|dismissed|snoozed|all]
                     show Attention Router cards awaiting operator review
  flow attention act <id> <make-task|forward|confirm-handoff|dismiss>
                     perform a simple operator-approved feed action
  flow attention sent <id> [--close-floating <floating-id>]
                     mark an approved send-reply card sent after confirmed post
  flow attention trace [--since 24h] [--disposition dropped|surfaced|error|all] [--limit 50]
                     inspect the steering decision funnel and trace rows
  flow attention feedback [--group source|channel|author|thread-type|suggested-action|confidence-band]
                     report Attention feedback approval/dismiss rates by one dimension
  flow attention calibration
                     per (action × raw confidence band): the observed operator-agreement
                     rate (calibrated confidence) vs the raw band the model emitted
  flow attention handoff accept <correlation-id> --reason "<why>"
                     accept an Attention confirmed handoff from this task's inbox
  flow attention handoff decline <correlation-id> --reason "<why>"
                     decline an Attention confirmed handoff and keep the feed card open

Edit / mutate
  flow edit           <ref>          opens brief.md in $EDITOR, bumps updated_at
  flow update task    <ref> [--work-dir <path>] [--mkdir]
                            [--status backlog|in-progress|done] [--priority high|medium|low]
                            [--assignee <name>] [--clear-assignee]
                            [--due-date <date>] [--clear-due]
                            [--depends-on <slug> ...] [--remove-dep <slug> ...] [--clear-deps]
                            [--subtask-of <slug>] [--unparent]
                            [--waiting "<who or what>"] [--clear-waiting]
                            [--project <slug>] [--clear-project]
                            [--model <m>] [--clear-model]   (backlog tasks only)
                            [--brief-status "<1–3 lines>"]   (refresh brief Current state; "-" = stdin)
                            (deprecated: --parent / --remove-parent / --clear-parent are aliases for --depends-on / --remove-dep / --clear-deps)
  flow update project <ref> [--priority high|medium|low]
  flow update playbook <ref> [--slug <s>] [--name <n>] [--work-dir <path>] [--mkdir]
                             [--project <slug>] [--clear-project]
  flow archive        <ref>
  flow unarchive      <ref>
  flow delete         <ref>
  flow restore        <ref>
  (flow edit, archive, unarchive, delete, and restore also accept playbook refs)

Workdirs
  flow workdir list
  flow workdir add <path> [--name <nickname>]   (captures origin remote if present)
  flow workdir remove <path>
  flow workdir scan [<root>] [--add]            (backfills origin remotes for detected repos)

Orchestration (parent-child agents — see §4.17)
  flow spawn <name> [--parent <slug>] [--depends-on <slug> ...] [--prompt <text>]
                    [--project <slug>] [--work-dir <path>] [--slug <s>]
                    [--priority h|m|l] [--agent claude|codex] [--no-open]
       Create a task and immediately open it in a new tab. --parent sets the
       organizational hierarchy parent (subtask-of, non-blocking); --depends-on
       (repeatable) adds blocking dependency edges. The prompt becomes the
       task's brief "What" section so the spawned agent starts with context.

  flow tell <task-slug> "<message>" [--from <slug-or-label>]
       Append a stamped message to ~/.flow/tasks/<slug>/inbox.md and an
       actionable flow_tell event to inbox.jsonl. A live Flow terminal is
       woken through the same inbox monitor used for Slack/GitHub events;
       otherwise the receiving agent sees the message on next SessionStart.
       Use this to nudge a sibling task without spawning a new conversation.

  flow wait <task-slug> --until <state> [--timeout <dur>]
       Block until the task reaches the requested state. <state> can be
       a task lifecycle (backlog|in-progress|done) or an agent runtime
       state (running|waiting|idle|dead|released). Uses /ws/events for
       live updates; short-circuits when already in state.
```

All references (`<ref>`) resolve by **exact slug match only**. There is
no fuzzy or substring matching. Use `--slug` to pick a short, memorable
slug at creation time (e.g. `--slug caas-exit`). If omitted, a slug is
auto-generated from the name (truncated to ~6 words).

### 4.16a Tagging tasks

**Triggers:** "tag this task as X", "add a tag X to <task>", "what
tags does <task> have", "show all tags", "list my tags", "what tags
are in use", "find all tasks tagged X".

**What tags are:** free-form single-string labels attached to tasks
for cross-cutting identification — `#frontend`, `#urgent`,
`#tech-debt`, `#h2-2026`, `#triage`, `#research`. Stored normalized
(lowercase, trimmed). Many-to-many: a task can have any number of
tags, a tag can be on any number of tasks. Tags are *single strings*
— if you want kv-style semantics (`type:bug`, `priority:p0`), use a
`key:value` colon convention inside the string. Don't introduce a
parallel kv schema.

**The vocabulary discipline rule:** before suggesting a new tag for
a task, ALWAYS run `flow list tags` first. That command lists every
tag in use across non-archived tasks with a per-tag task count. Reuse
existing tags whenever they fit — the user's tag vocabulary is more
useful when it stays consistent. Inventing a synonym (e.g.
`#frontend` when `#ui` already has 8 tasks) fragments the tag space
and makes filtering useless.

**Recipe — add tags:**

1. Run `flow list tags` and read the output. Note any existing tag
   that matches the user's intent.
2. If a good match exists, propose it via AskUserQuestion (header:
   "Use existing tag?", options: existing-tag candidates + "Use a
   new tag"). Skip this step if the user already named the exact
   tag.
3. Run `flow update task <ref> --tag <tag1> --tag <tag2> ...`.
   `--tag` is repeatable — pass it once per tag value. Tags are
   normalized to lowercase + trimmed; idempotent on duplicates.

**Recipe — remove or clear:**

- `flow update task <ref> --remove-tag <tag1> --remove-tag <tag2>`
  removes specific tags (also repeatable).
- `flow update task <ref> --clear-tags` removes all tags from a task.
  Confirm via AskUserQuestion (header: "Clear all tags?", options:
  "Yes, clear all" / "No, name specific tags") before mutating —
  clearing is destructive and per §8 every state mutation deserves
  a click.
- `--clear-tags` and `--remove-tag` are mutually exclusive (clear
  removes everything anyway). `--clear-tags --tag <new>` is allowed
  and means "wipe and replace with `<new>`" — useful for retagging.

**Recipe — find tasks by tag:**

- `flow list tasks --tag <tag>` filters the task list to that tag.
  Combine with `--status`, `--project`, `--priority`, etc.

**Display:** list/show output renders tags as `#tag1 #tag2` tokens
trailing the row. The hashtag prefix is render-only — do not type
`#` into `--tag` values (it would be normalized away, but treat the
rule as "tag values are unprefixed strings").

**Anti-patterns:**

- **Do not invent new tags without checking `flow list tags` first.**
  The vocabulary discipline rule isn't optional.
- **Do not use kv-style alternative storage.** Single strings with
  `key:value` convention are the canonical form.
- **Do not auto-tag.** Always confirm with the user before adding
  tags they didn't explicitly name. The exception is when the user's
  request literally names the tag ("tag this `#frontend`").
