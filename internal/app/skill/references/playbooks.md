### 4.12 Add a playbook

**Triggers:** "add a playbook", "create a playbook for X",
"track this as a playbook", "this is something I'll re-run".

**The interview is the whole point** (same philosophy as §4.2 task intake — you interview, then write down what the user said; you do NOT solution during intake).

**Sections to ask, ONE AT A TIME, in this order:**

1. **What?** One sentence describing what each run does.
2. **Why?** Why this playbook exists and what value it produces.
3. **Where?** Work_dir for runs (use §6 recipe).
4. **Each run does** — concrete steps every invocation performs. Bullet
   form. Replaces "Done when" from task intake.
5. **Out of scope?** Non-goals. Optional.
6. **Signals to watch for** — observable conditions that should change
   the run's behavior or trigger an escalation. Replaces "Open
   questions" — playbooks have long lifespans so prospective signals
   matter more than open questions.

**Then before calling `flow add playbook`:**

- Suggest 2-3 slug candidates via `AskUserQuestion` (header:
  "Pick a slug", one option per candidate plus "Other" for custom).
- Project attachment via `AskUserQuestion` (header: "Attach to?",
  one option per existing project plus "None (floating playbook)").
  Skip the question if there are no projects.
- `--mkdir` if work_dir doesn't exist — use `AskUserQuestion`
  (header: "Create dir?", options: "Yes, create it" / "No, fix the
  path") same as §6 step 6.
- **Schedule (optional).** If the user wants this playbook to run on a
  recurring cadence, capture it now and pass `--schedule "<phrase>"`.
  Accepts plain English ("every hour", "every 6 hours", "weekly",
  "Wednesday at 1pm", "daily at 9am") or a raw cron expression. A
  scheduled playbook fires **autonomously in `--auto` mode** when due
  (headless, no tab); manual `flow run playbook` runs still open a
  visible session. Leave it unset for manual-only playbooks. See §4.13a.

**Draft the brief, show to the user**, then use `AskUserQuestion`
(header: "Brief", options: "Save it" / "Revise") to confirm. Do not
run `flow add playbook` until the user picks "Save it". Then run it
and overwrite the stub `brief.md` with the full content (Read once,
then Edit/Write — same pattern as §5.2). Use the playbook brief
template from §7.

After save, use `AskUserQuestion` (header: "Run it now?", options:
"Run it now" / "Just save the definition") to offer the first run.
On "Run it now", proceed to §4.13. On "Just save the definition",
stop.

### 4.13 Run a playbook

**Triggers — any of these means "run `flow run playbook <slug>`":**
- "run the X playbook" / "trigger X" / "fire the X playbook"
- "fire the X agent" (legacy term users may use — playbook is the canonical name)
- "start a run of X" / "kick off X"
- A bare `flow run playbook X` typed as command

**Recipe:**

1. Ask session-mode (Regular vs Skip permissions vs Autonomous) via
   AskUserQuestion — reuses the §4.4 pattern. Skip if the user already
   specified. If the user asks for Codex, append `--agent codex`.
2. Run: `flow run playbook <slug>` (with `--dangerously-skip-permissions`
   if chosen, `--auto` if Autonomous is chosen, and `--agent codex` if
   requested). Forward `--with` / `--with-file` only with `--auto`.
3. The command creates a kind=playbook_run task and snapshots the brief.
   Regular and skip-permissions modes spawn a terminal tab. Autonomous mode
   runs detached through the task provider (`claude -p` or `codex exec`) and
   completes only when the run calls `flow done`.

**Anti-pattern (per §8):** never *ad-hoc* auto-fire. A bare `flow run
playbook` happens only on an explicit verb ("run", "trigger", "fire",
"start") — mentioning a playbook name in passing is not a trigger.
(Recurring **scheduled** firing is different: it's an explicit, persisted
schedule the user configured via §4.13a, and the scheduler fires it in
`--auto` mode when due. Setting up a schedule still requires the user to
ask for it.)

### 4.13a Schedule a playbook (recurring runs)

**Triggers:** "run X every day", "schedule the X playbook", "fire X every
6 hours", "run X weekly", "set X to run Wednesday at 1pm", "stop scheduling
X", "pause the X schedule".

A playbook can carry a recurring schedule so it fires unattended on a
cadence, in addition to manual triggering. **Scheduled runs always run in
`--auto` mode** (headless, self-closing, no terminal tab); a manual
`flow run playbook` still opens a visible session. The scheduler lives in
`flow ui serve` (an in-process heartbeat), so schedules fire whenever
Mission Control is running; a `flow playbook tick-due` entry also exists
for host cron when the server isn't running.

**Schedule expressions** — pass any of these as `--schedule "<phrase>"`:
- Presets: "every hour" / "hourly", "every day" / "daily", "weekly".
- Intervals: "every 6 hours", "every 30 minutes".
- Day-and-time: "Wednesday at 1pm", "every monday at 9am", "daily at 18:00".
- Raw cron: "0 13 * * 1-5".

**Recipe:**
- **Set / change** a schedule: `flow update playbook <slug> --schedule
  "<phrase>"` (or set it at creation with `flow add playbook ... --schedule`).
  If the phrase isn't understood, the command errors with examples —
  relay that and ask the user to rephrase; do not guess a cron.
- **Pause** (keep the schedule, stop firing): `flow update playbook <slug>
  --pause-schedule`. **Resume:** `--resume-schedule`.
- **Remove** entirely: `flow update playbook <slug> --clear-schedule`.
- **Inspect:** `flow show playbook <slug>` prints the schedule, next fire,
  and last fire. Mission Control's playbook detail shows the same with
  inline edit/pause/resume/clear controls.

**Behavior the user should know (surface only if relevant):**
- **Overlap:** if a prior scheduled run is still in flight when the next
  fire is due, the scheduler **skips** that fire and advances to the next.
- **Catch-up:** a schedule that came due while the machine was asleep /
  the server was down fires **once** on the next check, not once per
  missed interval.
- **Timezone:** day-and-time schedules use the machine's local timezone.

**Anti-patterns:**
- Do not invent or hand-edit schedule columns / `next_fire_at` in the DB —
  use the `flow update playbook` flags so next-fire is recomputed correctly.
- Do not set a schedule the user didn't ask for. Scheduling is opt-in.

#### Persisting in-run adjustments back to the playbook

A playbook run executes against a **frozen snapshot** of the playbook's
`brief.md`. Sometimes during a run the user adjusts the procedure —
"let's always do X here", "change the approach for step 3", "this step
should also check Y". When that happens, the run-time session has two
sources of truth diverging:

- The run's `brief.md` snapshot — what THIS run is executing against
- The playbook's live `brief.md` — what FUTURE runs will inherit

If the user's adjustment is meant to apply only to this run, do nothing
extra. But if it's a procedural improvement worth keeping, the live
playbook brief should be updated — otherwise next week's run forgets
the lesson.

**Trigger this prompt when:** the user makes a non-trivial procedural
change during a run — adds a step, changes the approach for a step,
adds a signal to watch for, narrows or expands scope. Tiny tactical
tweaks ("skip step 4 today, the system is offline") don't count;
durable changes do.

**Recipe — use AskUserQuestion:**

```
AskUserQuestion({
  questions: [{
    question: "Persist this change to the playbook so future runs include it?",
    header: "Persist?",
    options: [
      { label: "Persist to playbook",  description: "Edit playbooks/<slug>/brief.md so future runs inherit the change" },
      { label: "Just this run",         description: "Apply to this run only; future runs continue with the existing playbook" },
      { label: "Both — persist + note", description: "Edit the live playbook AND log the rationale in playbooks/<slug>/updates/" }
    ],
    multiSelect: false
  }]
})
```

**Important rules:**

- **Never edit the run-task's own `brief.md`** to change future behavior.
  That's a frozen snapshot — editing it has no effect on future runs and
  obscures what the run actually executed against.
- **The live playbook brief lives at `~/.flow/playbooks/<slug>/brief.md`.**
  Edit that file directly when persisting.
- **The "Both" option** is the right answer when the change is worth
  capturing AND its rationale is non-obvious from the diff alone — the
  update note explains *why*, the brief edit captures *what*.
- **Do not auto-persist without asking.** Even a clear improvement may
  be deliberately scoped to this run by the user.

#### First-run capture (special case)

The **first run** of a playbook is where the actual procedure
crystallizes. The brief was written aspirationally; concrete commands,
scripts, decision rules, and edge cases get discovered for the first
time. Without active capture, all that learning evaporates when the run
closes.

**Detection:** the bootstrap prompt sets a banner — "⚡ THIS IS THE
FIRST RUN OF THIS PLAYBOOK ⚡" — when the run-task is the only
non-archived `kind=playbook_run` row for its `playbook_slug`. Treat
that as your signal.

**Behavior on first run — be more proactive than usual:**

1. **Scripts and commands.** When you write a script, settle on a
   concrete command, or develop a snippet that wasn't in the brief,
   pause and AskUserQuestion *immediately*:

   ```
   AskUserQuestion({
     questions: [{
       question: "Capture this <script|command|decision> back to the playbook?",
       header: "Capture?",
       options: [
         { label: "Add to playbook brief",  description: "Append/edit the relevant section of playbooks/<slug>/brief.md — future runs see it inline" },
         { label: "Save as playbook reference",   description: "Write to playbooks/<slug>/<topic>.md (e.g., decision-tree.md, sample-script.md). Surfaced under other: for on-demand load" },
         { label: "Just this run",          description: "Apply locally; don't change the playbook (rare for first run)" }
       ],
       multiSelect: false
     }]
   })
   ```

2. **Edge cases / signals.** When the user hits a condition the brief
   didn't anticipate, AskUserQuestion whether to add it to the "Signals
   to watch for" section of the live brief.

3. **End-of-run capture sweep.** Before `flow done`, AskUserQuestion:

   > "Capture anything from this run back to the playbook before closing?"
   > - Yes — walk me through what to capture
   > - No, close out as-is

   On "walk me through": list the candidate captures (scripts produced,
   decisions made, edge cases hit, commands actually used). Offer each
   one via AskUserQuestion individually so the user can opt in
   per-item.

**Playbook references vs brief edits:**

- **Brief edits** are for *procedural* changes — additions to "Each run
  does", new "Signals to watch for", clarified scope. Inline content
  that every future run benefits from seeing during bootstrap.
- **Playbook reference files** (`playbooks/<slug>/<topic>.md`) are for
  reusable context — scripts, decision trees, sample outputs, reference
  tables. Things that future runs may or may not need; they're surfaced
  under `other:` in `flow show playbook` and loaded on-demand by the run
  session. Concrete deliverables produced by a run still belong under the
  run task's `artifacts/` directory, not beside the playbook brief.

**Capture-back is a primary deliverable of the first run.** Not an
afterthought. After the first run, the playbook should be
substantially more concrete than it started.

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
