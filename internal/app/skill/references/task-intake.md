## 4a. Interactive choices (use `AskUserQuestion` everywhere)

**This section overrides any inline prose phrasing later in the skill.**
If a later section says "offer X", "ask Y", or "confirm Z", that
always means "invoke `AskUserQuestion` with appropriate options" —
never a prose question typed into the chat.

Every choice the user makes — always AskUserQuestion, never a prose
question. Yes/no confirmations, pick-one-of-several, priority, slug
suggestions, project attachment, mutation confirmations, "want me to
do X?" — every single one runs through the tool so the user can click
to select instead of typing. Common patterns:

| Pattern | Options |
|---------|---------|
| Yes / No | Two options with contextual labels (e.g. "Save it" / "Revise", "Open now" / "Not now") |
| Pick from list | One option per candidate (tasks, projects, slugs) |
| Priority | "High", "Medium", "Low" |
| Mutation confirm | "Yes, do it" / "No, wait" with the action named in the description |

Keep `header` under 12 chars. Put enough context in `question` so
the choice is clear without scrolling back. If the user already
answered in their message, don't re-ask — just use their answer.

**Prose questions are deprecated.** Don't write "Want me to do X?"
or "Should I do Y?" or "(yes/no)" in chat — those force the user to
type a free-text reply. The tool produces clickable options; always
prefer the tool.

**Mid-interview drift.** Within an open-ended interview (intake,
deferred-section prompt), the parent question may be free-form
("Why?", "Done when?") but follow-up clarifications often narrow into
enumerable choices (architectures, install methods, yes/no). The
moment a sub-question has 2–4 discrete options, switch to
AskUserQuestion. Don't keep typing prose just because you started in
prose. The "interview" framing governs the *opening* question; every
narrowing inside it follows the same always-AskUserQuestion rule as
the rest of the skill.

## 5. Core workflows

These are the load-bearing part of the skill. When the user says one of
the trigger phrases, follow the corresponding recipe exactly.

### 4.1 Start the day

**Triggers:** "start my day", "what should I do today", "what am I
working on", "where did I leave off", "give me a status".

**Recipe:**

1. Run `flow attention list --status new`, `flow list projects`, and
   `flow list tasks --status in-progress`.
2. Run `flow list tasks --status backlog --priority high`.
3. Read the `waiting_on` and stale markers in the tasks output, and
   inspect any Attention cards as operator-review items (do not act on
   them automatically).
4. Summarize in these sections:
   - **Attention**: new Attention Router cards, with source, suggested
     action, confidence, matched task, and the shortest "why this"
     reason you can infer from the card/trace. If no cards exist, say
     there are no new Attention cards.
   - **In flight** (`in-progress`): 1 bullet per task, include any ⚠
     stale marker and any `[waiting: ...]` note.
   - **High-priority backlog**: 1 bullet per backlog task marked high.
   - **Waiting on someone**: pull out tasks with `waiting_on` set so the
     user can see the whole block at once.
   - **Stale** (anything with the ⚠ marker): call these out explicitly.
   - **Active playbooks**: any playbook with a run in the past 7 days.
     Pull from `flow list runs --since 7d` grouped by playbook; show
     playbook slug + most recent run timestamp. Skip if there are no
     runs in the window — don't show an empty header.
5. Use `AskUserQuestion` to let the user pick which task to work on.
   List each in-progress and high-priority backlog task as an option
   (label = slug, description = one-line summary). Include an "Add a
   new task" option if appropriate.

Do not auto-run `flow do` after listing. Wait for the user to pick.

### 4.1a Copyable standup / briefing

**Triggers:** "daily digest", "morning briefing", "standup", "brief me",
"copyable status", "what changed since yesterday".

**Recipe:**

1. Run `flow standup --for today` unless the user asks for Monday/week-start
   or the last 24 hours.
2. If the user asks for something they can paste elsewhere, run
   `flow standup --for today --clipboard` and still summarize the result in
   chat.
3. Treat this as read-only synthesis. Do not choose a task, open a session,
   or act on Attention cards from the briefing.
4. Make clear that the briefing separates **needs action** from **FYI** and
   groups items by project/source/urgency.

This does not replace the interactive start-the-day picker in §4.1. If the
user asks "what should I work on" or wants to pick next work, use §4.1 and
AskUserQuestion instead of only dumping a briefing.

### 4.2 Add a task — INTERVIEW MODE (mandatory)

**Triggers:** "add a task", "new task", "track this work", "let me add a
flow task for X".

**The interview is the whole point.** The skill's value vs. "just run
`flow add task`" is that you interview the user before saving. You NEVER
solution during intake. You NEVER fill blanks with guesses. If a section
is unclear, ask. If the user says "I don't know yet", write "Open
question: ..." in the brief and move on.

**Required sections (always asked, in this order):**

1. **Name** — one-sentence description of the work. Example: "Add OAuth
   login to the budgeting app."
2. **Slug** — short, memorable, ASCII. Use AskUserQuestion to suggest 2–3
   candidates derived from the name. User picks one or types a custom
   slug.
3. **Project or adhoc.** Before saving, tie the task to an existing
   project whenever it naturally belongs to one. If it does not belong to
   any project, capture that explicitly as adhoc/floating rather than
   silently omitting `--project`.
4. **Where?** — work_dir for the task. Use the §6 recipe. If a project is
   selected, default to that project's `work_dir`.
5. **Priority** — High / Medium / Low via AskUserQuestion. Default Medium.
6. **Agent (MANDATORY — never skip)** — claude or codex via
   AskUserQuestion. There is **no default**: `flow add task` now *requires*
   `--agent` and exits with a usage error if it is missing, so this question
   must always be asked, whether a human or an agent is creating the task.
   Ask "Which agent runs this task's sessions?" with options "Claude Code" /
   "Codex", and pass the answer as `--agent claude` / `--agent codex` (or the
   `--codex` / `--claude` shortcut). This is as non-negotiable as Name —
   you cannot save a task without it.
7. **Assignee** — always ask, easy to skip. Use AskUserQuestion with
   "Me (self)" as the default one-click choice and "Someone else" for a
   typed name. The default is self (NULL); pass `--assignee <name>` only when
   the user chooses someone else.
