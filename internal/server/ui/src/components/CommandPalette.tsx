import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useLocation } from 'wouter'
import {
  Search,
  CornerDownLeft,
  Hash,
  FolderGit2,
  BookText,
  Repeat,
  FileText,
  Brain,
  Clock3,
  Zap,
  AlertTriangle,
  CircleDashed,
} from 'lucide-react'
import { useSearch, useUiData } from '../lib/query'
import type { SearchResult } from '../lib/types'
import { ProviderIcon, StatusDot } from './ui'
import { fromSeconds, fromMinutes } from '../lib/format'
import { getRecents, pushRecent, type RecentItem } from '../lib/recents'

// Search scope chips — narrow results to one kind. 'all' shows everything.
const SCOPES = [
  { id: 'all', label: 'All' },
  { id: 'tasks', label: 'Tasks' },
  { id: 'projects', label: 'Projects' },
  { id: 'playbooks', label: 'Playbooks' },
  { id: 'updates', label: 'Updates' },
  { id: 'memories', label: 'Memories' },
  { id: 'transcripts', label: 'Transcripts' },
] as const
type ScopeId = (typeof SCOPES)[number]['id']

interface Item {
  key: string
  label: string
  sub?: string
  to: string
  icon: ReactNode
  meta?: ReactNode
}
interface Group {
  label: string
  icon: ReactNode
  items: Item[]
}

const NAV: Item[] = [
  { key: 'nav-mc', label: 'Mission Control', to: '/', icon: <Hash size={15} /> },
  { key: 'nav-sessions', label: 'Sessions', to: '/sessions', icon: <Hash size={15} /> },
  { key: 'nav-tasks', label: 'Tasks', to: '/tasks', icon: <FileText size={15} /> },
  { key: 'nav-projects', label: 'Projects', to: '/projects', icon: <FolderGit2 size={15} /> },
  { key: 'nav-playbooks', label: 'Playbooks', to: '/playbooks', icon: <Repeat size={15} /> },
  { key: 'nav-inbox', label: 'Inbox', to: '/inbox', icon: <FileText size={15} /> },
  { key: 'nav-kb', label: 'Knowledge Base', to: '/kb', icon: <BookText size={15} /> },
  { key: 'nav-memories', label: 'Memories', to: '/memories', icon: <Brain size={15} /> },
]

function resultIcon(type: string) {
  if (type.startsWith('project')) return <FolderGit2 size={15} />
  if (type.startsWith('playbook')) return <Repeat size={15} />
  if (type === 'memory') return <Brain size={15} />
  if (type.includes('update') || type === 'transcript') return <FileText size={15} />
  return <Hash size={15} />
}

