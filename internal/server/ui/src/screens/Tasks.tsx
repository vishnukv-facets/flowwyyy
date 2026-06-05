import { useMemo, useState } from 'react'
import { useLocation, useSearch } from 'wouter'
import {
  Archive,
  ArchiveRestore,
  ChevronDown,
  ChevronUp,
  CornerLeftUp,
  GitFork,
  Loader2,
  Pencil,
  Search,
  Trash2,
  X,
} from 'lucide-react'
import { useAction, useTasks, queryClient, type TaskFilters } from '../lib/query'
import { apiAction } from '../lib/api'
import { pushToast } from '../lib/toast'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { confirmAction } from '../lib/confirm'
import { EmptyState, ErrorNote, Loading, ProviderIcon, StatusDot } from '../components/ui'
import { Select } from '../components/Select'
import { ago, dueTone } from '../lib/format'
import { clickable } from '../lib/a11y'
import type { TaskView } from '../lib/types'

const STATUSES = [
  { v: '', label: 'All open' },
  { v: 'backlog', label: 'Backlog' },
  { v: 'in-progress', label: 'In progress' },
  { v: 'done', label: 'Done' },
  { v: 'archived', label: 'Archived' },
]
const PRIOS = [
  { v: '', label: 'Any' },
  { v: 'high', label: 'High' },
  { v: 'medium', label: 'Medium' },
  { v: 'low', label: 'Low' },
]
const PRIO_RANK: Record<string, number> = { high: 0, medium: 1, low: 2 }

type SortField = 'name' | 'project' | 'priority' | 'due' | 'updated' | 'created'
type SortDir = 'asc' | 'desc'
// Default direction when a column is first clicked: names/projects read better
// ascending, everything else newest/highest first.
const DEFAULT_DIR: Record<SortField, SortDir> = {
  name: 'asc',
  project: 'asc',
  priority: 'asc',
  due: 'asc',
  updated: 'desc',
  created: 'desc',
}

function compareTasks(a: TaskView, b: TaskView, field: SortField, dir: SortDir): number {
  const mul = dir === 'asc' ? 1 : -1
  switch (field) {
    case 'name':
      return mul * a.name.localeCompare(b.name)
    case 'project':
      return mul * (a.project_slug || '').localeCompare(b.project_slug || '')
    case 'priority':
      return mul * ((PRIO_RANK[a.priority] ?? 9) - (PRIO_RANK[b.priority] ?? 9))
    case 'due':
      // Dated tasks always sort before undated ones; dir flips order among the
      // dated set (asc = soonest/overdue first).
      if (a.due_date && b.due_date) return mul * (a.due_date < b.due_date ? -1 : a.due_date > b.due_date ? 1 : 0)
      if (a.due_date) return -1
      if (b.due_date) return 1
      return 0
    case 'updated':
      return mul * (Date.parse(a.updated_at) - Date.parse(b.updated_at))
    case 'created':
    default:
      return mul * (Date.parse(a.created_at) - Date.parse(b.created_at))
  }
}

