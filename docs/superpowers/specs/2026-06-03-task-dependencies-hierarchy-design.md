# First-class task dependencies & hierarchy

Date: 2026-06-03
Status: Draft for review
Scope: flow-manager (`flow` CLI + skill + web UI)

## Problem

flow already has a real dependency backend, but it's only half-exposed and one
concept is doing two jobs. Three concrete gaps motivated this work:

1. **No creation-time capture.** `flow add task` has no dependency/parent/due
   flag, and the UI "Create task" modal has no field for dependencies, parent,
   or due date. So when a human or an agent creates a task, the relationships
   that matter can only be wired *after the fact* (via `flow update task` /
   `flow spawn`) or not at all.
2. **No editing in the UI.** The only edit mutations the web UI sends are
   `update-priority`, `update-project`, `update-task-name` (plus
   permission-mode / provider). Due date, dependencies, hierarchy, assignee,
   tags, and waiting are all **CLI-only** today.
3. **One edge does two jobs.** "Parent-child hierarchy" and "dependency" are
   the *same* relationship in flow: a task's parent **is** its start-blocker
   (`task_dependencies` + the `tasks.parent_slug` mirror, read by
   `TaskStartBlockerFor`). This conflation produces a live contradiction in
   `flow spawn --parent` (below) and makes the model hard to reason about.

Additionally there is **no cycle detection**: `A` depends on `B`, `B` depends
on `A` is accepted today and deadlocks both tasks forever
(`EnsureTaskStartable` sees an always-pending parent on each).

### The spawn contradiction (verified)

`flow spawn --parent X` calls `AddTaskParent(child, X)` (`spawn.go:124`), which
makes the **child depend on X**, then calls `cmdDo` (`spawn.go:157`), which runs
`EnsureTaskStartable` (`do.go:115`). If X is in-progress (the normal case — a
running parent agent delegating subwork), spawn creates the child and then
**refuses to open it** because X isn't done. The skill describes `spawn` as
"fire the child off in its own tab," which the edge semantics directly
contradict. This is the clearest evidence that hierarchy and dependency must be
separate relationships.

## What already exists (do NOT rebuild)

- `task_dependencies(child_slug, parent_slug)` — many-to-many table, FK cascade,
  self-loop CHECK, indexes (`db.go`).
- `tasks.parent_slug` — denormalized single-parent mirror.
- `TaskStartBlockerFor` / `EnsureTaskStartable` / `TaskStartBlocker` /
  `PendingParent` — the blocking-gate logic, enforced at `flow do`
  (`do.go:115`), `flow do --here` (`do.go:728`), and server actions
  (`actions.go:581,1128,1327,1369,1418`, `terminal_bridge.go:1041`).
- `AddTaskParent` / `RemoveTaskParent` / `ClearTaskParents` / `ListParentSlugs`
  / `syncLegacyParentSlug` (`db.go`).
- `flow update task` already edits: `--parent` / `--remove-parent` /
  `--clear-parent`, `--due-date` / `--clear-due`, `--priority`, `--status`,
  `--assignee` / `--clear-assignee`, `--tag` / `--remove-tag` / `--clear-tags`,
  `--waiting` / `--clear-waiting`, `--project` / `--clear-project` (`update.go`).
- UI **reads** `parent_slug`, `parent`, `parents[]`, `children[]`, `waiting_on`,
  `due_date`, `due_info` (`types.go` / `types.ts`), renders a "Dependencies"
  column (`Tasks.tsx`), a "Due" column + agenda buckets, and an ASCII
  orchestration tree (`orchestration.ts`, `SessionDetail.tsx`).

## Model: two orthogonal relationships

| | **Hierarchy** (subtask-of) | **Dependency** (blocked-by) |
|---|---|---|
| Meaning | *where a task lives* (organization / rollup) | *when a task may run* (sequencing) |
| Cardinality | single parent (a tree) | many-to-many (a DAG) |
| Blocks start? | **never** | **yes** — child can't start until all parents are done |
| Storage | `tasks.parent_slug` (decoupled from blocker) | `task_dependencies` (unchanged) |
| Display | orchestration tree | "blocked by N / blocks N" edges |

**Decision: hierarchy never blocks anything.** Parents and children start and
complete independently. If you need "this umbrella can't proceed until X is
done," express X as a *dependency*. Rollup gating (parent-done requires
children-done) is explicitly **out of scope** — a non-breaking future extension.

### The core decoupling

Today `AddTaskParent` writes `task_dependencies` *and* mirrors the first parent
into `tasks.parent_slug`, and `TaskStartBlockerFor` falls back to `parent_slug`.
We sever that wire:

- `task_dependencies` → **blocking dependencies only**. `TaskStartBlockerFor`
  reads *only* this table. Remove the `parent_slug` fallback in
  `TaskStartBlockerFor` (`db.go:365-383`).
