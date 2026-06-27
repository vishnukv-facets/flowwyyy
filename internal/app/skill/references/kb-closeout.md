### 4.6 Waiting on someone

**Triggers:** "I'm waiting on <X>", "blocked on <Y>", "stuck until <Z>",
"need <person> to respond", "pinged <X>".

**Recipe:** run `flow update task <current-task> --waiting "<who or what>"`. The
status stays `in-progress`; `waiting_on` is just a freeform note that
will show up in `flow list` and `flow show task` so the user remembers.

**Unblocking triggers:** "X came back", "got the answer", "unblocked",
"no longer waiting on X". Before mutating, confirm via
`AskUserQuestion` (header: "Clear waiting?", options:
"Yes, clear it" / "Wait, not yet"). On "Yes", run
`flow update task <task> --clear-waiting`. On "Wait, not yet", stop
and let the user clarify. (This matches the §8 "do not mark done
without confirmation" anti-pattern philosophy — clearing `waiting_on`
is a state mutation and deserves the same explicit click.)

Do not infer the task slug silently — use `flow show task` (no arg)
to discover the bound task, otherwise use `AskUserQuestion` listing
in-progress tasks as options to disambiguate which task this is for.

### 4.7 Mark done

**Triggers — explicit:** "mark X done", "finish X", "X is done",
"close out X", "wrap up X".

**Triggers — wrap-up signals (treat as candidate triggers, then
confirm via AskUserQuestion):** "shipped", "PR merged", "deployed",
"released", "wrapped up", "that's working", "bug fixed", "test
passes", "ready to ship", "all good now", "we're good", "that did
it".

**Why closing matters — read this before treating `flow done` as
bookkeeping.** `flow done` is not just a status flip. It runs a
headless Claude sweep over the task's transcript that distills
durable facts into the user's KB (`~/.flow/kb/`) and, when the task
has a project, writes a project update at
`~/.flow/projects/<slug>/updates/` summarizing what got done and
why. **If a task never closes, that distillation never happens.**
Learnings stay locked in the transcript and never reach central
tracking — which is precisely the value the user installed flow to
capture. Treat closure as the load-bearing moment of the workflow,
not a clean-up afterthought. Letting work wrap up without prompting
closure is a silent loss of durable knowledge.

**Recipe:**

1. Confirm via `AskUserQuestion` (header: "Mark done?", options:
   "Yes, mark it done" / "No, not yet") before mutating. Per §8,
   never mark done without explicit confirmation — even if the user
   says "great, I finished that".
2. If the user hasn't just saved a progress note, use
   `AskUserQuestion` (header: "Closing note?", options:
   "Yes, save a note first" / "No, just mark done") to offer.
   On "Yes", run the §4.5 recipe first, then continue.
3. Run `flow done <ref>`. The session_id stays on the task row so a
   future reopen can still resume the same conversation in a fresh
   tab. The close-out sweep runs after the status flip and `flow done`
   waits for it to return; only then does Flow close the task's
   `flow-<slug>` tmux session. Do not manually kill the agent or close
   the tab before `flow done` returns — when invoked from inside the
   task's own tab, Flow schedules a delayed tmux close so the command
   can print and exit cleanly. Relay any NUDGE block `flow done`
   prints back to the user verbatim.

**Recognizing natural close-out moments — passive workflow.**

This is a passive workflow that runs alongside §4.10 (KB scoop) and
§4.11 (scope-creep): you watch the conversation for signals that
substantive work is wrapping up even when the user hasn't said the
word "done". When a signal fires, proactively offer closure via
`AskUserQuestion` — don't wait for the user to remember.

**When to fire:**

- Wrap-up phrasing from the wrap-up trigger list above.
- A clear milestone just landed (PR merged, deploy succeeded, all
  tests green, last open question on the brief resolved) and the
  user has moved to small-talk or signaled satisfaction ("perfect",
  "that's it", "nice", "thanks").