8. **Due date** — always ask, easy to skip. Use AskUserQuestion with
   "No due date" as the default one-click choice, plus common options such as
   "Today" / "This week" and an "Other" path for `YYYY-MM-DD`, `tomorrow`,
   weekday names, or `Nd`. Pass `--due <date>` only when the user sets one.

**Optional sections (offered, can be deferred):**

After the required fields above, use AskUserQuestion:

> "Want to capture more detail now (Why, Done when, Out of scope, Open
> questions), or defer until you start the task?"
> - Detail now (recommended for tasks you'll start later)
> - Defer until you start the task

**Detail now:** run the rest of the original §4.2 sections — Why, Done
when, Out of scope, Open questions — and draft the full brief. Use the
full task-brief template from §7.

**Defer:** save the task with a thin brief (template in §7). The
bootstrap-time prompt (§9 deferred-section prompt) will walk the user
through the missing sections when they `flow do` the task — at which
point the user has more context and is more motivated to think about
acceptance criteria.

**Confirmation flow** (both paths):
- Show the drafted brief.
- AskUserQuestion: "Brief — Save it / Revise"
- Save → `flow add task ...` → update the brief stub the binary
  just wrote with the drafted content (use `Edit` with
  `replace_all: true` after a single `Read`, or `Write` after a
  single `Read` — both are 2 tool calls; pick whichever feels
  natural).

**Then, BEFORE calling `flow add task`:**

- **Ask for a short slug.** Suggest 2–3 slug candidates derived from
  the task name (e.g. for "Add OAuth to budgeting app" suggest `oauth`,
  `auth-budget`, `oauth-budget`). Present them via `AskUserQuestion`
  so the user can click one (the "Other" option lets them type a custom
  slug). If the user picks Other and leaves it blank, omit `--slug`.
- **Project attachment.** Use `AskUserQuestion` with one option per
  existing project (label = slug, description = project name) plus an
  explicit "Adhoc / no project" option. If there are no projects, say
  that the task will be adhoc unless the user wants to create a project
  first. Do not silently create floating tasks.
- **Dependencies (easy to skip).** Ask whether this task is blocked by
  existing tasks that must complete first. Run `flow list tasks` (optionally
  filtered by `--project <slug>` when a project was chosen, and `--status
  backlog` or `--status in-progress`) to surface candidates. Use
  `AskUserQuestion` (header: "Blocked by?", `multiSelect: true`) with one
  option per relevant candidate task (label = slug, description = task
  name) plus a "Skip — no deps" option. Pass `--depends-on <slug>` once
  per selected task in the `flow add task` invocation. This is a
  *blocking dependency* (DAG): flow will gate execution order and the
  graph view on these edges. Skip if the user already named deps or if
  no plausible candidates exist.
- **Priority.** Use `AskUserQuestion` with "High", "Medium (Recommended)",
  "Low". Skip if the user already stated priority.
- **Agent (REQUIRED).** Use `AskUserQuestion` (header: "Agent", options:
  "Claude Code" / "Codex") to choose the session agent, then pass it as
  `--agent claude` / `--agent codex`. **You must include `--agent` in the
  `flow add task` invocation** — the binary rejects the command without it.
  Skip the question only if the user already named the agent in their
  request ("add a codex task for X"); never skip the flag. The agent can be
  changed later only while the task is still in backlog (via
  `flow update task <slug> --agent claude|codex`); once a session starts it
  is locked.
- **Assignee.** Use `AskUserQuestion` with "Me (self)" and "Someone else".
  Skip the question only if the user already named an assignee. Treat
  "Me (self)" as the default and omit `--assignee`; pass `--assignee <name>`
  only when the user chooses someone else and provides a name.
- **Due date.** Use `AskUserQuestion` with "No due date", "Today",
  "This week", and "Pick a date". Skip the question only if the user already
  stated a due date. Treat "No due date" as the default and omit `--due`;
  pass `--due <date>` only when the user chooses a date.
- **`--mkdir`** if the `work_dir` doesn't exist yet. Use `AskUserQuestion`
  with "Yes, create it" / "No, I'll fix the path".

**Draft the brief. Show it to the user.** Then use `AskUserQuestion`
(header: "Brief", options: "Save it" / "Revise") to confirm. Do not
run `flow add task` until the user picks "Save it". If they pick
"Revise", ask what to change, update the draft, and re-confirm.

**After `flow add task` succeeds**, it will print the task slug and
the absolute path to a stub `brief.md`. The flow is **Read once, then
Edit/Write**: Claude's `Write` and `Edit` tools both require a prior
`Read` of any existing file before mutating it (this is the harness's
guard against accidental overwrites). For brand-new tasks the stub
contents are predictable, so a single `Read` followed by either:
- `Edit` with `replace_all: true` (replaces the whole stub body), or
- `Write` (overwrites in full)

…is the right pattern. Use this literal template:

```markdown
# <name>

## What
<one sentence>

## Why
<short paragraph>

## Where
work_dir: <path>

## Done when
- <criterion 1>
- <criterion 2>
- <criterion 3>

## Out of scope
- <non-goal 1>

## Open questions
- <question 1>

---
*Before you start on this task, read CLAUDE.md in the work_dir.*
```

**Tag step — always ask, easy to skip.** Right after the brief
saves — before offering "Open now?" — you MUST surface a single
tag question. The user gets to pick "Skip" with one click; that
makes the step optional from the *user's* side, not yours. Do NOT
pre-skip this step on the user's behalf.

1. Run `flow list tags` to discover the user's existing vocabulary.
2. Use `AskUserQuestion` (header: "Tags?", `multiSelect: true`):
   - If existing tags came back: include the top 3 (most-used) as
     options labelled `#<tag>` with description "N tasks already
     have this tag". Add an option "New tag(s)" — when the user
     picks this via Other, they type comma-separated values. Add a
     final option "Skip — no tags" so the step is one click to
     bypass.
   - If no tags exist yet: just ask "Tag this task? (optional)"
     with options "Yes, set tags" / "Skip — no tags". On "Yes",
     prompt the user (prose is fine, there's nothing to suggest)
     for comma-separated values.
3. If the user picks any combination of existing tags and/or
   typed values, run `flow update task <slug> --tag <t1> --tag <t2> ...`.
4. If the user picks "Skip", do nothing — and move on to the
   "Open now?" question without dwelling.

The ONLY case where you may legitimately skip surfacing the question
is when the user has explicitly said something like "no more
questions, just save it" or "just save it" earlier in the same
turn. Otherwise, ask. Don't second-guess; preserve their right to
skip by giving them the click, not by pre-deciding for them.

Finally, offer how to proceed with the new task. The shape of the
question depends on whether THIS Claude/Codex session is already bound to
another flow task. Probe with `flow show task` (no arg). If it
errors with `not bound to a task`, the current session is unbound
(dispatch); otherwise it already belongs to the task it resolved.

**Unbound session — three options.** Use `AskUserQuestion`
(header: "Open now?"):

- **Yes, in a new tab** — proceed to the §4.4 recipe
  (`flow do <slug>`, which spawns a fresh tab and flips the task
  to in-progress). Pick when the work hasn't started yet.
- **Continue here (bind this session)** — keep working in this
  conversation; bind it to the new task so future
  `flow do <slug>` resumes here. Pick when the work motivating
  the task has already begun in this session — which is the
  common case when intake was triggered by §4.14 from the
  SessionStart hook intercept. Run **`flow do --here <slug>`**
  immediately (the binary reads the current agent session id, binds,
  and flips status to in-progress in one shot).
- **No, keep in backlog** — save and stop. Pick for future work
  the user won't touch today.

**Status follow-through — the task should not be left in backlog
after either Yes-path picks.** `flow do` and `flow do --here` both
flip the task to in-progress as part of the bind. If the work is
purely retrospective (the task exists to *record* something
already complete in this session — e.g. "track the script I just
wrote", "register what we just shipped"), the right next step
after Continue-here is to immediately offer §4.7 closure
(`AskUserQuestion`: "Mark done now?") so the task moves
backlog → in-progress → done in one flow. The task should never
sit in in-progress for retrospective records — its purpose is
already fulfilled the moment it's created and bound.

**Bound session — two options ONLY.** Use `AskUserQuestion`
(header: "Open now?", options: "Yes, open it" / "No, keep in
backlog"). Do **not** offer "Continue here" / "Bind this session"
/ "Rebind" / any variant. Reasons:

- A session_id can belong to at most one task (partial unique
  index). Binding this session to a second task would either
  orphan the prior task's transcript or violate the index.
- `flow do --here` REJECTS this case at the binary level even
  with `--force` — there is no escape hatch. Offering an option
  the binary will refuse is bad UX.
- The user's intent is almost always "open the new task in a
  separate tab" (Yes-new-tab). If they actually want to switch
  this session's task ownership, that's a different workflow
  (release the prior binding first) and must be asked
  explicitly, not slipped in as a third option here.

On "Yes", proceed to §4.4. On "No", stop.

> **Different-tab hint.** "Continue here" only ever applies in
> dispatch sessions and only ever attaches the *current* session.
> If the user is creating a task to track work that happened in
> a *different* Claude/Codex session they have open elsewhere, they
> need to switch to that other tab and run `flow do --here
> <slug>` there.

### 4.3 Add a project

**Triggers:** "add a project", "new project", "track this initiative".

Similar to §5.2 but shorter. Sections: **What / Why / Where / Scope**.
No "done when" (projects are ongoing containers, not completable units).
Confirm the `work_dir`. Draft. Show. Wait for "save it". Run `flow add
project`, then update the stub `brief.md` with the drafted content
(Read once, then Edit/Write — same pattern as §5.2).

Do not offer `flow do` on the project itself — you `do` tasks, not
projects.

**MANDATORY follow-up: create at least one task under the project.**
A project with zero tasks is a dead container — the user will forget
why they made it. Immediately after `flow add project` succeeds:

1. Say: "Project created. A project needs at least one task to be
   useful — what's the first concrete thing you want to do under
   <project-slug>?" (Use the project's actual slug.)
2. When the user answers, enter the task-intake workflow (§5.2)
   with `--project <slug>` pre-filled. Interview for What / Why /
   Where / Done when / Out of scope / Open questions as usual.
3. If the user says "I don't know yet" or "just create the project
   for now", DO NOT create a placeholder task and DO NOT silently
   drop it. Instead, explicitly tell them: "OK, no task for now —
   just tell me when you're ready to add one and I'll set it up."
   Do not surface the underlying `flow` commands.
4. If the user describes several tasks at once, create them all via
   sequential §5.2 interviews. Don't try to batch-extract; one
   interview per task.
5. Only after the first task exists (or the user has explicitly
   declined), use `AskUserQuestion` (header: "Open it now?", options:
   "Yes, open it" / "No, keep in backlog") to offer
   `flow do <first-task>`. If "Yes", proceed to §4.4. If "No", stop.

The rule is about pushing the user one step further than
`flow add project` — project creation is not a complete action on
its own, it's the start of a two-or-more-step workflow.

### 4.4 Start / resume work on a task

**Triggers — any of these means "run `flow do <ref>`":**
- "resume X" / "pick up X" / "continue X" / "open X"
- "let me work on X" / "lets work on X" / "let's work on X"
- "lets do X" / "let's do X" / "do X" / "do the X"
- "start X" / "start on X" / "begin X" / "get going on X"
- A bare "`flow do X`" typed as command-like input

**Recipe:**

1. **Ask the user which session mode they want** before running anything.
   Use the `AskUserQuestion` tool so the user can click to choose:

   ```
   AskUserQuestion({
     questions: [{
       question: "Which session mode for <task-slug>?",
       header: "Session mode",
       options: [
         { label: "Regular",          description: "Normal agent session with tool-approval prompts (safer)" },
         { label: "Skip permissions", description: "Pass --dangerously-skip-permissions (faster, no prompts)" },
         { label: "Autonomous",       description: "Headless background run — no tab, no human; Claude or Codex works end-to-end and calls flow done itself (--auto)" }
       ],
       multiSelect: false
     }]
   })
   ```

   Trigger phrases for Autonomous: "run X headlessly", "background run", "autonomous", "unattended", "fire and forget".
   Autonomous mode uses the task's provider and resolved model. Claude runs
   headlessly through `claude -p`; Codex runs non-interactively through
   `codex exec`. Because no human is watching, **match the model to the task
   when you create it** — pin `--model` (strong for hard work, small for
   trivial) or set `--priority high` so the resolver upshifts. See
   **Picking a model** in §2.

   If the user already specified a mode in their request (e.g. "do X
   with skip permissions", "do X normally", "auto mode"), use that —
   don't re-ask. If they explicitly ask for Codex, append `--agent codex`;
   otherwise default to the task's stored provider or Claude.

   **Special case: Autonomous mode (`--auto`)**
   ```
   flow do <slug> --auto
   flow do <slug> --auto --with "<one-off instruction>"
   flow do <slug> --auto --with-file <path>
   ```
   - Returns immediately; prints the supervisor PID and log path.
   - Supervisor runs `flow __auto-exec <slug>` detached using the task's provider and resolved model.
   - Claude runs headlessly with `--dangerously-skip-permissions`.
   - Codex runs with `codex exec`, prompt on stdin, and Flow's provider-neutral permission modes. The default `auto` mode uses no approval prompts with a `workspace-write sandbox`; explicit `bypass` disables approvals and sandboxing.
   - Run lifecycle: `auto_run_status` ∈ {running, completed, dead}. Check progress with `flow show task <slug>`.
   - Block on completion with `flow wait <slug> --until done`.
   - Constraints: no `--here`; in-flight guard (use `--force` to override).
   - After it returns, report the log path. Do NOT tail the log — leave that to the user.
2. Run: `flow do <user's ref>`. Pass the slug the user gave as one
   positional argument. Resolution is exact slug match. Append
   `--dangerously-skip-permissions` if the user chose skip-permissions;
   for Codex this maps to Codex's
   `--dangerously-bypass-approvals-and-sandbox`. Stored task
   `permission_mode` is provider-neutral. When a task/session does not
   explicitly choose a mode, flow uses `auto`: Codex gets no approval
   prompts while keeping the workspace-write sandbox. Explicit `default`
   means prompt on request with sandboxing for Codex, and `bypass`
   disables both Codex approvals and sandboxing.
   Append `--agent codex` only when the user requested Codex or when
   creating/running a task already stored with `session_provider=codex`.
3. If the command errors with "no task matching", ask the user to clarify
   or offer `flow add task` instead.
4. Pass `--fresh` ONLY if the user explicitly asked for a fresh session
   (e.g. "start over", "fresh session", "--fresh"). Never on your own.

**After `flow do` succeeds** it has already spawned a terminal tab.
Your job is done. Report "opened tab: <title>"
and stop. Do NOT:

- Run diagnostic commands like `pgrep`, `ls ~/.claude/projects/...`,
  or `osascript` to try to verify the tab opened.
- Try to spawn a terminal tab yourself with osascript or zellij. `flow do` already
  did this.
- Re-run `flow do` unless the user explicitly asked for a retry.
- Try to peek into the new session's activity. It's a separate
  conversation; you have no access to it.

If `flow do` itself errored (rc != 0), relay the error and stop. Do
not attempt workarounds; the user will decide what to do next.

#### Special case: live-session guard

`flow do` refuses to spawn when the task's `session_id` is already
running in another Claude process — typically because the user has the
task's tab open elsewhere and forgot. The error names the running
session ID and points at `--force`. When you see it:

1. Tell the user, in plain language, that the task already has a
   running session in another tab. Suggest they switch to that tab.
2. Offer via AskUserQuestion (header: "Open another?", options:
   "Switch to the existing tab" / "Open another anyway") whether to
   bypass the guard. On "Open another anyway", retry with `--force`.

Don't auto-retry with `--force`. The guard exists because two live
sessions on the same task fork the conversation and cause confusion;
bypassing it should be an explicit choice.

#### Special case: macOS Accessibility error from the Terminal.app backend

When `flow do` runs from a stock Terminal.app shell and macOS hasn't
granted Accessibility, the binary returns a multi-line error that
explicitly names "Terminal" as the app needing the grant and includes
a `open "x-apple.systempreferences:..."` URL to jump straight to the
right Settings pane. When you see that error:

1. **Trust the error verbatim.** It says "Terminal" because macOS
   attributes Accessibility to the responsible parent app, which is
   Terminal.app — NOT Claude Code, NOT the flow binary. Do not advise
   the user to toggle "Claude" or "flow"; that wastes their time.
2. **Open the Accessibility pane for them**: run
   `open "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"`.
3. **Tell the user, in plain language**, what to do next: enable the
   toggle for "Terminal" in the list (or click + and add Terminal.app
   from `/System/Applications/Utilities/` if it isn't shown).
4. **Wait for the user to confirm** they've granted it. Don't poll
   or retry on a timer.
5. When they confirm, retry the original `flow do` invocation with
   the same flags they originally chose (session mode, `--fresh`,
   etc.).

Macros for this: do not invent more candidate apps to toggle, do not
suggest the user reinstall flow, do not attempt to grant Accessibility
yourself. macOS guards Accessibility deliberately — there is no CLI to
self-grant it, and Claude cannot bypass that.

### 4.4a Which model a session launches with

Every task launches its Claude/Codex session with a specific model. You do
**not** need to ask the user which model during intake — model selection is
optional and resolved automatically at launch. Surface it only when the user
brings it up ("run this on opus", "use a cheaper model", "why did this open on
haiku?").

**How the model is resolved at `flow do` / `flow run playbook` time:**

1. **Explicit per-task model wins.** If the task carries a model (set via
   `flow add task --model <m>` or `flow update task --model <m>` while in
   backlog), flow passes exactly that to the CLI and stops. The value can be a
   tier alias the provider resolves to its latest (Claude: `opus` / `sonnet` /
   `haiku`) or any raw model id (`claude-opus-4-8`, a Codex model id, etc.) —
   flow passes it through verbatim.
2. **Otherwise flow picks a tier.** The baseline is a **default tier**
   (`FLOW_MODEL_TIER`, default **medium**) mapped per provider:

   | Tier | Claude | Codex |
   |------|--------|-------|
   | large | `opus` | `gpt-5.5` |
   | medium (default) | `sonnet` | `gpt-5.4` |
   | small | `haiku` | `gpt-5.4-mini` |

   The default is deliberately **not** the biggest model — most tasks don't
   need it.
3. **Auto-downshift.** When `FLOW_MODEL_AUTODOWNSHIFT` is on (the default) and
   the task's brief is **descriptive enough**, flow downshifts one rung
   (large→medium, medium→small) — a well-specified brief leaves little for the
   model to figure out, so a smaller/cheaper model suffices. "Descriptive
   enough" means: no `*Deferred*` sections, a filled-out brief (≥80 words), and
   ≥2 concrete `Done when` bullets. Set `FLOW_MODEL_AUTODOWNSHIFT=off` to
   disable, or pin an explicit `--model` to override per task.

Auto-downshift runs **only at bootstrap**, never on resume — a live session
keeps the model it started with (no mid-session switching). The model is
**locked once a session starts** (like the agent): change it only while the
task is in backlog.

`flow show task` surfaces a `model:` line — either `<m>  [explicit]` or a
preview of the auto-resolution (`<m>  [auto: <tier> tier]`, noting when a
descriptive brief downshifted it) — so the user can see what a launch will use.

### 4.5 Save a progress note

**Triggers:** "save a note", "log progress", "write an update", "note
that…", "record that I…", "document that I just…".

**Recipe:**

1. Compose a filename: `YYYY-MM-DD-<kebab-short-title>.md`. The kebab
   title is 3–5 words summarizing the note. Use today's date.
2. Compose the note content. **Under 10 lines.** Exactly two paragraphs
   plus an optional blockers line:
   - Paragraph 1: what got done. Specific. No hedging.
   - Paragraph 2: what's next or what the user is thinking about next.
   - Optional blockers: "Blocked on: <X>" if applicable.
3. **Show the filename and the content to the user.** Then use
   `AskUserQuestion` (header: "Save note?", options: "Save it" /
   "Revise") to confirm. Do not write silently.
4. Determine the entity:
   - For a **regular task**, notes go under
     `~/.flow/tasks/<slug>/updates/`. Slug from `flow show task` (no
     arg, reverse-lookup) or asked.
   - For a **playbook run**, notes ALSO go under
     `~/.flow/tasks/<run-slug>/updates/` (runs are tasks).
   - For a **playbook definition**, notes go under
     `~/.flow/playbooks/<slug>/updates/` for cross-invocation observations
     ("noticed flaky output when X", "next iteration should consolidate
     steps 2 and 3"). Use this when capturing things that should inform
     the playbook itself, not a single run.
5. Use the `Write` tool to create
   `~/.flow/tasks/<slug>/updates/<filename>.md` with the confirmed
   content. If the user is noting project-level progress, use
   `~/.flow/projects/<slug>/updates/` instead.
6. Confirm to the user: "saved: <absolute path>".

Do NOT run any `flow` command to write the note itself — updates are just files.

**Then refresh the brief's Current state** when the situation actually
changed (scope shifted, a blocker appeared or cleared, a milestone landed):

```bash
flow update task <slug> --brief-status "Blocked on deploy-role perms; selective release retried, awaiting Omendra."
```

This overwrites the brief's machine-maintained Current state block (a terse
1–3 line snapshot — NOT a copy of the note). Keep it to a few lines; if it
needs more, that belongs in the `updates/` note, not the brief. Always
refresh it before going idle, when handing the task off, and on `flow done`,
so the brief never goes stale. Skip it for notes that don't change the
overall state. (Runs are tasks — same command works for a run slug.)

### 4.8 Archive / soft-delete / cleanup

**Triggers:** "archive X", "clean up", "clean up my done tasks", "hide
finished work", "delete X", "remove X", "trash X", "restore X".

**Recipe:**

- **Archive vs delete choice.** If the user says archive/hide/clean up
  done work, archive. If the user says delete/remove/trash, use
  Soft-delete. If intent is ambiguous, ask via `AskUserQuestion`
  whether they want "Archive it" or "Move to trash".
- Single task/project: confirm via `AskUserQuestion` (header:
  "Archive?", options: "Yes, archive `<slug>`" / "No, keep it"),
  then on "Yes" run `flow archive <ref>`.
- Bulk "archive everything done": run `flow list tasks --status done`.
  Show the list to the user. Then, unless the user already said
  "archive all done" explicitly, use `AskUserQuestion` (header:
  "Archive all?", options: "Yes, archive all listed" / "Pick one by
  one" / "Cancel"). On "Yes", iterate and archive them all, printing
  each action. On "Pick one by one", run a single-task `AskUserQuestion`
  for each. On "Cancel", stop.
- If the user regrets it: `flow unarchive <ref>`.

Archive never deletes files on disk — brief.md and updates/ remain. Make
sure the user knows this so they don't worry about losing notes.

**Soft-delete:**
- Single task/project/playbook: confirm via `AskUserQuestion` (header:
  "Delete?", options: "Yes, move to trash" / "No, keep it"), then on
  "Yes" run `flow delete <ref>`. This sets `deleted_at`; normal lists
  and the UI hide it, but the row and markdown files remain.
- Use `task/<slug>`, `project/<slug>`, or `playbook/<slug>` when the
  same slug may exist in multiple entity types.
- To inspect deleted work, use `flow list tasks --deleted`,
  `flow list projects --deleted`, or `flow list playbooks --deleted`.
  Use `--include-deleted` when you need active and deleted rows together.
- To undo it, run `flow restore <ref>`. Restore clears `deleted_at` only;
  it does not unarchive archived work. If the user asks to restore the
  wrong thing, list `--deleted` rows first and ask them to pick.

**Playbooks:**
- `flow archive <playbook-slug>` hides the playbook from
  `flow list playbooks` but does not affect past runs (they're independent
  task rows). Past runs can be archived independently with
  `flow archive <run-slug>`.
- "Bulk clean up done runs" pattern: `flow list runs --status done`,
  then archive each.

### 4.9 Weekly review

**Triggers:** "weekly review", "week in review", "what did I ship this
week", "friday review".

**Recipe:**

1. `flow list tasks --status done --since monday` — what shipped.
2. `flow list tasks --status in-progress` — what's still in flight. For
   each one, read the newest file in its `updates/` directory (via the
   `Read` tool) to summarize the latest state in 1 line.
3. Call out any `⚠` stale tasks and any `waiting_on` tasks explicitly.
4. `flow list tasks --status backlog --priority high` — what's queued.
5. `flow workdir list` — surface any workdir that hasn't been used in
   30+ days; mention these as "consider archiving" candidates.
6. `flow list runs --since monday` — group by playbook slug, count runs,
   pull each playbook's most-recent run timestamp.

Produce a digest in this exact shape:

```
## Shipped this week
- <task> — <one-line outcome>

## In flight
- <task> — <latest-update summary>  [⚠ stale if applicable]

## Stalled / waiting
- <task> — waiting on: <who/what>

## Next up
- <task> — <why it's high priority>

## Workdir hygiene
- <path> — untouched since <date>

## Playbook activity
- <playbook-slug> — N runs this week, most recent <date>
```

Do not solve anything during a weekly review — it's a reporting
workflow, not a planning workflow.

### 4.9a Search flow memory

**When to use:** Use `flow search` when you need to locate context
across flow's markdown-backed memory and you do not already know the
exact task, project, playbook, KB file, or agent memory file to open.
This is the fast path for questions like "where did we decide X?",
"what did we do for customer/product Y?", "have we seen this error
before?", or "what memory exists about this tool/person/project?".

`flow search "<query>"` searches briefs, updates, and memories by
default. "Memories" means flow KB files under `~/.flow/kb/`, Codex
memory/instruction markdown, and Claude auto-memory markdown. Session
transcripts are deliberately opt-in because they can be much larger:
use `--in transcripts` for transcript-only search or `--in all` when
you explicitly need briefs, updates, memories, and transcripts together.

Common patterns:

```
flow search "Socket Mode"
flow search "permission mode" --in memories
flow search "unsupported updatedInput" --in all --limit 10
flow search "review thread" --format json
```

Search is a locator, not an authority. After finding a hit, open the
source the normal way before making claims:

- For task/project/playbook hits, run `flow show ...` and read the
  surfaced brief/update files.
- For transcript hits, run `flow transcript <task-slug>` if you need
  the surrounding conversation.
- For memory hits, read only the specific `source_path` or KB file that
  the result points to; do not bulk-load every KB or memory file.

This does **not** replace the execution-session bootstrap contract in
§9. Still read the current task brief, updates, parent project context,
and repo conventions first. Use search after bootstrap when the work
requires cross-task or cross-memory context.

### 4.9b Respond to Attention confirmed handoffs

**When to use:** If this task's inbox contains a "Confirmed handoff request from the attention router" with a `Correlation ID`, the steerer is asking this task to verify whether the source thread belongs here before the card is marked handled.

**Recipe:**

1. Read the handoff context block, then compare it against this task's brief,
   updates, and current transcript context. Do not accept just because the
   steerer guessed this task; the point of confirmed handoff is that the owning
   task has better context than the steerer.
2. If it belongs here, run:
   ```
   flow attention handoff accept <correlation-id> --reason "<why this belongs here>"
   ```
   Accepting marks the Attention card acted and links it to this task. The
   context block already landed in this inbox, so continue from it normally.
3. If it does not belong here, run:
   ```
   flow attention handoff decline <correlation-id> --reason "<why this should route elsewhere>"
   ```
   Declining keeps the Attention card open so the steerer/operator can escalate,
   re-triage, make a new task, or choose another candidate. If you know the
   better owner, name it in the reason.
4. If the context is insufficient, decline with that reason instead of staying
   silent. Pending handoffs time out explicitly and do not mark the card acted.

**Anti-patterns:**

- Do not run ordinary `flow tell` as a substitute for accepting; it bypasses the
  correlation id and leaves the feed card unresolved.
- Do not accept without a reason. The reason is the audit trail the operator
  sees when reviewing why the route was confirmed.
- Do not keep a handoff pending while you investigate unrelated work; accept or
  decline once you have enough task-local evidence.

### 4.15 Upgrade flow itself

**Triggers:** "update flow", "upgrade flow", "is there a new version
of flow", "new flow version", "what version am I on", "what's the
latest flow", "flow is stale", a bare `flow --version` typed as
command-like input. Also fires when the SessionStart hook reports
`flow-version-stale: <new-version>` in its additionalContext (the
hook does an at-most-once-per-day cached check against GitHub
releases) — when you see that signal, proactively offer the upgrade
via `AskUserQuestion`.

**Recipe:**

1. Run `flow --version` to capture the currently-installed version.
2. The canonical install/upgrade procedure lives in the README at
   `https://github.com/Facets-cloud/flow`. Use the `Read` tool /
   `WebFetch` to read the **Install** and **Upgrade** sections —
   they're the source of truth for the binary download URL,
   architecture flag (`arm64` for Apple Silicon, `amd64` for Intel),
   and the `xattr -d com.apple.quarantine` workaround for unsigned
   binaries. Do not invent download URLs; read them from the README.
3. Download the new binary per the README and replace the existing
   one (typically at `/usr/local/bin/flow`; confirm with
   `which flow` if unsure).
4. Run `flow skill update` to refresh the embedded skill on disk and
   re-wire both the SessionStart and UserPromptSubmit hooks in
   `~/.claude/settings.json`. (The auto-upgrade path runs the same
   refresh on the next `flow` invocation, but explicit is better and
   surfaces any errors immediately.)
5. Run `flow --version` again and confirm the version changed. If it
   did not change, the binary on `$PATH` is still the old one —
   check `which flow` against the path you wrote to.

**Anti-patterns:**

- **Do not invent download URLs.** Read them from the README at
  `https://github.com/Facets-cloud/flow`. Releases are at
  `/releases/latest/download/`; the README has the exact form.
- **Do not run `flow skill install` on an existing install** — it
  errors. Use `flow skill update` for the refresh path.
- **Do not skip the `xattr -d com.apple.quarantine`** step on a
  freshly-downloaded binary — Gatekeeper will refuse to run it
  otherwise.

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

### 4.16 Bind an in-flight Claude/Codex session to a task

**Triggers:** "bind this session to <task>", "track this session
under <task>", "attach this conversation to <task>", "this session is
for <task>". Also fires when the user manually creates a flow task
while already deep in an ad-hoc Claude or Codex session and wants future
`flow do <slug>` to resume *this* conversation rather than start a
new one. The §4.2 "Continue here" option is the most common entry
point.

**Recipe:** if the target task already exists (or you just created
it via §4.2), run:

```
flow do --here <slug>
```

`flow do --here` reads the current session id from the active agent
host (`$CLAUDE_CODE_SESSION_ID` for Claude Code, `$CODEX_THREAD_ID`
for Codex), validates it, stores the matching `session_provider`,
and writes the id to `tasks.session_id`. Side effects: status flips
backlog → in-progress (the session-id invariant requires it). No
terminal spawn happens; the binding is the only mutation.

**Safety properties enforced by the binary** (you don't have to
police these):

- Refuses if no supported current-session env var is set, or if the
  exposed id is malformed. Claude ids must be v4 UUIDs; Codex thread
  ids must be UUID-shaped.
- Refuses if **THIS session** is already bound to a different task.
  `--force` does NOT override this — session_id uniqueness is
  structural. The user must release the prior binding first or
  open the target in a new tab via `flow do <slug>`.
- Refuses if **the target task** already has a different
  session_id bound. `--force` overrides this case (and only this
  case), but the user has been told it orphans the target's
  prior session.
- No-op (idempotent) if the target is already bound to this same
  session.
- Refuses if the target is `done`. Reopen via
  `flow update task <slug> --status in-progress` first; the
  prior session_id is preserved across done, so `--here` becomes
  unnecessary after reopen.

**Anti-patterns:**

- **Do not invent or guess the session id.** The host env var is the
  only authoritative source.
- **Do not bind without confirming the task slug.** If multiple
  tasks could plausibly own this conversation, AskUserQuestion to
  pick.
- **Do not run `--here` from a different tab to attach a session
  in another tab.** The env var is per-process; `--here` always
  attaches the *current* session. To attach a session in another
  tab, switch to that tab and run `flow do --here` there.

## 6. The `work_dir` question — rules

When you're about to ask the user "where does this task live?", run
these steps BEFORE asking, so the question is informed:

1. **Run `flow workdir list`.** Fuzzy-match the task name against
   registered nicknames and paths. If you get an obvious match (e.g.
   task "Add OAuth to budgeting-app" and a registered workdir named
   `budgeting-app`), propose that path via `AskUserQuestion` (header:
   "Use this path?", options: "Yes, use `<path>`" / "Pick a different
   path"). On "Pick a different path", continue to step 2.
2. **If no local match, check GitHub via `gh`.** Run `gh repo list
   --limit 50 --json name,owner,description`. If any repo name or
   description plausibly matches the task, present the top 3 via
   `AskUserQuestion` (header: "Which repo?") with one option per
   candidate (label = `<repo-name>`, description = repo description)
   plus a "None of these — use a path instead" option. If the user
   picks a repo, offer (via `AskUserQuestion`, header: "Clone it?",
   options: "Yes, clone to `~/code/<name>`" / "No, I'll handle it")
   to run `gh repo clone <owner>/<repo> ~/code/<name>` and, after
   clone, run `flow workdir add ~/code/<name>` so next time it's a
   local match.
3. **If `gh` isn't authenticated** (command errors with an auth
   message), fall back gracefully via `AskUserQuestion` (header:
   "GitHub unreachable", options: "Give me a path" / "Make it
   floating"). On "Give me a path", prompt the user for an absolute
   path (this single text input is fine — there are no enumerable
   options). On "Make it floating", skip work_dir entirely.
4. **If the user wants a floating task** (no repo), skip the question
   entirely and let `flow add task` auto-create
   `~/.flow/tasks/<slug>/workspace/`.
5. **Never guess a path.** Don't invent `~/code/foo` because the task
   name sounds like "foo". Always confirm via `AskUserQuestion`.
6. **If the path doesn't exist**, use `AskUserQuestion` (header:
   "Create dir?", options: "Yes, create it" / "No, fix the path")
   to ask whether to pass `--mkdir`. On "Yes", append `--mkdir` to
   the `flow add task` invocation. On "No", loop back to ask for a
   corrected path.

## 7. The task brief format

Use this as a literal template when writing `brief.md` files. Section
headings are fixed; content is whatever came out of the interview.

```markdown
# <task name, verbatim>

## What
<one sentence from the interview, no editorializing>

## Why
<short paragraph capturing the user's reason>

## Where
work_dir: <absolute path>

## Done when
- <bullet 1 from acceptance criteria>
- <bullet 2>
- <bullet 3>

## Out of scope
- <non-goal 1>

## Open questions
- <question 1>
- <question 2>

---
*Before you start on this task, read CLAUDE.md in the work_dir and any
nested CLAUDE.md files in the subtree you plan to modify. Then read
every file under `updates/` (if any exist) to catch up on prior
progress.*
```

**Thin task brief (intake-minimal):**

```markdown
# <name>

## What
<one sentence from intake>

## Why
*Deferred — fill in at task start.*

## Where
work_dir: <path>

## Done when
*Deferred — fill in at task start.*

## Out of scope
*Deferred*

## Open questions
*Deferred*

---
*This brief is thin. Before you start substantive work, the bootstrap
session will prompt you to fill in the deferred sections.*
```

A section is "deferred" if its body is the literal string
`*Deferred — fill in at task start.*` or `*Deferred*`. The bootstrap
session detects this and offers the user a deferred-section prompt
(§9).

If a section has no content, leave the heading with an italic "none"
underneath. Don't omit headings — the parallel structure makes the
briefs scannable.

**Cross-referencing other tasks — use `[[task-slug]]`, never prose.**
When a brief or update points at another task — a subtask parent, a
blocking dependency, a related effort — write that task's **exact slug**
in double brackets. Note: `[[slug]]` is a *prose backlink* indexed by
flow for the "linked from" graph; it does NOT create a blocking
dependency or hierarchy edge. For machine-trackable edges that gate
execution order or appear in the graph view, use `--depends-on` (blocking)
or `--subtask-of` / `flow spawn --parent` (hierarchy). Both mechanisms
complement each other: set the DB edge for correctness, use `[[slug]]`
in the brief text for human readability. flow indexes `[[slug]]` references
into a backlink graph: `flow show <task>` lists them under `linked from:`,
and Mission Control renders them as clickable in-app links. A brief that
says "the parent task's brief" or "see the auto-runs task" in prose creates
**no** link and breaks the graph. Rules:
- **Bare slug only.** `[[task-slug]]` — no `[[slug|label]]` pipe, no
  paths, no spaces inside the brackets. A target containing `/`, `\`, or
  `|` is ignored, and a slug with no matching task registers nothing
  (so use the real slug from `flow list tasks`, not a guess).
- **Tasks only.** Projects and playbooks are not backlink targets;
  don't bracket their slugs expecting a link.
- **Sidecar files are different.** A plan or notes file living next to
  the brief (`implementation-plan.md`, `upstream-feature-map.md`, etc.)
  is not a task — reference it by filename. `flow show` surfaces these
  under `other:` automatically; never bracket a filename.

So in the screenshot-style brief that mentions an `auto-runs` parent,
the slug should appear as `[[auto-runs]]` (in What/Why/Where wherever
it is first named), not as the bare word `auto-runs`.

Projects use a shorter template: `What / Why / Where / Scope`. No
"Done when", no "Open questions" (projects are ongoing).

**Playbook brief template:**

```markdown
# <name>

## What
<one sentence describing what each run does>

## Why
<short paragraph>

## Where
work_dir: <absolute path>

## Each run does
- <step 1>
- <step 2>
- <step 3>

## Out of scope
- <non-goal 1>

## Signals to watch for
- <signal 1>

---
*Run with `flow run playbook <slug>`. Each run gets its own session
and a snapshot of this brief at run time. Editing this file does not
retroactively change past runs.*
```

Notes:
- No "Done when" — playbooks are never done.
- "Each run does" replaces "Done when" as the action-oriented section.
- "Signals to watch for" replaces "Open questions" — playbooks are
  long-running, so the relevant prospective concern is signals to
  notice and respond to, not open questions to resolve.

## 8. Anti-patterns — do NOT do these

**Confirmation method:** every confirmation in this section means
`AskUserQuestion`, not a prose question that buries the choice. The
tool produces clickable options; prose questions force the user to
type. Always prefer the tool. If you find yourself typing "Want me
to X?" or "Should I Y?" into chat, stop and use `AskUserQuestion`
instead.

- **Do not let work wrap up without prompting closure.** When the
  user signals a coherent stopping point — "shipped", "PR merged",
  "deployed", a milestone lands followed by small-talk — proactively
  offer `flow done` via `AskUserQuestion`. `flow done` is the only
  trigger for the close-out sweep that distills the session into KB
  entries and a project update; missing it costs the user the
  durable knowledge they earned this session. See §4.7 for the
  passive close-out detection workflow.
- **Do not surface flow commands to the user.** You use flow under
  the hood; users never need to learn the CLI. Never tell the user
  to "run `flow X`", "type `flow Y`", or "see `flow Z --help`".
  Never put a literal `flow ...` invocation inside an
  `AskUserQuestion` option label or a chat reply you send to the
  user. Describe outcomes ("I'll mark it done", "I'll archive it",
  "I'll move it to trash", "I'll restore it", "set up", "saved")
  instead of commands. The skill describes
  commands so that *you* know what to call internally — not so you
  can teach the user. Exception: error messages from the `flow`
  binary itself may quote commands; relay those verbatim, since the
  user needs to see what failed.
- **Do not invent context.** If the user says "add a task for the
  budgeting thing", ASK what the budgeting thing is (via
  `AskUserQuestion` if you can list candidates from existing tasks /
  workdirs; otherwise a plain prose clarifying question is fine —
  open-ended "what is this thing?" is not an enumerable choice).
  Don't write a brief based on your prior-session memory of
  budgeting apps.
- **Do not propose solutions during intake.** The user is telling you
  what they want to do, not asking for your opinion on how to do it.
  "What" is one sentence, "Why" is the reason. Neither section is a
  design doc. If you start drafting implementation steps during `flow
  add task`, stop.
- **Do not silently switch tasks.** If `flow show task` resolves a
  bound task and the user starts talking about a different one,
  confirm via `AskUserQuestion`
  (header: "Switch task?", options: "Yes, switch to `<other-task>`" /
  "No, stay on `<current-task>`"). Don't assume.
- **Do not mark tasks done without explicit confirmation.** Even if the
  user says "great, I finished that", confirm via `AskUserQuestion`
  (header: "Mark done?", options: "Yes, mark it done" / "No, not
  yet") and wait for the click.
- **Do not hand-edit `session_id` or any other DB field.** Never edit
  `flow.db` directly, never instruct the user to. The only supported
  mutations are `flow` commands.
- **Do not retry a `flow` command that errored.** Read the error, relay
  it to the user, and ask. In particular, do not loop `flow do X` → see
  "multiple matches" → guess one → run again. Ask.
- **Do not bundle multiple saves into one `flow add task` call.** One
  task per interview. If the user mentions three things they want to
  track, run the interview three times (or ask to batch and then do it
  explicitly with user consent).
- **Do not skip the interview on "quick adds".** Even when the user
  says "just add a task for X, nothing fancy", ask at minimum: What?
  Why? Where? **Agent (claude|codex)?** — the agent is mandatory and
  `flow add task` refuses to run without `--agent`. You can compress the
  other sections to "TBD" if they
  push back, but `What/Why/Where` are non-negotiable.
- **Do not overwrite an existing `brief.md` without checking what's
  there.** `flow add task` writes a stub. You overwrite that stub
  (Read once, then Edit/Write). If the Read shows real content
  rather than the expected stub (e.g. the user edited between add
  and your call), merge thoughtfully and confirm with the user
  before writing.
- **Do not forget to offer progress notes.** After a long working
  session, the user will forget to log what they did. At natural
  breakpoints, proactively use `AskUserQuestion` (header:
  "Save note?", options: "Yes, save a note" / "No, skip it") to
  prompt — never a prose "want me to save a note?" question.
- **Do not silently continue scope-drifted work under the bootstrapped
  task.** When the work genuinely moves off the bound task (new repo,
  new product, new line of investigation sustained over multiple
  turns — see §5.11 for signals), surface the drift via
  `AskUserQuestion` and offer to branch into a new task. Letting
  unrelated work accumulate under the wrong task poisons that task's
  transcript and buries decisions the user will later want to find.
- **Do not *ad-hoc* auto-fire `flow run playbook`.** A bare manual run
  happens only on an explicit verb ("run", "trigger", "fire", "start") —
  mentioning a playbook by name in passing is not a trigger. (This is
  separate from a configured recurring **schedule**, which the user
  set up explicitly and the scheduler fires in `--auto` mode — see §4.13a.)
- **Do not edit a run-task's `brief.md` to change the playbook's
  behavior for future runs.** That brief is a frozen snapshot. To
  change behavior, edit the playbook's `brief.md` and start a new
  run.
- **Do not set or change a playbook schedule the user didn't ask for,
  and never hand-edit schedule columns in the DB.** Scheduling is opt-in;
  use the `flow add/update playbook --schedule` flags (§4.13a) so the
  next fire time is computed correctly.
