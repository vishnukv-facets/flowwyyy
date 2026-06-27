## 9. The execution-session bootstrap contract

When `flow do <task>` spawns a Claude session in a new terminal tab, it
pre-allocates a UUID, writes it to `tasks.session_id` before spawning,
and passes it to `claude --session-id <uuid>`. This makes the session's
jsonl file appear at the deterministic path
`~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`. For Codex sessions
(`flow do <task> --agent codex`), Codex owns the session id; flow launches
interactive `codex`, then captures the generated id from Codex's JSONL
session store and writes it back to `tasks.session_id`. During that capture
window a Codex task can be `in-progress` with an empty `session_id`.

Subsequent `flow do <same-task>` calls read `session_provider` and
`session_id`, then spawn either `claude --resume <uuid>` or
`codex resume <id>` to continue the same conversation.

**If you are the execution session spawned by `flow do`:**

Do ALL of the following in order, before touching any code or
proposing any plan:

1. **Invoke the flow skill via the `Skill` tool.** The `flow hook
   session-start` output already names this step, but the hook is
   belt-and-braces — the Skill tool is the authoritative way to load
   the operating manual that governs workflows, KB discipline, and
   scope-creep detection.

2. **Load the task context:**
   ```
   flow show task
   ```
   From its output, use the `Read` tool on:
   - The file at the `brief:` path (the task brief — the problem
     statement the user captured when creating this task).
   - Every file listed under `updates:` (prior progress notes, in
     chronological order — skim for blockers and decisions).

   **Do NOT read the `kb:` files at bootstrap.** They're lazy-loaded
   on demand — see §5.10 for when to actually Read them.

   **If `flow show task` indicates `kind: playbook_run`:** also run
   `flow show playbook <playbook-slug>` first (for context: the playbook's
   intent and recent runs). Note any files under its `other:` section —
   they're sidecar references you can load on demand. Then read your
   task's `brief.md` — that's the snapshot taken when this run started,
   and it's your authoritative instructions. The playbook's live
   `brief.md` may have evolved since; you don't need to re-read it.

   **Files listed under `other:`** in any `flow show` output (task,
   project, or playbook) are sidecar references — research notes, decision
   trees, design docs, etc. dropped into the entity's directory. Do **not**
   read them eagerly. Read them on demand when something in the brief, in
   user input, or in the work makes them relevant. This matches the
   lazy-load principle for KB files (§5.10 in the skill, §4.10 in the
   section numbering).

   **If the task is a forked provider-handoff task:** its brief will include a
   "Fork lineage" section and copied source-context files such as
   `source-brief.md`, `source-transcript.md`, and `source-*.md` sidecars.
   Read those copied source files eagerly after the task brief and updates.
   They are not optional references; they are the historical context that lets
   Claude or Codex continue from the source task without the user re-explaining
   the work. Copied source updates live under this task's own `updates/`
   directory with `source-` prefixes, so they are already covered by the normal
   update-read step above.

   After bootstrap, if you need cross-task or memory context and do not
   know the exact file/entity to open, use `flow search "<terms>"` as the
   locator. Default search covers briefs, updates, and memories; add
   `--in all` only when transcripts are needed too. See §4.9a.