- The user references a separate task in a way that implies a
  context switch ("now let me look at <other thing>") — offer to
  close the current one first if its work is at a coherent stopping
  point.

**When NOT to fire:**

- Mid-debugging or mid-implementation, even if a partial milestone
  was hit. Closure is for coherent stopping points, not every green
  test.
- The user explicitly said "more work coming" earlier this session —
  remember that signal and don't re-ask on the same thread.
- The very first turns of a session — the user just opened the task;
  let the work happen first.

**Recipe:**

1. Pause your current line of response.
2. Use `AskUserQuestion` (header: "Mark done?", options:
   "Yes — close it and run the close-out sweep" / "Not yet, more
   work coming"). The "Yes" option's description should name the
   sweep: "flow done distills KB entries and a project update from
   this transcript — closing now persists what we learned this
   session."
3. On "Yes", proceed to the §4.7 recipe above (closing-note offer,
   then `flow done`).
4. On "Not yet", accept the answer and don't re-ask on the same
   thread of work in the same session.

**Playbook-specific notes:**

- **Run-tasks** (kind=playbook_run) support `flow done <run-slug>` like
  any task. Their close-out sweep also runs and can capture
  playbook-specific learnings.
- Note: **playbook definitions are never "done" — they're archived.**
  When a playbook is no longer in use, run `flow archive <playbook-slug>`.
  There is no `flow done playbook` command.

### 4.10 Listening for knowledge-base facts (scoop mode)

This is a **passive** workflow — it runs alongside every other workflow
in §5, continuously, without the user asking for it.

**What flow's knowledge base is:**
Five markdown files under `~/.flow/kb/`, seeded by `flow init`:

| File | Holds |
|---|---|
| `user.md` | Durable facts about the user — role, preferences, working style, constraints, availability |
| `org.md` | Company, team, structure, people the user interacts with |
| `products.md` | What the org ships — product lines, modules, features, releases |
| `processes.md` | How the org works — tools, conventions, rituals, review rules |
| `business.md` | Customers, business model, revenue, deals, market positioning |

These files are surfaced in `flow show task` and `flow show project`
output under a `kb:` section, so execution sessions can read them.

**How to decide whether a user statement belongs in the KB:**

Listen for statements that are **durable facts**, not transient state.
Bucket them by these signals:

| User says something like… | Goes to |
|---|---|
| "I'm the / my role is / I prefer / I hate / I always / I never" | `user.md` |
| "our team / my manager is / we have N people / <name> is / reports to" | `org.md` |
| "our product / we ship / feature X / module Y / next release" | `products.md` |
| "we use X for / our process / every Friday / we deploy on / review rule" | `processes.md` |
| "our customers / <customer> asked / revenue / contract / margin / market" | `business.md` |

**The scoop rule: append without asking.** If you hear a durable fact,
use Read to load the matching file, check it's not already there, then
Write an appended entry. Never pause to ask "should I record this?".
Just do it, then announce quietly in one line:

> noted in kb/org.md: "<short paraphrase>"

**Entry format — copy this exactly:**
```
- YYYY-MM-DD — <short quote or paraphrase of what the user said>
```

One line per entry. Keep it terse. Quote the user's actual words where
possible. If the fact is a list (e.g. "our products are A, B, C"),
write one entry per item rather than cramming them into a single line.

**Guardrails (non-negotiable):**

1. **Only durable facts.** "I'm tired today" is not durable. "I prefer
   async communication" is. When in doubt, don't record.
2. **Deduplicate.** Read the file first. If the same fact (even
   paraphrased) is already there, don't append a duplicate.
3. **Never invent.** Only record what the user literally said or clearly
   implied. Do not embellish, extrapolate, or guess.
4. **Real-time scoop is append-only.** When scooping a fact live, never edit
   existing entries; if a fact changes, add a new dated entry noting the change.
   **Exception — close-out upgrade:** during the `flow done` close-out sweep,
   when the completed work *settles a provisional entry* (a plan/intention now
   executed, e.g. "X plans to do Y by Friday" once Y is done) or contradicts a
   stated fact, you MAY **supersede that entry in place** — rewrite the plan into
   the outcome, or remove it if now trivial — so the always-loaded KB stays
   current instead of carrying stale plans into every future brief. Be
   conservative: supersede ONLY what the work clearly settled; never touch
   entries you're unsure about, and never delete facts that are still true. Git
   history preserves the evolution.
5. **One bucket per fact.** If a fact plausibly fits two categories, pick
   the more specific one. Do not cross-post.
6. **Privacy.** KB files may contain personal or org-sensitive info. If
   the user initializes a git repo inside `~/.flow/`, remind them to add
   `kb/` to `.gitignore`.
7. **When helping across many tasks**, read the KB files once per session
   and re-read only if you wrote new entries. They're not load-bearing
   for every turn — but they're load-bearing for "who is this user,
   what is their context" decisions.

**When to read the KB — lazy-load only:**

The KB files are **not** loaded at session start. The SessionStart hook
and the §9 bootstrap contract both explicitly skip them. Read a kb file
only when the current question in front of you actually needs that
category of fact. Signals that it's time to Read one:

- The user mentions a person, product, tool, or customer name and you
  don't know who/what they are → read `org.md` / `products.md` /
  `business.md` as appropriate.
- You're composing a task brief / project brief / progress note and
  want to reflect the user's working style accurately → read `user.md`.
- The user asks "how do we usually do X?" or "what's our convention
  for Y?" → read `processes.md`.
- A brief or CLAUDE.md uses terminology you don't recognize (e.g.
  an internal codename, a product term, a legacy component name) →
  read the relevant kb file for definitions.
