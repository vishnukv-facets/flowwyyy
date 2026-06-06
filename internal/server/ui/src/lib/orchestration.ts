// Client-side orchestration-tree assembly.
//
// flow's spawn/tell/wait model links tasks via `parent_slug` (a denormalized
// first-parent mirror) and the task_dependencies table (the full parent set,
// surfaced as `parents`). The server already ships parent_slug / parents /
// children on every row in /api/tasks (see BuildTaskView in views.go), and that
// list query is live-invalidated on every activity event plus a 5s poll. So we
// rebuild the whole family hierarchy in the browser from the flat list — no
// dedicated tree endpoint, and running/done state is current for free.
//
// Tree-first by design: we render the parent_slug spine as a tree and flag
// additional parents (DAG edges) as a hint, rather than drawing a full graph.
import type { TaskSummary, TaskView } from './types'

export interface OrchNode {
  task: TaskView
  children: OrchNode[]
  /** Parents beyond the primary parent_slug — a DAG hint surfaced on the node. */
  extraParents: number
}

/**
 * Canonical per-task status derivation, mirroring the Tasks table row
 * (`task.live ? 'running' : task.waiting_on ? 'waiting' : task.status`). Kept
 * here so the tree's status dots match the rest of the UI exactly.
 */
export function nodeStatus(t: TaskView): string {
  return t.live ? 'running' : t.waiting_on ? 'waiting' : t.status
}

/** Stable spawn-order sort: oldest first, slug as tiebreaker. */
function byCreated(a: TaskView, b: TaskView): number {
  if (a.created_at !== b.created_at) return a.created_at < b.created_at ? -1 : 1
  return a.slug < b.slug ? -1 : a.slug > b.slug ? 1 : 0
}

/**
 * Build the orchestration tree for `focusSlug`'s family: walk up via
 * parent_slug to the topmost reachable ancestor, then assemble the full subtree
 * beneath it. Both walks are cycle-safe (a `visited` guard), so a malformed
 * parent chain degrades to a finite tree instead of hanging the render. A task
 * with no parent and no children yields a single-node tree. Returns null only
 * when `focusSlug` isn't present in `tasks`.
 */
export function buildFamilyTree(tasks: TaskView[], focusSlug: string): OrchNode | null {
  const bySlug = new Map<string, TaskView>()
  for (const t of tasks) bySlug.set(t.slug, t)
  const focus = bySlug.get(focusSlug)
  if (!focus) return null

  // children index keyed by each task's parent_slug
  const kids = new Map<string, TaskView[]>()
  for (const t of tasks) {
    const p = t.parent_slug
    if (!p) continue
    const arr = kids.get(p)
    if (arr) arr.push(t)
    else kids.set(p, [t])
  }

  // walk up to the root ancestor (cycle-safe)
  let root = focus
  const seenUp = new Set<string>([root.slug])
  while (root.parent_slug) {
    const parent = bySlug.get(root.parent_slug)
    if (!parent || seenUp.has(parent.slug)) break
    seenUp.add(parent.slug)
    root = parent
  }

  // build the subtree downward (cycle-safe via seenDown)
  const seenDown = new Set<string>()
  const build = (t: TaskView): OrchNode => {
    seenDown.add(t.slug)
    const childTasks = (kids.get(t.slug) ?? [])
      .filter((c) => !seenDown.has(c.slug))
      .sort(byCreated)
    const parentCount = t.parents?.length ?? (t.parent_slug ? 1 : 0)
    return {
      task: t,
      extraParents: Math.max(0, parentCount - 1),
      children: childTasks.map(build),
    }
  }
  return build(root)
}

/** Total node count in a tree — for tab labels and modal titles. */
export function countNodes(node: OrchNode | null): number {
  if (!node) return 0
  let n = 1
  for (const c of node.children) n += countNodes(c)
  return n
}

// ---- startability & whole-list forest (Tasks-screen tree mode) ------------
//
// flow links tasks through TWO independent relations and the Tasks screen needs
// to read them differently:
//
//   • parent_slug  — the *grouping* / umbrella spine (e.g. every smart-assistant
//     subtask points at `smart-assistant-roadmap`). This is what we draw as the
//     tree. An umbrella parent is a folder, NOT a gate.
//   • task_dependencies — the real blocking DAG, surfaced per-row as `parents`.
//     A task is blocked while any of these is unfinished.
//
// The server's `parents` is the task_dependencies set, but it falls back to the
// single parent_slug task when a row has no dependency edges (views.go). So to
// recover the *pure* blocking set we drop any parent that is merely the umbrella
// (`p.slug === t.parent_slug`). For the canonical family this yields exactly the
// intended reading: `attention-context-packs` (deps: none) is startable, and
// everything chained off it is blocked until it lands.

const isOpen = (status: string): boolean => status !== 'done'

/**
 * Pure blocking parents of `t`: its task_dependencies parents with the umbrella
 * grouping link (parent_slug) removed. These are the tasks that must finish
 * before `t` can start. Each carries a live `status`, so done-ness is readable
 * without the parent row being present in the list.
 */
export function blockingParents(t: TaskView): TaskSummary[] {
  return (t.parents ?? []).filter((p) => p.slug !== t.parent_slug)
}