export function Tasks() {
  useDocumentTitle('Tasks')
  const [, navigate] = useLocation()
  const search = useSearch()
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [bulkPending, setBulkPending] = useState(false)

  // URL is the source of truth for every filter/sort — so they survive a
  // refresh and are shareable. We parse on each render and write via replace.
  const params = useMemo(() => new URLSearchParams(search), [search])
  const status = params.get('status') ?? ''
  const priority = params.get('priority') ?? ''
  const project = params.get('project') ?? ''
  const tag = params.get('tag') ?? ''
  const q = params.get('q') ?? ''
  const sortField = (params.get('sort') as SortField) || 'created'
  const sortDir = (params.get('dir') as SortDir) || 'desc'

  const setParams = (patch: Record<string, string>) => {
    const next = new URLSearchParams(search)
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v)
      else next.delete(k)
    }
    const qs = next.toString()
    navigate(qs ? `/tasks?${qs}` : '/tasks', { replace: true })
  }

  const onSort = (field: SortField) => {
    if (field === sortField) {
      setParams({ sort: field, dir: sortDir === 'asc' ? 'desc' : 'asc' })
    } else {
      setParams({ sort: field, dir: DEFAULT_DIR[field] })
    }
  }

  // "Archived" is an orthogonal axis to status, surfaced as its own segment:
  // when active we ask the server to include archived rows (and done, since an
  // archived task may also be done) and drop the status filter, then narrow to
  // archived-only client-side in `base` below.
  const archivedView = status === 'archived'
  const filters: TaskFilters = {
    status: archivedView ? undefined : status || undefined,
    priority: priority || undefined,
    include_done: status === 'done' || archivedView,
    include_archived: archivedView || undefined,
  }
  const { data, isLoading, error } = useTasks(filters)

  // Status/priority filter the server query; project/tag/text + sort run
  // client-side so the chip options stay visible regardless of the active one.
  const base = useMemo(
    () =>
      (data ?? []).filter((t) => {
        // Archived view: show only archived rows (any status). Other views:
        // never show archived (the server already excludes them; this guards
        // against any leaking through), and hide playbook_run unless on Done.
        if (archivedView) return !!t.archived_at
        if (t.archived_at) return false
        return t.kind !== 'playbook_run' || status === 'done'
      }),
    [data, status, archivedView],
  )
  const projectOpts = useMemo(
    () => Array.from(new Set(base.map((t) => t.project_slug || '').filter(Boolean))).sort(),
    [base],
  )
  const tagOpts = useMemo(
    () => Array.from(new Set(base.flatMap((t) => t.tags ?? []))).sort(),
    [base],
  )
  const tasks = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return base
      .filter((t) => {
        if (project && (t.project_slug || '') !== project) return false
        if (tag && !(t.tags ?? []).includes(tag)) return false
        if (!needle) return true
        return [t.name, t.slug, t.project_slug || '', ...(t.tags ?? [])].some((s) =>
          s.toLowerCase().includes(needle),
        )
      })
      .slice()
      .sort((a, b) => compareTasks(a, b, sortField, sortDir))
  }, [base, project, tag, q, sortField, sortDir])

  const toggleSel = (slug: string) =>
    setSelected((prev) => {
      const next = new Set(prev)
      next.has(slug) ? next.delete(slug) : next.add(slug)
      return next
    })
  const visibleSlugs = useMemo(() => tasks.map((t) => t.slug), [tasks])
  const allSelected = visibleSlugs.length > 0 && visibleSlugs.every((s) => selected.has(s))
  const toggleSelectAll = () =>
    setSelected((prev) => {
      if (allSelected) {
        const next = new Set(prev)
        visibleSlugs.forEach((s) => next.delete(s))
        return next
      }
      return new Set([...prev, ...visibleSlugs])
    })

  // Bulk = iterate the existing single-target action behind one confirm.
  const runBulk = async (opts: {
    kind: string
    verb: string
    extra?: Record<string, string>
    entityKind?: string
    danger?: boolean
  }) => {
    const slugs = [...selected]
    if (!slugs.length) return
    const ok = await confirmAction({
      title: `${opts.verb} ${slugs.length} task${slugs.length === 1 ? '' : 's'}?`,
      body: `This runs "${opts.verb.toLowerCase()}" on each selected task, one at a time.`,
      confirmLabel: opts.verb,
      danger: opts.danger ?? true,
    })
    if (!ok) return
    setBulkPending(true)
    const results = await Promise.allSettled(
      slugs.map((s) =>
        apiAction({ kind: opts.kind, target: s, ...(opts.entityKind ? { entity_kind: opts.entityKind } : {}), ...opts.extra }),
      ),
    )
    setBulkPending(false)
    const failed = results.filter((r) => r.status === 'rejected').length
    setSelected(new Set())
    queryClient.invalidateQueries()
    if (failed) pushToast('error', `${opts.verb}: ${failed}/${slugs.length} failed`)
    else pushToast('ok', `${opts.verb.toLowerCase()} ${slugs.length} task${slugs.length === 1 ? '' : 's'}`)
  }

  const filtersActive = !!(status || priority || project || tag || q)

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">work queue</div>
          <h1 className="h-xl">Tasks</h1>
        </div>
      </div>

      <div className="row gap wrap" style={{ marginBottom: 18, gap: 14, alignItems: 'center' }}>
        <div className="input-icon" style={{ maxWidth: 220 }}>
          <Search size={14} className="dim" />
          <input
            className="input"
            placeholder="Filter tasks…"
            value={q}
            onChange={(e) => setParams({ q: e.target.value })}
          />
        </div>
        <div className="segmented">
          {STATUSES.map((s) => (
            <button key={s.v} className={status === s.v ? 'active' : ''} onClick={() => setParams({ status: s.v })}>
              {s.label}
            </button>
          ))}
        </div>
        <div className="segmented">
          {PRIOS.map((p) => (
            <button key={p.v} className={priority === p.v ? 'active' : ''} onClick={() => setParams({ priority: p.v })}>
              {p.label}
            </button>
          ))}
        </div>
        {projectOpts.length > 1 && (
          <div className="filter-select">
            <Select
              value={project}
              onChange={(v) => setParams({ project: v })}
              options={[
                { value: '', label: `All projects · ${projectOpts.length}` },
                ...projectOpts.map((p) => ({ value: p, label: p })),
              ]}
              placeholder="Projects"
            />
          </div>
        )}
        {tagOpts.length > 0 && (
          <div className="filter-select">
            <Select
              value={tag}
              onChange={(v) => setParams({ tag: v })}
              options={[
                { value: '', label: `All tags · ${tagOpts.length}` },
                ...tagOpts.map((t) => ({ value: t, label: `#${t}` })),
              ]}
              placeholder="Tags"
            />
          </div>
        )}
        {filtersActive && (
          <button className="btn ghost sm" onClick={() => navigate('/tasks', { replace: true })}>
            <X size={13} /> Clear
          </button>
        )}
      </div>

      {isLoading ? (
        <Loading rows={6} />
      ) : error ? (
        <ErrorNote error={error} />
      ) : tasks.length === 0 ? (
        <EmptyState title="No tasks match" hint="Adjust the filters or create a new task." />
      ) : (
        <div className="card" style={{ padding: '6px 14px 4px', marginBottom: selected.size > 0 ? 72 : 0 }}>
          <table className="tbl fixed">
            <colgroup>
              <col style={{ width: 30 }} />
              <col style={{ width: 28 }} />
              <col />
              <col style={{ width: 152 }} />
              <col style={{ width: 100 }} />
              <col style={{ width: 64 }} />
              <col style={{ width: 108 }} />
              <col style={{ width: 38 }} />
              <col style={{ width: 104 }} />
              <col style={{ width: 60 }} />
              <col style={{ width: 60 }} />
            </colgroup>
            <thead>
              <tr>
                <th>
                  <input
                    type="checkbox"
                    aria-label="Select all tasks"
                    checked={allSelected}
                    onChange={toggleSelectAll}
                  />
                </th>
                <th />
                <SortableTh field="name" label="Task" sortField={sortField} sortDir={sortDir} onSort={onSort} />
                <th>Dependencies</th>
                <SortableTh field="project" label="Project" sortField={sortField} sortDir={sortDir} onSort={onSort} />
                <SortableTh field="priority" label="Priority" sortField={sortField} sortDir={sortDir} onSort={onSort} />
                <SortableTh field="due" label="Due" sortField={sortField} sortDir={sortDir} onSort={onSort} />
                <th>Agent</th>
                <th>Tags</th>
                <SortableTh field="updated" label="Updated" align="right" sortField={sortField} sortDir={sortDir} onSort={onSort} />
                <th />
              </tr>
            </thead>
            <tbody>
              {tasks.map((t) => (
                <TaskRow
                  key={t.slug}
                  task={t}
                  selected={selected.has(t.slug)}
                  onToggleSel={() => toggleSel(t.slug)}
                  onOpen={() => navigate(`/session/${t.slug}`)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {selected.size > 0 && (
        <div className="bulk-bar" role="toolbar" aria-label="Bulk actions">
          <span className="bulk-count">{selected.size} selected</span>
          <span className="faint" style={{ fontSize: 12 }}>priority</span>
          {(['high', 'medium', 'low'] as const).map((p) => (
            <button
              key={p}
              className="btn ghost sm"
              disabled={bulkPending}
              onClick={() => runBulk({ kind: 'update-priority', verb: `Set ${p}`, extra: { priority: p }, danger: false })}
            >
              <span className={`prio ${p}`} /> {p}
            </button>
          ))}
          <div className="spacer" />
          {archivedView ? (
            <button className="btn ghost sm" disabled={bulkPending} onClick={() => runBulk({ kind: 'unarchive', verb: 'Unarchive', danger: false })}>
              <ArchiveRestore size={13} /> Unarchive
            </button>
          ) : (
            <button className="btn ghost sm" disabled={bulkPending} onClick={() => runBulk({ kind: 'archive', verb: 'Archive' })}>
              <Archive size={13} /> Archive
            </button>
          )}
          <button className="btn ghost sm" disabled={bulkPending} onClick={() => runBulk({ kind: 'delete', verb: 'Trash', entityKind: 'task' })}>
            <Trash2 size={13} /> Trash
          </button>
          <button className="btn icon ghost sm" title="Clear selection" aria-label="Clear selection" onClick={() => setSelected(new Set())}>
            <X size={14} />
          </button>
        </div>
      )}
    </div>
  )
}

function SortableTh({
  field,
  label,
  sortField,
  sortDir,
  onSort,
  align,
}: {
  field: SortField
  label: string
  sortField: SortField
  sortDir: SortDir
  onSort: (f: SortField) => void
  align?: 'right'
}) {
  const active = sortField === field
  return (
    <th
      aria-sort={active ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none'}
      style={align === 'right' ? { textAlign: 'right' } : undefined}
    >
      <button className={`th-sort${active ? ' active' : ''}`} onClick={() => onSort(field)}>
        {label}
        {active && (sortDir === 'asc' ? <ChevronUp size={11} /> : <ChevronDown size={11} />)}
      </button>
    </th>
  )
}

function TaskRow({
  task,
  selected,
  onToggleSel,
  onOpen,
}: {
  task: TaskView
  selected: boolean
  onToggleSel: () => void
  onOpen: () => void
}) {
  const action = useAction()
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState(task.name)

  const cancel = () => {
    setName(task.name)
    setEditing(false)
  }
  const save = () => {
    const trimmed = name.trim()
    if (!trimmed || trimmed === task.name) {
      cancel()
      return
    }
    action.mutate(
      { kind: 'update-task-name', slug: task.slug, name: trimmed },
      { onSuccess: () => setEditing(false), onError: cancel },
    )
  }

  const archive = async (e: React.MouseEvent) => {
    e.stopPropagation()
    const ok = await confirmAction({
      title: 'Archive this task?',
      body: `"${task.name}" will be moved out of your active queue. You can unarchive it later.`,
      confirmLabel: 'Archive',
      danger: true,
    })
    if (ok) action.mutate({ kind: 'archive', target: task.slug })
  }

  const unarchive = (e: React.MouseEvent) => {
    e.stopPropagation()
    action.mutate({ kind: 'unarchive', target: task.slug })
  }

  const trash = async (e: React.MouseEvent) => {
    e.stopPropagation()
    const ok = await confirmAction({
      title: 'Move this task to trash?',
      body: `"${task.name}" will be soft-deleted and hidden from your lists. You can restore it from Trash later.`,
      confirmLabel: 'Move to trash',
      danger: true,
    })
    if (ok) action.mutate({ kind: 'delete', target: task.slug, entity_kind: 'task' })
  }

  const childCount = task.children?.length ?? 0
  const parentName = task.parent?.name || task.parent_slug
  const forkedFrom = task.forked_from?.name || task.forked_from_slug
  const forkCount = task.forks?.length ?? 0

  return (
    <tr className={selected ? 'row-selected' : ''} {...clickable(onOpen, { disabled: editing })} aria-label={`Open ${task.name}`}>
      <td>
        <input
          type="checkbox"
          aria-label={`Select ${task.name}`}
          checked={selected}
          onClick={(e) => e.stopPropagation()}
          onChange={onToggleSel}
        />
      </td>
      <td>
        <StatusDot status={task.live ? 'running' : task.waiting_on ? 'waiting' : task.status} />
      </td>
      <td>
        {editing ? (
          <input
            className="input inline-rename"
            autoFocus
            value={name}
            onClick={(e) => e.stopPropagation()}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                save()
              } else if (e.key === 'Escape') {
                e.preventDefault()
                cancel()
              }
            }}
            onBlur={save}
          />
        ) : (
          <div className="cell-name">
            <span className="clip" style={{ fontWeight: 500 }}>{task.name}</span>
            <button
              className="btn icon ghost sm rename-btn"
              title="Rename task"
              aria-label="Rename task"
              onClick={(e) => {
                e.stopPropagation()
                setName(task.name)
                setEditing(true)
              }}
            >
              {action.isPending ? <Loader2 size={12} className="spin" /> : <Pencil size={12} />}
            </button>
          </div>
        )}
        <div className="mono faint clip" style={{ fontSize: 11 }}>
          {task.slug}{task.assignee ? ` · @${task.assignee}` : ''}
        </div>
      </td>
      <td>
        {parentName || childCount > 0 || forkedFrom || forkCount > 0 ? (
          <div className="cell-deps">
            {forkedFrom && (
              <span className="dep-chip fork" title={`Forked from ${forkedFrom}${task.fork_reason ? ` · ${task.fork_reason}` : ''}`}>
                <GitFork size={11} /> <span className="clip">{forkedFrom}</span>
              </span>
            )}
            {forkCount > 0 && (
              <span className="dep-chip fork" title={`Forked into ${task.forks?.map((f) => f.name).join(', ')}`}>
                <GitFork size={11} /> forks {forkCount}
              </span>
            )}
            {parentName && (
              <span className="dep-chip depends" title={`Depends on ${parentName}`}>
                <CornerLeftUp size={11} /> <span className="clip">{parentName}</span>
              </span>
            )}
            {childCount > 0 && (
              <span className="dep-chip blocks" title={`Blocks ${childCount} task${childCount === 1 ? '' : 's'}`}>
                <GitFork size={11} /> blocks {childCount}
              </span>
            )}
          </div>
        ) : (
          <span className="faint">—</span>
        )}
      </td>
      <td className="dim clip">{task.project_slug || <span className="faint">—</span>}</td>
      <td><span className={`prio ${task.priority}`}>{task.priority}</span></td>
      <td>
        {task.due_info ? (
          <span
            className={`badge ${dueTone(task.due_date, task.due_info)}`}
            style={{ whiteSpace: 'nowrap', height: 'auto', padding: '2px 7px', fontSize: 11 }}
            title={task.due_date ? `Due ${task.due_date}` : undefined}
          >
            {task.due_info}
          </span>
        ) : (
          <span className="faint">—</span>
        )}
      </td>
      <td><ProviderIcon provider={task.session_provider} size={14} /></td>
      <td>
        <div className="cell-tags">
          {(task.tags ?? []).slice(0, 3).map((tag) => <span key={tag} className="tag">{tag}</span>)}
        </div>
      </td>
      <td className="dim mono" style={{ textAlign: 'right', fontSize: 11.5 }}>{ago(task.updated_at)}</td>
      <td style={{ textAlign: 'right' }}>
        {task.archived_at ? (
          <button
            className="btn icon ghost sm row-action"
            title="Unarchive task"
            aria-label="Unarchive task"
            disabled={action.isPending}
            onClick={unarchive}
          >
            <ArchiveRestore size={13} />
          </button>
        ) : (
          <button
            className="btn icon ghost sm row-action"
            title="Archive task"
            aria-label="Archive task"
            disabled={action.isPending}
            onClick={archive}
          >
            <Archive size={13} />
          </button>
        )}
        <button
          className="btn icon ghost sm row-action"
          title="Move to trash"
          aria-label="Move to trash"
          disabled={action.isPending}
          onClick={trash}
        >
          <Trash2 size={13} />
        </button>
      </td>
    </tr>
  )
}