- You're generating cross-cutting advice ("how should I approach
  this?") that would benefit from context about the user's role,
  organization, or product suite.

If you are not sure which KB file or prior task contains the answer,
use `flow search "<terms>" --in memories` or `flow search "<terms>"`
first, then read only the specific source file or entity that the result
identifies. Search is compatible with lazy loading because it is an
on-demand lookup for a concrete question, not bootstrap-time bulk read.

Signals it's NOT time to read the KB:

- The user ran a one-shot mutation command (`flow done`, `flow archive`,
  `flow delete`, `flow restore`, `flow update task`, etc.) and you're
  just relaying the result.
- The current task is purely mechanical and self-contained ("run the
  tests", "fix the obvious typo").
- You already read the relevant file earlier this session and nothing
  new has been written to it since.

**Read at most the specific file you need, not all 5.** If you need
`org.md` to identify someone, don't also preemptively Read the other
four. Load on demand, one file at a time.

**Writing (scoop mode) is still eager.** The lazy rule is for reading.
When you hear a durable fact, append it to the matching kb file
immediately — that doesn't require the file to have been loaded first.
Read-Write just means "load, check for duplicates, write" as a single
sequence at the moment the fact is heard.

**Auxiliary files in entity directories** (any `.md` files in
`tasks/<slug>/`, `projects/<slug>/`, or `playbooks/<slug>/` other than
`brief.md` and the contents of `updates/`) are surfaced by `flow show`
under an `other:` section. These are sidecar references and copied context
files, not task deliverables. Apply the same lazy-load discipline as KB
files: load them on demand when relevant to the work, not preemptively.

**Task artifacts directory.** When you create a deliverable for the current
task — research report, design note, generated data, screenshot, PDF, script
output, or any other file the user should inspect later — write it under:

```
$FLOW_ROOT/tasks/<task-slug>/artifacts/
```

If `$FLOW_ROOT` is unset, use the default `~/.flow`. Create the `artifacts/`
directory if needed. Mission Control's task **Artifacts** tab is driven by
files in this directory. Do not write new deliverables as top-level files next
to `brief.md`; top-level `.md` files are `other:` context, not UI artifacts.
Use top-level sidecars only for on-demand references that future sessions may
load as context rather than user-facing outputs.

**Past tasks and projects can be referenced too.** `flow list tasks` and
`flow list projects` default to non-archived, non-deleted active rows;
done, archived, and deleted rows need explicit flags: `--status done` for
completed work, `--include-archived` to include archived rows,
`--deleted` to see only soft-deleted rows, and `--include-deleted` to
include active plus soft-deleted rows. `flow show task <slug>` and
`flow transcript <slug>` work on done/archived/deleted tasks too.

### 4.18 Recover a KB or brief file from backup

**Triggers:** "restore <file>", "roll back org.md", "the KB got wiped",
"undo that edit to the brief", "an earlier version of <kb/brief>",
"what changed in <file>", "recover the knowledge base".

flow keeps a durable, versioned backup of all curated markdown — the
knowledge base (`kb/*.md`) and every project/task/playbook/owner
`brief.md` + `updates/*.md` — in a self-managed git repo under the flow
root, plus rotated database snapshots. A checkpoint is taken before every
destructive write (KB hygiene pass, UI/CLI edits, the `flow done` sweep),
on server boot, and on a schedule, so prior versions are recoverable
without scraping transcripts. **Use this whenever a curated file was lost,
truncated, or wrongly edited.**

**Recipe:**

1. Identify the file's path relative to the flow root, e.g. `kb/org.md`
   or `tasks/<slug>/brief.md`.
2. Show its history: `flow backup list <relpath>` — each line is a
   version (short sha · timestamp · reason).
3. Inspect a version if needed: `flow backup show <rev> <relpath>` or
   `flow backup diff <rev> <relpath>`.
4. Confirm with the user (via `AskUserQuestion`) before restoring — it's a
   content mutation. Then: `flow backup restore <relpath> [--at <rev>]`.
   With no `--at`, it restores the most recent version that differs from
   the current file (the right default after a wipe). The restore is itself
   checkpointed first, so it is reversible.

`flow backup status` summarizes the schedule, checkpoint/snapshot counts,
and offsite remote. The same history/restore is available on Mission
Control's Knowledge page. The database (task metadata) is backed up as
rotated snapshots under `backups/db/` (not in the markdown repo). For a new
machine, `flow init --restore-from <private-git-url>` rebuilds full state
(markdown + db) from a configured offsite remote.

**Offsite (GitHub).** By default (`FLOW_BACKUP_OFFSITE=auto`), when a **personal**
GitHub token is available, flow automatically provisions a **private** `flow-backup`
repo in the operator's personal GitHub account (via the go-github SDK) and syncs to
it on the schedule + on boot; with no token it stays local-only. The token must be a
PERSONAL one (classic PAT with `repo`, or fine-grained with Administration+Contents)
— the **GitHub App connector cannot host backups**: it mints only installation
tokens, which GitHub does not allow to create a repo in a personal account, and the
App is webhook/issue/PR-scoped anyway. The token is set in Mission Control on the
**Knowledge → Backups** panel (stored in the OS keyring, hydrated into
`FLOW_BACKUP_TOKEN`), or supplied via env `GITHUB_TOKEN`/`GH_TOKEN`/`FLOW_BACKUP_TOKEN`
or the `gh` CLI. Set `FLOW_BACKUP_OFFSITE=local` to keep backups on this machine only.
The repo is always private (KB carries personal/org facts) and always under the
personal account, never an org — even if the App is installed on one. To provision/use
it on demand, the agent can run `flow backup remote github`.

**Anti-patterns:**

- Don't hand-edit or `git`-poke the backup repo under the flow root — use
  `flow backup ...`. The repo uses a separated git directory on purpose.
- Don't enable an offsite remote on a public repo — the KB carries
  personal/org facts; the remote must be private.