3. **Load the parent project context, if any.** If `flow show task`
   printed a `project:` line that isn't `(floating)`, run:
   ```
   flow show project <project-slug>
   ```
   (or just `flow show project` — it defaults to the bound task's project).
   From its output, use `Read` on:
   - The file at its `brief:` path (the project brief — overarching
     context, goals, scope shared across sibling tasks).
   - Every file listed under its `updates:` (project-level progress
     notes — often capture cross-task decisions and blockers that
     matter for your task even if your task's own updates don't
     mention them).

   Again, skip the project's `kb:` section at bootstrap.

4. **Load repo conventions.** Read `AGENTS.md` and/or `CLAUDE.md` in
   your `work_dir` (if present), plus any nested convention files under
   subdirectories you plan to modify. These are authoritative for build
   commands, test commands, style, and gotchas — they override any
   assumption you might make from the brief.

5. **Only then begin work.** If any brief section is blank or
   unclear, ASK the user before inferring. If the user didn't
   specify a "Done when" in the brief, confirm acceptance criteria
   with them before making changes.

**Throughout the session**, watch for new KB-worthy facts per §5.10 and
append them to the matching `kb/*.md` file on the fly — no permission
needed, no interview required. Just write and quietly note what you
recorded. And lazy-read any kb file when you hit a question that
actually needs that context — not before.

### Deferred-section prompt

If any section body in your brief is the literal `*Deferred — fill in at
task start.*` or `*Deferred*`, pause before doing any work and offer the
user (via AskUserQuestion):

- **Fill in now** — run a mini-§4.2 interview for just the missing
  sections (Why, Done when, Out of scope, Open questions). Save the
  filled-in brief by overwriting the existing `brief.md`.
- **Skip — proceed** — accept that scope is implicit. Reasonable for
  small/known tasks.

This shifts the intake burden from intake-time to task-start-time, where
the user has more context.

Applies only to regular tasks (kind=regular). Playbook-run briefs are
snapshots and should not be edited; if the live playbook brief had
deferred sections, those should have been resolved at playbook intake.

### Cross-task context via transcripts

If you need to understand what happened in a sibling task's session
(e.g. a prior task under the same project made decisions that affect
yours), use:

```
flow transcript <sibling-task-slug>
```

This outputs a readable conversation transcript from that task's agent
session — user messages, assistant messages, tool calls, and results.
Use `--compact` to omit tool results and thinking blocks for a shorter
overview. Pipe through `grep` or `head` if the full transcript is too
long to read at once.

**When to use:** When the brief and updates for a sibling task don't
give you enough context, or when you need to understand specific
implementation decisions made during that task's session.

### Field edits — `flow update task` / `flow update project`

`flow update task` is the canonical lane for in-place field edits on
a task. `flow update project` is the same for project rows (priority
only, for now). All field setters live here — there are no per-field
mini-commands like `flow priority` / `flow due` / `flow waiting` /
`flow assignee` (those used to exist; they were folded into update).

```
flow update task <ref>
    [--work-dir <path>] [--mkdir]
    [--status backlog|in-progress|done]
    [--priority high|medium|low]
    [--agent claude|codex]   (backlog tasks only — locked once a session starts)
    [--model <m>] [--clear-model]   (backlog tasks only — locked once a session starts)
    [--assignee <name>] [--clear-assignee]
    [--due-date <date>]   [--clear-due]
    [--depends-on <slug> ...] [--remove-dep <slug> ...] [--clear-deps]
    [--subtask-of <slug>] [--unparent]
    [--waiting "<who or what>"] [--clear-waiting]
    [--project <slug>] [--clear-project]
    [--tag <t> ...] [--remove-tag <t> ...] [--clear-tags]
    (deprecated: --parent / --remove-parent / --clear-parent are aliases for --depends-on / --remove-dep / --clear-deps)

flow update project <ref>
    [--priority high|medium|low]

flow update playbook <ref>
    [--slug <s>] [--name <n>] [--work-dir <path>] [--mkdir]
    [--project <slug>] [--clear-project]
```

When to use which flag:

- **`--work-dir <path>`** — the repo moved on disk (renamed parent,
  moved between drives, cloned to a new path). Pass `--mkdir` if the
  new path doesn't exist yet.
- **`--status <s>`** — primary use case is rolling a `done` task back
  to `in-progress` so `flow do` will reopen it (the do-from-done path
  is gated). Also handy for in-progress → backlog to "demote" a task
  you're not actively working on. Setting backlog → in-progress on a
  non-Codex task with NULL session_id errors with a pointer at `flow do` /
  `flow do --here` — those are the paths that attach an agent session.
  Codex may briefly have NULL session_id only while flow is capturing the
  id after a fresh Codex launch. Setting status to a value it already has
  is a no-op.
- **`--priority <p>`** — change a task or project priority. Same enum
  as creation: high|medium|low.
- **`--agent claude|codex`** (or `--codex` / `--claude`) — change the
  session agent. Allowed **only while the task is in backlog and no
  session has started**; once a session exists (running, idle, or done)
  the agent is locked and the command errors. This is the CLI twin of
  the UI's inline agent picker, which only appears for backlog tasks.
- **`--model <m>` / `--clear-model`** — set or clear the session model
  override. Allowed **only while the task is in backlog and no session has
  started** (same lock as `--agent`; mid-session model switching is out of
  scope). `--model` accepts a tier alias the provider resolves
  (Claude: `opus`/`sonnet`/`haiku`; Codex: `gpt-5.4-mini`/`gpt-5.4`/`gpt-5.5`)
  or any raw model id the CLI accepts — flow passes it through verbatim.
  `--clear-model` drops the override so the model is auto-resolved at launch
  (see §4.4a). The two flags are mutually exclusive.
  assignee. Convention: NULL = "self" (default); any other value =
  "assigned to that name". The list/show output surfaces the assignee
  only when it's non-null.
- **`--due-date <date>` / `--clear-due`** — set or clear the due date.
  Date formats: `YYYY-MM-DD`, `today`, `tomorrow`, weekday names, `Nd`.
- **`--depends-on <slug>` / `--remove-dep <slug>` / `--clear-deps`** —
  add, remove, or clear *blocking dependencies* (a DAG). A task with
  unfinished `depends-on` edges is treated as blocked: the graph view
  and execution order reflect these edges. `--depends-on` and
  `--remove-dep` are both repeatable. Use this when the task literally
  cannot or should not start until the dep is done; use `waiting_on`
  for looser external blockers (waiting on a person, a PR, an external
  review). Visible in `flow show task` under `depends on:` and `blocks:`.
  Deprecated aliases: `--parent` / `--remove-parent` / `--clear-parent`
  (the binary prints a warning but still applies the dep change).
- **`--subtask-of <slug>` / `--unparent`** — set or clear the
  *organizational hierarchy parent* (non-blocking). Use this to nest a
  task under a larger piece of work for display and grouping purposes —
  the subtask can start and finish independently of its parent.
  Visible in `flow show task` under `subtask of:` and `subtasks:`.
- **`--waiting "<X>"` / `--clear-waiting`** — set or clear the
  `waiting_on` freeform note (see §4.6). Status stays in-progress;
  the note is just there to remind the user.
- **`--project <slug>` / `--clear-project`** — attach a task to an
  existing project, or detach it back to floating. The target project
  must exist and not be archived/deleted; archived/deleted projects
  are rejected so updates don't get orphaned under hidden containers.
  Swap is silent (no confirmation), matching `--priority` /
  `--assignee` semantics — if the user said "attach to X", just do it.
  Attaching a project also **adopts the project's `work_dir`** when the
  task is still sitting in its auto-created throwaway workspace
  (`~/.flow/tasks/<slug>/workspace/`) and you didn't pass an explicit
  `--work-dir` in the same command — so a project-attached task runs in
  the real repo, not a clone. A deliberately-chosen `work_dir` is never
  clobbered. (If the task already has a session in the old workspace, the
  command prints a note: reopen with `flow do <slug> --fresh` to start in
  the repo.) On `flow update playbook`, the same flags re-project the
  playbook for **future runs only**; existing `kind=playbook_run` rows
  keep the `project_slug` they snapshotted at run time and are not
  retroactively rewritten.

There is **no** `--session-id` flag. The session_id is owned by
`flow do`, Codex capture, or `flow do --here`; manual rewriting was a
foot-gun (silent overwrite of an existing binding) and the lane is gone.
Use `flow do --here <slug>` from inside the Claude or Codex session you want to bind.

At least one field-changing flag must be given. `--work-dir` is an
escape hatch — do not run it as a workaround for a bug in `flow do`;
surface the bug instead.