- `tasks.parent_slug` → **hierarchy only**. Never consulted by the blocker.
  Stop `AddTaskParent` from calling `syncLegacyParentSlug`; introduce dedicated
  hierarchy setters instead (below).

This decoupling **also fixes the spawn contradiction**: `flow spawn --parent`
becomes a hierarchy link, so the child opens immediately.

### New DB functions (`internal/flowdb/db.go`)

- `SetTaskHierarchyParent(db, childSlug, parentSlug)` — sets `tasks.parent_slug`
  (hierarchy). Validates: parent exists, not self, and **no hierarchy cycle**
  (walk `parent_slug` ancestors from `parentSlug`; reject if `childSlug` is
  reachable).
- `ClearTaskHierarchyParent(db, childSlug)` — sets `parent_slug = NULL`.
- `AddTaskDependency(db, childSlug, parentSlug)` — replaces the dependency role
  of `AddTaskParent`; no longer mirrors into `parent_slug`. Validates self-loop
  (existing) **and no dependency cycle** (walk `task_dependencies` parents
  transitively from `parentSlug`; reject if `childSlug` is reachable).
- Keep `RemoveTaskDependency` / `ClearTaskDependencies` (rename of the
  `*Parent` removers; drop the `syncLegacyParentSlug` call).
- `AddTaskParent` and friends are kept as **thin deprecated aliases** delegating
  to `AddTaskDependency` etc., so no caller breaks during the transition.

### Cycle detection

A single internal helper `wouldCreateCycle(db, edgeTable/column, from, to)`
does a bounded DFS over the relevant edges. Used by both
`SetTaskHierarchyParent` and `AddTaskDependency`. On rejection, return a
descriptive error naming the offending chain (e.g.
`adding dependency a→b would create a cycle: a → b → c → a`). Depth-bounded
(e.g. 1000) as a runaway guard.

### Migration (match prior behavior)

- Existing `task_dependencies` rows **stay as blocking dependencies** — no
  behavior change for anything currently using real dependencies.
- Existing `tasks.parent_slug` values are (today) mirrors of the first
  dependency. Migration **nulls `parent_slug` where a matching
  `task_dependencies(child, parent_slug)` row exists**, so:
  - hierarchy starts genuinely empty (no edge shows as both dep and parent), and
  - the blocking relationship is fully preserved by the surviving
    `task_dependencies` row.
- A `parent_slug` with **no** matching dependency row (shouldn't occur given the
  current mirror invariant, but defensively) is left intact as hierarchy.
- Idempotent (guarded by a probe, like the other `migrate*` funcs).