export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [, navigate] = useLocation()
  const [q, setQ] = useState('')
  const [active, setActive] = useState(0)
  const [scope, setScope] = useState<ScopeId>('all')
  const [recents, setRecents] = useState<RecentItem[]>([])
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const { data: search, isFetching: searchFetching } = useSearch(q, scope)
  const { data: ui } = useUiData()

  useEffect(() => {
    if (open) {
      setQ('')
      setActive(0)
      setScope('all')
      setRecents(getRecents())
      setTimeout(() => inputRef.current?.focus(), 10)
    }
  }, [open])

  // Esc closes the palette from anywhere (capture, so it beats other handlers).
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        onClose()
      }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [open, onClose])

  // Default (empty query): a smart, sectioned glimpse of the system.
  const defaultGroups = useMemo<Group[]>(() => {
    if (!ui) return []
    const agents = ui.AGENTS
    const agentItem = (a: (typeof agents)[number]): Item => ({
      key: `a-${a.slug}`,
      label: a.name,
      sub: `${a.project || 'ad-hoc'} · ${a.last_action || a.summary || a.slug}`,
      to: `/session/${a.slug}`,
      icon: <ProviderIcon provider={a.provider} size={15} />,
      meta: (
        <span className="palette-item-meta">
          <StatusDot status={a.status} /> {fromSeconds(a.last_activity_sec)}
        </span>
      ),
    })
    const waiting = agents.filter((a) => a.status === 'waiting')
    const running = agents.filter((a) => a.status === 'running')
    const idle = agents.filter((a) => a.status === 'idle' || a.status === 'stale')
    const groups: Group[] = []
    // Recently-opened (persisted) — what you were actually working on.
    if (recents.length)
      groups.push({
        label: 'Recent',
        icon: <Clock3 size={12} />,
        items: recents.map((r, i) => ({
          key: `r-${r.to}-${i}`,
          label: r.label,
          sub: r.sub,
          to: r.to,
          icon: resultIcon(r.type || ''),
        })),
      })
    if (waiting.length)
      groups.push({ label: 'Needs your attention', icon: <AlertTriangle size={12} />, items: waiting.map(agentItem) })
    if (running.length)
      groups.push({ label: 'Running', icon: <Zap size={12} />, items: running.slice(0, 8).map(agentItem) })
    if (idle.length)
      groups.push({ label: 'Idle', icon: <CircleDashed size={12} />, items: idle.slice(0, 8).map(agentItem) })
    groups.push({
      label: `Backlog · ${ui.BACKLOG.length}`,
      icon: <FileText size={12} />,
      items: ui.BACKLOG.map((t) => ({
        key: `b-${t.slug}`,
        label: t.name,
        sub: `${t.project} · ${fromMinutes(t.started_min)} old`,
        to: `/session/${t.slug}`,
        icon: <span className={`prio ${t.priority}`} />,
      })),
    })
    groups.push({ label: 'Go to', icon: <Hash size={12} />, items: NAV })
    return groups
  }, [ui, recents])

  const searchGroups = useMemo<Group[]>(() => {
    if (!search) return []
    const toItems = (arr: SearchResult[] | undefined): Item[] =>
      (arr ?? []).map((r, i) => ({
        key: `${r.type}-${r.slug}-${i}`,
        label: r.name || r.slug,
        sub: r.snippet || r.scope || r.type,
        to: r.url,
        icon: resultIcon(r.type),
      }))
    const groups: Group[] = []
    // A group shows only when the scope is "all" or matches that group's id.
    const push = (id: ScopeId, label: string, items: Item[]) => {
      if ((scope === 'all' || scope === id) && items.length)
        groups.push({ label, icon: <Hash size={12} />, items })
    }
    push('tasks', 'Tasks', toItems(search.tasks))
    push('projects', 'Projects', toItems(search.projects))
    push('playbooks', 'Playbooks', toItems(search.playbooks))
    push('updates', 'Updates', toItems(search.updates))
    push('memories', 'Memories', toItems(search.memories))
    push('transcripts', 'Transcripts', toItems(search.transcripts))
    return groups
  }, [search, scope])

  const groups = q.trim() ? searchGroups : defaultGroups
  const flat = useMemo(() => groups.flatMap((g) => g.items), [groups])

  useEffect(() => {
    if (active >= flat.length) setActive(0)
  }, [flat, active])

  useEffect(() => {
    const el = listRef.current?.querySelector('.palette-item.active')
    el?.scrollIntoView({ block: 'nearest' })
  }, [active])

  if (!open) return null

  const go = (it: Item, newTab = false) => {
    // ⌘/Ctrl held → open in a new browser tab instead of navigating in place
    // (mirrors the open-in-new-tab button on session cards).
    if (newTab && it.to) {
      window.open(it.to, '_blank', 'noopener,noreferrer')
      onClose()
      return
    }
    if (it.to && it.to !== '/') pushRecent({ label: it.label, sub: it.sub, to: it.to })
    onClose()
    navigate(it.to)
  }

  let idx = -1
  return (
    <div className="scrim palette-scrim" onMouseDown={onClose}>
      <div className="palette" onMouseDown={(e) => e.stopPropagation()}>
        <div className="palette-input">
          <Search size={17} className="dim" />
          <input
            ref={inputRef}
            className="palette-field"
            placeholder="Search tasks, projects, briefs, updates, memories…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'ArrowDown') {
                e.preventDefault()
                setActive((a) => Math.min(flat.length - 1, a + 1))
              } else if (e.key === 'ArrowUp') {
                e.preventDefault()
                setActive((a) => Math.max(0, a - 1))
              } else if (e.key === 'Enter' && flat[active]) {
                go(flat[active], e.metaKey || e.ctrlKey)
              }
            }}
          />
          <span className="kbd" title="Hold ⌘ (or Ctrl) and press Enter to open in a new tab">⌘↵ new tab</span>
          <span className="kbd">esc</span>
        </div>
        <div className="palette-scopes">
          {SCOPES.map((s) => (
            <button
                key={s.id}
                className={`palette-chip${scope === s.id ? ' active' : ''}`}
                onClick={() => {
                  setScope(s.id)
                  setActive(0)
                }}
              >
                {s.label}
              </button>
          ))}
        </div>
        <div className="palette-list" ref={listRef}>
          {flat.length === 0 && (
            <div className="palette-empty">{q.trim() ? (searchFetching ? 'Searching…' : 'No matches') : 'Loading…'}</div>
          )}
          {groups.map((g) => (
            <div key={g.label}>
              <div className="palette-section">
                {g.icon} {g.label}
              </div>
              {g.items.map((it) => {
                idx += 1
                const i = idx
                return (
                  <button
                    key={it.key}
                    className={`palette-item${i === active ? ' active' : ''}`}
                    onMouseEnter={() => setActive(i)}
                    onClick={(e) => go(it, e.metaKey || e.ctrlKey)}
                  >
                    <span className="palette-item-icon dim">{it.icon}</span>
                    <span className="col" style={{ minWidth: 0, flex: 1 }}>
                      <span className="clip">{it.label}</span>
                      {it.sub && <span className="faint clip" style={{ fontSize: 11.5 }}>{it.sub}</span>}
                    </span>
                    {it.meta}
                    {i === active && <CornerDownLeft size={13} className="faint" />}
                  </button>
                )
              })}
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}
