import { useMemo, useState } from 'react'
import { Link } from 'wouter'
import { Archive, CheckCircle2, ChevronRight, FolderGit2, Pause, Repeat, Search, TerminalSquare, X } from 'lucide-react'
import { useUiData, queryClient } from '../lib/query'
import { apiAction } from '../lib/api'
import { pushToast } from '../lib/toast'
import { confirmAction } from '../lib/confirm'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { AgentCard } from '../components/AgentCard'
import { EmptyState, ErrorNote, Loading } from '../components/ui'
import { clickable } from '../lib/a11y'
import type { UiAgent } from '../lib/types'

type Filter = 'all' | 'running' | 'waiting' | 'idle' | 'done'

const ADHOC = '__adhoc'
const PB_PREFIX = 'pb:'

const SORTS = [
  { v: 'activity', label: 'Activity' },
  { v: 'tokens', label: 'Tokens' },
  { v: 'priority', label: 'Priority' },
  { v: 'diff', label: 'Diff' },
] as const
type SortKey = (typeof SORTS)[number]['v']
const PRIO_RANK: Record<string, number> = { high: 0, medium: 1, low: 2 }

// Sort agents within a group. Activity = most-recently-active first; tokens =
// highest context fill first; priority = high→low; diff = largest change first.
function sortAgents(list: UiAgent[], sort: SortKey): UiAgent[] {
  return list.slice().sort((a, b) => {
    switch (sort) {
      case 'tokens': {
        const fa = a.tokens_max ? a.tokens_used / a.tokens_max : 0
        const fb = b.tokens_max ? b.tokens_used / b.tokens_max : 0
        return fb - fa
      }
      case 'priority':
        return (PRIO_RANK[a.priority] ?? 9) - (PRIO_RANK[b.priority] ?? 9)
      case 'diff':
        return b.diff.add + b.diff.rem - (a.diff.add + a.diff.rem)
      case 'activity':
      default:
        return a.last_activity_sec - b.last_activity_sec
    }
  })
}

