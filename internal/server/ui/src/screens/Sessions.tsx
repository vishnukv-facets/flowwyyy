import { useMemo, useState } from 'react'
import { Link } from 'wouter'
import { ChevronRight, FolderGit2, Repeat, TerminalSquare } from 'lucide-react'
import { useUiData } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { AgentCard } from '../components/AgentCard'
import { EmptyState, ErrorNote, Loading } from '../components/ui'
import type { UiAgent } from '../lib/types'

type Filter = 'all' | 'running' | 'waiting' | 'idle' | 'done'

const ADHOC = '__adhoc'
const PB_PREFIX = 'pb:'

export function Sessions() {
  useDocumentTitle('Sessions')
  const { data: ui, isLoading, error } = useUiData()
  const [filter, setFilter] = useState<Filter>('all')
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())

  const live = ui?.AGENTS ?? []
  const done = ui?.DONE_AGENTS ?? []

  const counts = {
    all: live.length,
    running: live.filter((a) => a.status === 'running').length,
    waiting: live.filter((a) => a.status === 'waiting').length,
    idle: live.filter((a) => a.status === 'idle' || a.status === 'stale').length,
    done: done.length,
  }

  let shown = live
  if (filter === 'running') shown = live.filter((a) => a.status === 'running')
  else if (filter === 'waiting') shown = live.filter((a) => a.status === 'waiting')
  else if (filter === 'idle') shown = live.filter((a) => a.status === 'idle' || a.status === 'stale')
  else if (filter === 'done') shown = done

  // Group playbook runs under their playbook; everything else by project;
  // null/empty project → Ad-hoc.
  const groups = useMemo(() => {
    const map = new Map<string, UiAgent[]>()
    for (const a of shown) {
      const key =
        a.kind === 'playbook_run' && a.playbook_slug
          ? PB_PREFIX + a.playbook_slug
          : a.project || ADHOC
      if (!map.has(key)) map.set(key, [])
      map.get(key)!.push(a)
    }
    const rank = (k: string) => (k === ADHOC ? 2 : k.startsWith(PB_PREFIX) ? 1 : 0)
    // Projects first (alphabetical), then playbooks, then Ad-hoc.
    return [...map.entries()].sort((a, b) => {
      const ra = rank(a[0])
      const rb = rank(b[0])
      return ra !== rb ? ra - rb : a[0].localeCompare(b[0])
    })
  }, [shown])

  const toggle = (key: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev)
      next.has(key) ? next.delete(key) : next.add(key)
      return next
    })

  const tabs: { id: Filter; label: string }[] = [
    { id: 'all', label: 'In flight' },
    { id: 'running', label: 'Running' },
    { id: 'waiting', label: 'Waiting' },
    { id: 'idle', label: 'Idle' },
    { id: 'done', label: 'Done' },
  ]

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

      {isLoading ? (
        <Loading label="sessions" />
      ) : error ? (
        <ErrorNote error={error} />
      ) : shown.length === 0 ? (
        <EmptyState
          icon={<TerminalSquare size={30} />}
          title="Nothing here"
          hint="Sessions appear once you start a task. Hit “New task” to launch one."
        />
      ) : (
        <div className="col" style={{ gap: 22 }}>
          {groups.map(([key, agents]) => {
            const isAdhoc = key === ADHOC
            const isPlaybook = key.startsWith(PB_PREFIX)
            const pbSlug = isPlaybook ? key.slice(PB_PREFIX.length) : ''
            const open = !collapsed.has(key)
            const runningN = agents.filter((a) => a.status === 'running').length
            return (
              <section key={key}>
                <div className="group-head" onClick={() => toggle(key)}>
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
                    {agents.map((a) => <AgentCard key={a.slug} agent={a} />)}
                  </div>
                )}
              </section>
            )
          })}
        </div>
      )}
    </div>
  )
}
