import { useMemo, useState } from 'react'
import { useLocation } from 'wouter'
import { Archive, FolderGit2, Plus, Search, Trash2 } from 'lucide-react'
import { useAction, useProjects } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { confirmAction } from '../lib/confirm'
import { CreateProjectModal } from '../components/modals'
import { EmptyState, ErrorNote, Loading } from '../components/ui'
import { clickable } from '../lib/a11y'

// Chat/Slack-derived task names often carry a trailing URL or permalink
// (e.g. "#channel - https://chat.google.com/room/…"). Strip that noise so
// the project tile reads cleanly; fall back to the raw name if empty.
function cleanTaskTitle(name: string): string {
  const cleaned = name
    .replace(/https?:\/\/\S+/g, '')
    .replace(/\s*[-–—·:|]\s*$/, '')
    .replace(/\s+/g, ' ')
    .trim()
  return cleaned || name
}

const SORTS = [
  { v: 'recent', label: 'Recent' },
  { v: 'name', label: 'Name' },
  { v: 'priority', label: 'Priority' },
  { v: 'active', label: 'Active' },
] as const
type SortKey = (typeof SORTS)[number]['v']
const PRIO_RANK: Record<string, number> = { high: 0, medium: 1, low: 2 }

export function Projects() {
  useDocumentTitle('Projects')
  const [, navigate] = useLocation()
  const [q, setQ] = useState('')
  const [sort, setSort] = useState<SortKey>('recent')
  const [showArchived, setShowArchived] = useState(false)
  const { data, isLoading, error } = useProjects({ include_archived: showArchived })
  const action = useAction()
  const [createOpen, setCreateOpen] = useState(false)

  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (data ?? [])
      .filter((p) => {
        if (!needle) return true
        return (
          p.name.toLowerCase().includes(needle) ||
          p.slug.toLowerCase().includes(needle) ||
          p.work_dir.toLowerCase().includes(needle)
        )
      })
      .slice()
      .sort((a, b) => {
        if (sort === 'name') return a.name.localeCompare(b.name)
        if (sort === 'priority') return (PRIO_RANK[a.priority] ?? 9) - (PRIO_RANK[b.priority] ?? 9)
        if (sort === 'active') return b.task_counts.in_progress - a.task_counts.in_progress
        return Date.parse(b.updated_at) - Date.parse(a.updated_at)
      })
  }, [data, q, sort])

  const archive = async (e: React.MouseEvent, slug: string, name: string) => {
    e.stopPropagation()
    const ok = await confirmAction({
      title: 'Archive this project?',
      body: `"${name}" and its tasks stay intact, but the project leaves your active list. You can unarchive it later.`,
      confirmLabel: 'Archive',
      danger: true,
    })
    if (ok) action.mutate({ kind: 'archive', target: slug, entity_kind: 'project' })
  }

  const trash = async (e: React.MouseEvent, slug: string, name: string) => {
    e.stopPropagation()
    const ok = await confirmAction({
      title: 'Move this project to trash?',
      body: `"${name}" will be soft-deleted and hidden from your lists. Its tasks and files remain, and you can restore it from Trash later.`,
      confirmLabel: 'Move to trash',
      danger: true,
    })
    if (ok) action.mutate({ kind: 'delete', target: slug, entity_kind: 'project' })
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">workspaces</div>
          <h1 className="h-xl">Projects</h1>
        </div>
        <div className="spacer" />
        <button className="btn" onClick={() => setCreateOpen(true)}>
          <Plus size={15} /> New project
        </button>
      </div>

      {!isLoading && !error && data && data.length > 0 && (
        <div className="row gap wrap" style={{ marginBottom: 18, gap: 14, alignItems: 'center' }}>
          <div className="input-icon" style={{ maxWidth: 280 }}>
            <Search size={14} className="dim" />
            <input
              className="input"
              placeholder="Filter by name, slug, or path…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          <div className="segmented">
            {SORTS.map((s) => (
              <button key={s.v} className={sort === s.v ? 'active' : ''} onClick={() => setSort(s.v)}>
                {s.label}
              </button>
            ))}
          </div>
          <div className="chips">
            <button
              className={`chip${showArchived ? ' active' : ''}`}
              aria-pressed={showArchived}
              onClick={() => setShowArchived((v) => !v)}
            >
              <Archive size={12} /> archived
            </button>
          </div>
          <div className="spacer" />
          <span className="faint mono" style={{ fontSize: 12 }}>
            {shown.length}
            {shown.length !== (data?.length ?? 0) ? ` / ${data?.length}` : ''}
          </span>
        </div>
      )}

      {isLoading ? (
        <Loading rows={4} />
      ) : error ? (
        <ErrorNote error={error} />
      ) : !data || data.length === 0 ? (
        <EmptyState icon={<FolderGit2 size={30} />} title="No projects yet" hint="Group related tasks and playbooks under a project." />
      ) : shown.length === 0 ? (
        <EmptyState icon={<FolderGit2 size={30} />} title="No projects match" hint="Adjust the filter or toggle archived." />
      ) : (
        <div className="grid cards stagger">
          {shown.map((p) => {
            const archived = !!p.archived_at
            return (
            <article
              key={p.slug}
              className={`card acard${archived ? ' archived' : ''}`}
              aria-label={`Open project ${p.name}`}
              {...clickable(() => navigate(`/project/${p.slug}`))}
            >
              <div className="acard-top">
                <FolderGit2 size={17} className="dim" style={{ marginTop: 1 }} />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div className="acard-title clip">{p.name}</div>
                  <div className="acard-ref clip">{p.work_dir}</div>
                </div>
                {archived && <span className="tag">archived</span>}
                <span className={`prio ${p.priority}`}>{p.priority}</span>
                {!archived && (
                  <>
                    <button
                      className="btn icon ghost sm row-action"
                      title="Archive project"
                      aria-label="Archive project"
                      onClick={(e) => archive(e, p.slug, p.name)}
                    >
                      <Archive size={14} />
                    </button>
                    <button
                      className="btn icon ghost sm row-action"
                      title="Move to trash"
                      aria-label="Move project to trash"
                      onClick={(e) => trash(e, p.slug, p.name)}
                    >
                      <Trash2 size={14} />
                    </button>
                  </>
                )}
              </div>
              <div className="row gap" style={{ gap: 18, fontSize: 12.5 }}>
                <span className="num"><b>{p.task_counts.in_progress}</b> <span className="faint">active</span></span>
                <span className="num"><b>{p.task_counts.backlog}</b> <span className="faint">queued</span></span>
                <span className="num"><b>{p.task_counts.done}</b> <span className="faint">done</span></span>
              </div>
              {p.recent_tasks?.length > 0 && (
                <div className="acard-recent">
                  {p.recent_tasks.slice(0, 3).map((t) => (
                    <div key={t.slug} className="acard-recent-row" title={t.name}>
                      <span className={`prio ${t.priority}`} />
                      <span className="clip">{cleanTaskTitle(t.name)}</span>
                    </div>
                  ))}
                </div>
              )}
            </article>
            )
          })}
        </div>
      )}
      <CreateProjectModal open={createOpen} onClose={() => setCreateOpen(false)} />
    </div>
  )
}