Behavioral consequence to call out: **new** `flow spawn --parent` creates
hierarchy (non-blocking); previously-spawned edges that were blocking remain
blocking (we can't distinguish them retroactively). This preserves existing
tasks while fixing the behavior going forward.

## CLI surface

### `flow add task` (`internal/app/add.go`)

Add three flags (all optional, all validated before insert):

- `--depends-on <slug>` (repeatable) — add a blocking dependency. Resolves each
  slug, runs cycle detection, writes `task_dependencies`.
- `--subtask-of <slug>` — set the hierarchy parent. Resolves, cycle-checks,
  sets `parent_slug`.

Note: `add task` **already** has `--due` (`add.go:151`, wired at `add.go:288`),
plus `--assignee`, `--priority`, `--permission-mode`, `--project`. So
creation-time capture of those is **already done on the CLI**; the only new
`add task` work is `--depends-on` / `--subtask-of`. (The due-date *creation* gap
is UI-only — see UI section.)

### `flow update task` (`internal/app/update.go`)

- Add `--depends-on` / `--remove-dep` / `--clear-deps` (repeatable) as the
  clear, canonical dependency flags → `AddTaskDependency` etc.
- Keep `--parent` / `--remove-parent` / `--clear-parent` as **deprecated
  aliases** for the dependency flags (preserve scripts + skill text), with a
  one-line stderr deprecation hint pointing at `--depends-on`.
- Add `--subtask-of <slug>` / `--unparent` for hierarchy →
  `SetTaskHierarchyParent` / `ClearTaskHierarchyParent`.
- Everything else (`--due-date`, `--priority`, `--assignee`, `--tag`,
  `--waiting`, `--project`) is unchanged and already complete.

### `flow spawn` (`internal/app/spawn.go`)

- `--parent` now calls `SetTaskHierarchyParent(child, parent)` (hierarchy),
  **not** `AddTaskDependency`. This is the contradiction fix.
- Add `--depends-on <slug>` (repeatable) so a spawned task can *also* declare
  real blocking deps when that's genuinely intended.
- Brief provenance line stays ("Spawned from parent task: ...").

### `flow show task` (`internal/app/show.go`)

Display the two relationships separately:
- `subtask of:` <hierarchy parent> and `subtasks:` <children by parent_slug>.
- `depends on:` <blocking parents> and `blocks:` <blocking children>, with each
  parent's status so a blocked task is obvious.

## Skill / agent intake (`internal/app/skill/SKILL.md`)

- Document the two concepts explicitly and when to use each (replace the current
  "`--parent` for real dependencies, `waiting_on` for loose blockers" framing,
  which now reads as three overlapping things).
- Update §4.17 (orchestration) for the new `spawn` semantics (hierarchy,
  non-blocking) and the `--depends-on` option.
- Task-creation interview: after capturing What/Why/Where, the agent asks (when
  plausibly relevant, not robotically) — "Does this depend on any existing task
  finishing first? Is it a subtask of something? Due date?" — and wires them via
  the new `add task` flags at creation time.
- Update the `flow update task` reference section for the new flags + the
  `--parent` deprecation alias.
- Skill is source of truth: rebuild after editing so `flow skill update` ships
  it (per CLAUDE.md).

## UI (`internal/server/ui/src` + `internal/server/`)

### Create modal (`components/modals.tsx`, `CreateTaskModal`)

Add fields: **Due date** (date input), **Depends on** (multi-select task
picker), **Subtask of** (single task picker). Extend the `create-flow` payload
with `due`, `depends_on[]`, `subtask_of`. Server `create-flow` handler
(`actions.go:230`) wires them through the same DB functions.

### Edit affordance for existing tasks

The UI has no general task editor today. Add an **Edit task** modal (or extend
the existing inline menus) covering: due date, dependencies, subtask-of,
assignee, tags, waiting. Back it with **new RPC mutation kinds** that call the
same validated DB functions the CLI uses:
`update-due`, `update-deps` (add/remove), `update-hierarchy`, `update-assignee`,
`update-tags`, `update-waiting`. (Each mirrors a `flow update task` flag, so the
logic should be factored so CLI and server share it rather than duplicating SQL.)

### Display

- Rework the `Tasks.tsx` "Dependencies" column to read from the dependency
  edges (`parents`/`children` as *blocking* deps), separate from hierarchy.
- The orchestration tree (`orchestration.ts`) now renders the **hierarchy**
  spine (`parent_slug`); show blocking dependencies as a distinct annotation
  ("blocked by N" / "blocks N"), not as tree nesting.
- `TaskView` / `TaskView` TS type: keep `parent`/`parents`/`children` but make
  their meaning explicit (hierarchy vs dependency); add separate fields if the
  two need to coexist in one payload (e.g. `subtask_of`, `subtasks[]` for
  hierarchy; `depends_on[]`, `blocks[]` for dependencies). Finalize field names
  during Phase 3.

## Phasing

1. **Model + CLI + validation** (pure Go, fully unit-testable): DB split +
   decoupling, cycle detection, migration, `add task` / `update task` /
   `spawn` flag changes, `show task` output. Independently shippable.
2. **Agent intake**: SKILL.md updates + rebuild. Depends on Phase 1 flags.
3. **UI**: create-modal fields, edit modal, new RPC mutations, display rework.
   Depends on Phase 1 (and shares its DB functions).

## Testing

- Table-driven Go tests in `internal/flowdb` for: cycle detection (hierarchy &
  dependency, direct and transitive), decoupling (setting hierarchy does not
  block; setting dependency does), migration (existing dep rows preserved,
  mirror `parent_slug` nulled).
- `internal/app` tests for the new flags on `add`/`update`/`spawn`, the
  `--parent` deprecation alias, and the spawn-no-longer-blocks behavior
  (regression test for the contradiction).
- E2E: extend `e2e_test.go` to create a task with `--depends-on` + `--subtask-of`
  and assert `flow do` blocks on the dependency but not the hierarchy parent.
- No DB mocks (repo convention); real SQLite in a temp dir.

## Out of scope (YAGNI)

- Rollup gating (parent-done requires children-done).
- A true graph visualization (keep the tree + dependency annotations).
- Cross-project dependency policies.
- Reordering / priority inheritance across the tree.

## Decisions (reversible — flag if you'd choose differently)

1. **Flag names: `--depends-on` and `--subtask-of`.** Self-documenting and
   unambiguous about direction. (`--needs`/`--blocks` invert confusingly;
   `--under` is cute but vague.)
2. **Keep `--parent` as a deprecated alias** for `--depends-on`, with a stderr
   hint. The skill is the main consumer and we control it, but a hard-cut risks
   silently breaking any user scripts — match-prior-behavior wins here. Revisit
   removal in a later cleanup.
3. **UI edit: inline-per-field, matching existing patterns.** The UI already
   edits priority/provider/status inline; due-date and subtask-of fit the same
   inline-picker model. The multi-value dependency editor gets a small
   "Dependencies" sub-panel on the task detail view rather than a full modal.
   Finalized in Phase 3 against the actual component conventions.