export interface Startability {
  /** Unfinished blockers gating this task right now. Empty ⇒ nothing upstream. */
  blockedBy: TaskSummary[]
  /** Open tasks that depend on this one — i.e. this task currently gates them. */
  blocks: TaskSummary[]
  /** Open and unblocked: can be picked up now. */
  startable: boolean
  /** Groups subtasks under it (umbrella parent via parent_slug) — a container,
   * not a leaf unit of work. */
  container: boolean
}

/**
 * Derive per-task startability for the whole list in one pass. `blocks` is the
 * reverse of the (umbrella-stripped) blocking edges, counting only OPEN
 * dependents — a done dependent is no longer gated, so it doesn't inflate the
 * "blocks N" cue. `blockedBy` reads each blocker's embedded status, so it is
 * correct even when the blocker row isn't in `tasks`.
 */
export function computeStartability(tasks: TaskView[]): Map<string, Startability> {
  const summaryOf = new Map<string, TaskSummary>()
  for (const t of tasks) {
    summaryOf.set(t.slug, {
      slug: t.slug,
      name: t.name,
      status: t.status,
      priority: t.priority,
      project_slug: t.project_slug,
      updated_at: t.updated_at,
    })
  }

  // reverse edges: blocker slug -> open dependents that wait on it
  const dependents = new Map<string, TaskSummary[]>()
  for (const t of tasks) {
    if (!isOpen(t.status)) continue
    const self = summaryOf.get(t.slug)!
    for (const p of blockingParents(t)) {
      const arr = dependents.get(p.slug)
      if (arr) arr.push(self)
      else dependents.set(p.slug, [self])
    }
  }

  const out = new Map<string, Startability>()
  for (const t of tasks) {
    const blockedBy = blockingParents(t).filter((p) => isOpen(p.status))
    const blocks = dependents.get(t.slug) ?? []
    out.set(t.slug, {
      blockedBy,
      blocks,
      startable: isOpen(t.status) && blockedBy.length === 0,
      container: (t.children?.length ?? 0) > 0,
    })
  }
  return out
}

export interface ForestNode {
  task: TaskView
  children: ForestNode[]
}

/**
 * Assemble the parent_slug forest over `tasks`. A task whose parent_slug is
 * absent from the set (or empty) becomes a root; children nest under their
 * parent. Cycle-safe via a `seen` guard, so a malformed chain degrades to a
 * finite forest. `compare` orders roots and each sibling group.
 */
export function buildForest(
  tasks: TaskView[],
  compare: (a: TaskView, b: TaskView) => number,
): ForestNode[] {
  const bySlug = new Map(tasks.map((t) => [t.slug, t]))
  const kids = new Map<string, TaskView[]>()
  for (const t of tasks) {
    const p = t.parent_slug
    if (!p || !bySlug.has(p)) continue
    const arr = kids.get(p)
    if (arr) arr.push(t)
    else kids.set(p, [t])
  }
  const seen = new Set<string>()
  const build = (t: TaskView): ForestNode => {
    seen.add(t.slug)
    const childTasks = (kids.get(t.slug) ?? []).filter((c) => !seen.has(c.slug)).sort(compare)
    return { task: t, children: childTasks.map(build) }
  }
  const roots = tasks.filter((t) => !t.parent_slug || !bySlug.has(t.parent_slug)).sort(compare)
  return roots.map(build)
}

/**
 * Filter a forest to nodes matching `keep`, preserving the ancestor chain of any
 * kept node so matches always render with their grouping context (e.g. a search
 * hit deep in a family still shows under its umbrella parent).
 */
export function pruneForest(
  nodes: ForestNode[],
  keep: (t: TaskView) => boolean,
): ForestNode[] {
  const out: ForestNode[] = []
  for (const n of nodes) {
    const children = pruneForest(n.children, keep)
    if (keep(n.task) || children.length > 0) out.push({ task: n.task, children })
  }
  return out
}

export interface FlatRow {
  task: TaskView
  depth: number
  /** Whether each ancestor (outermost→innermost) was its parent's last child —
   * drives the box-drawing connector prefix. */
  ancestorLines: boolean[]
  isLast: boolean
  hasChildren: boolean
  collapsed: boolean
}

/**
 * Depth-first flatten of a (pruned) forest into table rows, honouring collapse
 * state: a collapsed node keeps its row but omits its descendants. `ancestorLines`
 * lets the row draw the same connector glyphs as the SessionDetail orch tree.
 */
export function flattenForest(
  nodes: ForestNode[],
  collapsed: Set<string>,
  ancestorLines: boolean[] = [],
): FlatRow[] {
  const out: FlatRow[] = []
  nodes.forEach((n, i) => {
    const isLast = i === nodes.length - 1
    const hasChildren = n.children.length > 0
    const isCollapsed = collapsed.has(n.task.slug)
    out.push({
      task: n.task,
      depth: ancestorLines.length,
      ancestorLines,
      isLast,
      hasChildren,
      collapsed: isCollapsed,
    })
    if (hasChildren && !isCollapsed) {
      out.push(...flattenForest(n.children, collapsed, [...ancestorLines, isLast]))
    }
  })
  return out
}