export function Sessions() {
  useDocumentTitle('Sessions')
  const { data: ui, isLoading, error } = useUiData()
  const [filter, setFilter] = useState<Filter>('all')
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const [q, setQ] = useState('')
  const [projectFilter, setProjectFilter] = useState('')
  const [providerFilter, setProviderFilter] = useState('')
  const [sort, setSort] = useState<SortKey>('activity')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [bulkPending, setBulkPending] = useState(false)

  const live = ui?.AGENTS ?? []
  const done = ui?.DONE_AGENTS ?? []

  const counts = {
    all: live.length,
    running: live.filter((a) => a.status === 'running').length,
    waiting: live.filter((a) => a.status === 'waiting').length,
    idle: live.filter((a) => a.status === 'idle' || a.status === 'stale').length,
    done: done.length,
  }

  // Filter chips draw their options from the whole fleet so toggling one option
  // never makes the others disappear.
  const allAgents = useMemo(() => [...live, ...done], [live, done])
  const projectOpts = useMemo(
    () => Array.from(new Set(allAgents.map((a) => a.project || '').filter(Boolean))).sort(),
    [allAgents],
  )
  const providerOpts = useMemo(
    () => Array.from(new Set(allAgents.map((a) => a.provider).filter(Boolean))).sort(),
    [allAgents],
  )

  const filtered = useMemo(() => {
    let base: UiAgent[]
    if (filter === 'done') base = done
    else if (filter === 'running') base = live.filter((a) => a.status === 'running')
    else if (filter === 'waiting') base = live.filter((a) => a.status === 'waiting')
    else if (filter === 'idle') base = live.filter((a) => a.status === 'idle' || a.status === 'stale')
    else base = live
    const needle = q.trim().toLowerCase()
    return base.filter((a) => {
      if (projectFilter && (a.project || '') !== projectFilter) return false
      if (providerFilter && a.provider !== providerFilter) return false
      if (!needle) return true
      return [a.name, a.slug, a.project || '', a.last_action || '', a.summary || ''].some((s) =>
        s.toLowerCase().includes(needle),
      )
    })
  }, [filter, live, done, q, projectFilter, providerFilter])

  // Group playbook runs under their playbook; everything else by project;
  // null/empty project → Ad-hoc. Agents within a group are sorted by `sort`.
  const groups = useMemo(() => {
    const map = new Map<string, UiAgent[]>()
    for (const a of filtered) {
      const key =
        a.kind === 'playbook_run' && a.playbook_slug
          ? PB_PREFIX + a.playbook_slug
          : a.project || ADHOC
      if (!map.has(key)) map.set(key, [])
      map.get(key)!.push(a)
    }
    for (const [key, arr] of map) map.set(key, sortAgents(arr, sort))
    const rank = (k: string) => (k === ADHOC ? 2 : k.startsWith(PB_PREFIX) ? 1 : 0)
    // Projects first (alphabetical), then playbooks, then Ad-hoc.
    return [...map.entries()].sort((a, b) => {
      const ra = rank(a[0])
      const rb = rank(b[0])
      return ra !== rb ? ra - rb : a[0].localeCompare(b[0])
    })
  }, [filtered, sort])

  const toggle = (key: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev)
      next.has(key) ? next.delete(key) : next.add(key)
      return next
    })

  const toggleSel = (slug: string) =>
    setSelected((prev) => {
      const next = new Set(prev)
      next.has(slug) ? next.delete(slug) : next.add(slug)
      return next
    })

  const visibleSlugs = useMemo(() => filtered.map((a) => a.slug), [filtered])
  const allVisibleSelected = visibleSlugs.length > 0 && visibleSlugs.every((s) => selected.has(s))
  const toggleSelectAll = () =>
    setSelected((prev) => {
      if (allVisibleSelected) {
        const next = new Set(prev)
        visibleSlugs.forEach((s) => next.delete(s))
        return next
      }
      return new Set([...prev, ...visibleSlugs])
    })

  // Bulk = iterate the existing single-target action per slug behind one
  // confirm, then a single toast + invalidation (instead of N noisy toasts).
  const runBulk = async (kind: string, verb: string) => {
    const slugs = [...selected]
    if (!slugs.length) return
    const ok = await confirmAction({
      title: `${verb} ${slugs.length} session${slugs.length === 1 ? '' : 's'}?`,
      body: `This runs "${verb.toLowerCase()}" on each selected session, one at a time.`,
      confirmLabel: verb,
      danger: kind !== 'pause',
    })
    if (!ok) return
    setBulkPending(true)
    const results = await Promise.allSettled(slugs.map((s) => apiAction({ kind, target: s })))
    setBulkPending(false)
    const failed = results.filter((r) => r.status === 'rejected').length
    setSelected(new Set())
    queryClient.invalidateQueries()
    if (failed) pushToast('error', `${verb}: ${failed}/${slugs.length} failed`)
    else pushToast('ok', `${verb.toLowerCase()} ${slugs.length} session${slugs.length === 1 ? '' : 's'}`)
  }

  const tabs: { id: Filter; label: string }[] = [
    { id: 'all', label: 'In flight' },
    { id: 'running', label: 'Running' },
    { id: 'waiting', label: 'Waiting' },
    { id: 'idle', label: 'Idle' },
    { id: 'done', label: 'Done' },
  ]

  const hasAgents = filter === 'done' ? done.length > 0 : live.length > 0

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">agent sessions</div>
          <h1 className="h-xl">Sessions</h1>
        </div>
        <div className="spacer" />
        <div className="segmented">
          {tabs.map((t) => (
            <button key={t.id} className={filter === t.id ? 'active' : ''} onClick={() => setFilter(t.id)}>
              {t.label} <span className="faint mono">{counts[t.id]}</span>
            </button>
          ))}
        </div>
      </div>

      {hasAgents && (
        <div className="row gap wrap" style={{ marginBottom: 18, gap: 14, alignItems: 'center' }}>
          <div className="input-icon" style={{ maxWidth: 240 }}>
            <Search size={14} className="dim" />
            <input
              className="input"
              placeholder="Filter sessions…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          {projectOpts.length > 1 && (
            <div className="chips">
              <button className={`chip${projectFilter === '' ? ' active' : ''}`} onClick={() => setProjectFilter('')}>
                all projects
              </button>
              {projectOpts.map((p) => (
                <button
                  key={p}
                  className={`chip${projectFilter === p ? ' active' : ''}`}
                  onClick={() => setProjectFilter((cur) => (cur === p ? '' : p))}
                >
                  {p}
                </button>
              ))}
            </div>
          )}
          {providerOpts.length > 1 && (
            <div className="chips">
              {providerOpts.map((p) => (
                <button
                  key={p}
                  className={`chip${providerFilter === p ? ' active' : ''}`}
                  onClick={() => setProviderFilter((cur) => (cur === p ? '' : p))}
                >
                  {p}
                </button>
              ))}
            </div>
          )}
          <div className="segmented">
            {SORTS.map((s) => (
              <button key={s.v} className={sort === s.v ? 'active' : ''} onClick={() => setSort(s.v)}>
                {s.label}
              </button>
            ))}
          </div>
          <div className="spacer" />
          {visibleSlugs.length > 0 && (
            <div className="chips">
              <button className={`chip${allVisibleSelected ? ' active' : ''}`} aria-pressed={allVisibleSelected} onClick={toggleSelectAll}>
                {allVisibleSelected ? 'Deselect all' : 'Select all'}
              </button>
            </div>
          )}
        </div>
      )}

      {isLoading ? (
        <Loading label="sessions" />
      ) : error ? (
        <ErrorNote error={error} />
      ) : filtered.length === 0 ? (
        <EmptyState
          icon={<TerminalSquare size={30} />}
          title={hasAgents ? 'No sessions match' : 'Nothing here'}
          hint={hasAgents ? 'Adjust the filters above.' : 'Sessions appear once you start a task. Hit “New task” to launch one.'}
        />
      ) : (
        <div className="col" style={{ gap: 22, paddingBottom: selected.size > 0 ? 72 : 0 }}>
          {groups.map(([key, agents]) => {
            const isAdhoc = key === ADHOC
            const isPlaybook = key.startsWith(PB_PREFIX)
            const pbSlug = isPlaybook ? key.slice(PB_PREFIX.length) : ''
            const open = !collapsed.has(key)
            const runningN = agents.filter((a) => a.status === 'running').length
            return (
              <section key={key}>
                <div className="group-head" aria-expanded={open} {...clickable(() => toggle(key))}>
                  <ChevronRight size={15} className={`group-caret${open ? ' open' : ''}`} />
                  {isAdhoc ? (
                    <span className="dot idle" />
                  ) : isPlaybook ? (
                    <Repeat size={14} className="dim" />
                  ) : (
                    <FolderGit2 size={14} className="dim" />
                  )}
                  <span className="group-title">{isAdhoc ? 'Ad-hoc' : isPlaybook ? pbSlug : key}</span>
                  {isPlaybook && <span className="tag">playbook</span>}
                  <span className="section-count">{agents.length}</span>
                  {runningN > 0 && <span className="badge ok"><span className="dot running" />{runningN} running</span>}
                  <div className="spacer" />
                  {isPlaybook ? (
                    <Link href={`/playbook/${pbSlug}`} className="btn ghost sm" onClick={(e) => e.stopPropagation()}>
                      Playbook
                    </Link>
                  ) : !isAdhoc ? (
                    <Link href={`/project/${key}`} className="btn ghost sm" onClick={(e) => e.stopPropagation()}>
                      Project
                    </Link>
                  ) : null}
                </div>
                {open && (
                  <div className="grid cards stagger" style={{ marginTop: 12 }}>
                    {agents.map((a) => (
                      <AgentCard
                        key={a.slug}
                        agent={a}
                        selectable
                        selected={selected.has(a.slug)}
                        onToggle={() => toggleSel(a.slug)}
                      />
                    ))}
                  </div>
                )}
              </section>
            )
          })}
        </div>
      )}

      {selected.size > 0 && (
        <div className="bulk-bar" role="toolbar" aria-label="Bulk actions">
          <span className="bulk-count">{selected.size} selected</span>
          <div className="spacer" />
          <button className="btn ghost sm" disabled={bulkPending} onClick={() => runBulk('pause', 'Pause')}>
            <Pause size={13} /> Pause
          </button>
          <button className="btn ghost sm" disabled={bulkPending} onClick={() => runBulk('done', 'Done')}>
            <CheckCircle2 size={13} /> Done
          </button>
          <button className="btn ghost sm" disabled={bulkPending} onClick={() => runBulk('archive', 'Archive')}>
            <Archive size={13} /> Archive
          </button>
          <button className="btn icon ghost sm" title="Clear selection" aria-label="Clear selection" onClick={() => setSelected(new Set())}>
            <X size={14} />
          </button>
        </div>
      )}
    </div>
  )
}
