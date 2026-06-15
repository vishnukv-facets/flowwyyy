import { useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { Link, useLocation } from 'wouter'
import { FlowMark } from './FlowMark'
import { ClaudeRunner } from './ClaudeMascot'
import { useMascotPrefs } from '../lib/mascot'
import {
  AlertTriangle,
  Bell,
  BookText,
  Bot,
  Brain,
  CheckCircle2,
  Database,
  FolderGit2,
  HardDrive,
  Inbox,
  LayoutDashboard,
  ListTodo,
  MessagesSquare,
  Moon,
  Network,
  Plug,
  Plus,
  Radar,
  RefreshCw,
  Repeat,
  Search,
  Settings,
  Sun,
  TerminalSquare,
  Trash2,
} from 'lucide-react'
import { rpc, type ConnStatus } from '../lib/rpc'
import { getTheme, toggleTheme, type Theme } from '../lib/theme'
import { useAttention, useInbox, useUiData } from '../lib/query'
import { ago } from '../lib/format'
import { pushToast } from '../lib/toast'
import { apiAction } from '../lib/api'
import { confirmAction } from '../lib/confirm'
import { SourceIcon } from './ui'
import { CommandPalette } from './CommandPalette'
import { AskFlow } from './AskFlow'
import { CreateTaskModal } from './modals'
import { Toaster } from './Toaster'
import { ConfirmHost } from './ConfirmHost'
import { FloatingTerminalLayer } from './FloatingTerminalTray'
import type { FlowDBDocStat, FlowDBInfo } from '../lib/types'

interface NavDef {
  to: string
  label: string
  icon: ReactNode
  match: (p: string) => boolean
  badge?: number
  tone?: string
}

type NotificationItem = { key: string; title: string; sub: string }
type BrowserNotificationPermission = NotificationPermission | 'unsupported'

function currentBrowserNotificationPermission(): BrowserNotificationPermission {
  if (typeof window === 'undefined' || !('Notification' in window)) return 'unsupported'
  return Notification.permission
}

// Pops a toast the moment a new notification arrives (a session starts waiting,
// or an unread inbox message lands) — so you notice without watching the bell.
// Seeds on first load so pre-existing items don't toast on page open.
function useNotificationToasts(
  ui?: ReturnType<typeof useUiData>['data'],
  inbox?: ReturnType<typeof useInbox>['data'],
  attention?: ReturnType<typeof useAttention>['data'],
) {
  const seen = useRef<Set<string> | null>(null)
  const notifyDesktop = useCallback((it: NotificationItem) => {
    // Fire a desktop alert whenever flow isn't the focused window — i.e. you're
    // working in another app (the whole point of an alert). The old gate used
    // !document.hidden, which is true only when the flow TAB is backgrounded or
    // minimized; with the tab simply sitting behind your IDE/terminal it skipped
    // the notification, so "I enabled alerts but get nothing" was by design.
    if (currentBrowserNotificationPermission() !== 'granted' || typeof document === 'undefined' || document.hasFocus()) return
    try {
      new Notification(`flow: ${it.title}`, { body: it.sub, tag: it.key })
    } catch {
      // Browser notification availability can change under us; keep in-app
      // toasts as the reliable fallback.
    }
  }, [])
  useEffect(() => {
    const items: NotificationItem[] = []
    for (const a of ui?.AGENTS ?? []) {
      if (a.status === 'waiting') {
        items.push({ key: `w:${a.slug}`, title: a.name, sub: a.waiting_for?.why || 'Awaiting your input' })
      }
    }
    for (const e of inbox?.entries ?? []) {
      if (e.unread) {
        items.push({ key: `u:${e.task_slug}:${e.timestamp}`, title: e.task_name, sub: e.body_snippet || 'New message' })
      }
    }
    for (const a of attention ?? []) {
      if (a.status === 'new' && a.urgency === 'urgent') {
        items.push({ key: `a:${a.id}`, title: a.summary || 'Attention', sub: a.reason || a.suggested_action })
      }
    }
    if (seen.current === null) {
      // first observation — remember what's already there, don't toast it
      seen.current = new Set(items.map((i) => i.key))
      return
    }
    for (const it of items) {
      if (!seen.current.has(it.key)) {
        seen.current.add(it.key)
        pushToast('info', `${it.title} — ${it.sub}`)
        notifyDesktop(it)
      }
    }
  }, [ui, inbox, attention, notifyDesktop])
}

export function Shell({ children }: { children: ReactNode }) {
  const [loc] = useLocation()
  const [theme, setTheme] = useState<Theme>(getTheme())
  const [conn, setConn] = useState<ConnStatus>(rpc.getStatus())
  const [paletteOpen, setPaletteOpen] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const { data: ui } = useUiData()
  const { data: inbox } = useInbox()
  const { data: attentionItems } = useAttention('new')
  const attentionCount = attentionItems?.length ?? 0
  useNotificationToasts(ui, inbox, attentionItems)

  useEffect(() => rpc.onStatus(setConn), [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey
      if (mod && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setPaletteOpen((o) => !o)
      } else if (mod && e.key.toLowerCase() === 'n' && !createOpen) {
        e.preventDefault()
        setCreateOpen(true)
      } else if (e.key === '/' && document.activeElement === document.body) {
        e.preventDefault()
        setPaletteOpen(true)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [createOpen])

  const running = (ui?.AGENTS ?? []).filter((a) => a.status === 'running').length
  const waiting = (ui?.AGENTS ?? []).filter((a) => a.status === 'waiting').length
  const monitored = (ui?.AGENTS ?? []).filter((a) => a.monitored).length
  const backlog = ui?.BACKLOG?.length ?? 0
  const unread = inbox?.unread_count ?? 0
  const mascotPrefs = useMascotPrefs()

  const groups: { label: string; items: NavDef[] }[] = [
    {
      label: 'Workspace',
      items: [
        { to: '/', label: 'Mission Control', icon: <LayoutDashboard size={16} />, match: (p) => p === '/' },
        {
          to: '/sessions',
          label: 'Sessions',
          icon: <TerminalSquare size={16} />,
          match: (p) => p === '/sessions' || p.startsWith('/session/'),
          badge: running || undefined,
          tone: 'var(--ok)',
        },
        { to: '/tasks', label: 'Tasks', icon: <ListTodo size={16} />, match: (p) => p === '/tasks', badge: backlog || undefined },
        { to: '/owners', label: 'Owners', icon: <Bot size={16} />, match: (p) => p === '/owners' },
        { to: '/graph', label: 'Graph', icon: <Network size={16} />, match: (p) => p === '/graph' || p === '/brain' },
        {
          to: '/inbox',
          label: 'Inbox',
          icon: <Inbox size={16} />,
          match: (p) => p === '/inbox',
          badge: unread || undefined,
          tone: 'var(--accent-hi)',
        },
        { to: '/chats', label: 'Chats', icon: <MessagesSquare size={16} />, match: (p) => p === '/chats' },
        {
          to: '/attention',
          label: 'Attention',
          icon: <Bell size={16} />,
          match: (p) => p === '/attention',
          badge: attentionCount || undefined,
          tone: 'var(--warn)',
        },
      ],
    },
    {
      label: 'Library',
      items: [
        { to: '/projects', label: 'Projects', icon: <FolderGit2 size={16} />, match: (p) => p === '/projects' || p.startsWith('/project/') },
        { to: '/playbooks', label: 'Playbooks', icon: <Repeat size={16} />, match: (p) => p === '/playbooks' || p.startsWith('/playbook/') },
        { to: '/kb', label: 'Knowledge', icon: <BookText size={16} />, match: (p) => p === '/kb' },
        { to: '/memories', label: 'Memories', icon: <Brain size={16} />, match: (p) => p === '/memories' },
      ],
    },
    {
      label: 'System',
      items: [
        { to: '/workdirs', label: 'Workdirs', icon: <HardDrive size={16} />, match: (p) => p === '/workdirs' },
        { to: '/connectors', label: 'Connectors', icon: <Plug size={16} />, match: (p) => p === '/connectors' },
        { to: '/settings', label: 'Settings', icon: <Settings size={16} />, match: (p) => p === '/settings' },
        { to: '/trash', label: 'Trash', icon: <Trash2 size={16} />, match: (p) => p === '/trash' },
      ],
    },
  ]
  const allNav = groups.flatMap((g) => g.items)
  const title = allNav.find((n) => n.match(loc))?.label ?? 'flowwyyy'
  const connLabel = conn === 'open' ? 'live' : conn === 'connecting' ? 'connecting' : 'offline'

  return (
    <div className="shell">
      <aside className="rail">
        <Link href="/" className="rail-brand">
          <FlowMark size={23} />
          <span className="rail-wordmark">
            flowwyyy<span className="accent">.</span>
          </span>
        </Link>
        <nav className="rail-nav">
          {groups.map((g) => (
            <div className="rail-group" key={g.label}>
              <div className="rail-group-label">{g.label}</div>
              {g.items.map((n) => (
                <Link key={n.to} href={n.to} className={`rail-item${n.match(loc) ? ' active' : ''}`}>
                  <span className="rail-item-icon">{n.icon}</span>
                  <span className="rail-item-label">{n.label}</span>
                  {n.badge ? (
                    <span className="rail-badge" style={n.tone ? { color: n.tone, borderColor: 'currentColor' } : undefined}>
                      {n.badge}
                    </span>
                  ) : null}
                </Link>
              ))}
            </div>
          ))}
        </nav>
        {mascotPrefs.enabled && <ClaudeRunner conn={conn} running={running} monitored={monitored} inbox={unread} />}
        <div className="rail-foot">
          {waiting > 0 && (
            <Link href="/sessions" className="rail-stat warn">
              <span className="dot waiting" /> {waiting} awaiting input
            </Link>
          )}
          {monitored > 0 && (
            <div className="rail-stat">
              <Radar size={13} /> {monitored} monitored
            </div>
          )}
          <div className="rail-foot-bar">
            <StoragePopover db={ui?.FLOWDB} activeSessions={(ui?.AGENTS ?? []).length} />
            <div className="spacer" />
            <button
              type="button"
              className="btn icon ghost sm theme-toggle"
              onClick={(e) => {
                // Center of the button is the origin of the circular theme wipe.
                const r = e.currentTarget.getBoundingClientRect()
                setTheme(toggleTheme({ x: r.left + r.width / 2, y: r.top + r.height / 2 }))
              }}
              aria-label="Toggle theme"
              title="Toggle theme"
            >
              {/* key={theme} remounts the icon each swap so the spin-in plays. */}
              <span key={theme} className="theme-toggle-icon">
                {theme === 'dark' ? <Sun size={15} /> : <Moon size={15} />}
              </span>
            </button>
          </div>
        </div>
      </aside>

      <div className="main">
        <header className="topbar">
          <div className="topbar-title">
            <h1 className="topbar-h">{title}</h1>
          </div>
          <div className="spacer" />
          <button type="button" className="searchbtn" onClick={() => setPaletteOpen(true)}>
            <Search size={15} className="dim" />
            <span className="dim">Search…</span>
            <span className="kbd">⌘K</span>
          </button>
          <div className={`conn conn-${conn}`} title={`data channel: ${connLabel}`}>
            <span className="dot" />
            <span className="conn-label">{connLabel}</span>
          </div>
          <NotificationsBell />
          <AskFlow />
          <button type="button" className="btn primary" onClick={() => setCreateOpen(true)}>
            <Plus size={16} /> New task
          </button>
        </header>
        <main className="content">{children}</main>
      </div>

      <CommandPalette
        open={paletteOpen}
        onClose={() => setPaletteOpen(false)}
        onNewTask={() => setCreateOpen(true)}
        onToggleTheme={() => setTheme(toggleTheme())}
      />
      <CreateTaskModal open={createOpen} onClose={() => setCreateOpen(false)} />
      <FloatingTerminalLayer />
      <Toaster />
      <ConfirmHost />
    </div>
  )
}

function StoragePopover({ db, activeSessions }: { db?: FlowDBInfo; activeSessions: number }) {
  const qc = useQueryClient()
  const [busy, setBusy] = useState(false)
  const canCompact = !!db?.exists && !!db.can_compact && !busy
  const runCompact = async (e: { currentTarget: EventTarget & HTMLElement }) => {
    e.currentTarget.closest('details')?.removeAttribute('open')
    if (!db?.exists || !db.can_compact) return
    const ok = await confirmAction({
      title: 'Compact flow.db?',
      body: `SQLite will rewrite flow.db with VACUUM to reclaim ${db.reclaimable_human_size}. It does not delete briefs, updates, transcript files, or search results. Compacting is safest when sessions are idle; ${activeSessions} session${activeSessions === 1 ? '' : 's'} are currently listed in Mission Control.`,
      confirmLabel: 'Compact',
      cancelLabel: 'Cancel',
    })
    if (!ok) return
    setBusy(true)
    try {
      const resp = await apiAction({ kind: 'compact-db' }, 180000)
      pushToast('ok', resp.message || 'Database compacted')
      await qc.invalidateQueries({ queryKey: ['ui-data'] })
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : 'Compact failed')
    } finally {
      setBusy(false)
    }
  }

  const topDocs = (db?.documents ?? []).slice(0, 4)
  const topObjects = (db?.objects ?? []).slice(0, 7)
  const quickOk = db?.quick_check === 'ok'
  const quickFromCompact = db?.quick_check_source === 'compact-precheck'
  const quickHealthLabel = quickOk && quickFromCompact ? 'ok · compact' : db?.quick_check || 'unknown'
  const showQuickNote = !!db?.quick_check_note && db.quick_check_source !== 'live'

  return (
    <details className="menu storage-menu">
      <summary className="rail-stat faint storage-trigger" title="Database storage">
        <Database size={12} />
        <span>{db?.exists ? db.human_size : '—'}</span>
      </summary>
      <div className="menu-pop storage-pop">
        <div className="storage-head">
          <div>
            <div className="storage-title">flow.db</div>
            <div className="storage-path clip">{db?.display_path || db?.path || 'not initialized'}</div>
          </div>
          {db?.exists && (
            <span className={`storage-health${quickOk ? ' ok' : ' warn'}`} title={db.quick_check_note || undefined}>
              {quickOk ? <CheckCircle2 size={13} /> : <AlertTriangle size={13} />}
              {quickHealthLabel}
            </span>
          )}
        </div>

        {!db?.exists ? (
          <div className="storage-empty">Database file is not available yet.</div>
        ) : (
          <>
            <div className="storage-meter" aria-label="Database active and reclaimable storage">
              <span
                className="storage-meter-used"
                style={{ flexGrow: Math.max(db.used_bytes, 1) }}
              />
              <span
                className="storage-meter-free"
                style={{ flexGrow: Math.max(db.reclaimable_bytes, 0) }}
              />
            </div>
            <div className="storage-grid">
              <StorageMetric label="On disk" value={db.human_size} />
              <StorageMetric label="Active" value={db.used_human_size || '—'} />
              <StorageMetric label="Reclaimable" value={db.reclaimable_human_size || '0 B'} tone={db.reclaimable_bytes > 0 ? 'warn' : undefined} />
              <StorageMetric label="Pages free" value={`${db.free_page_count || 0}`} mono />
            </div>
            {activeSessions > 0 && (
              <div className="storage-note warn">
                <AlertTriangle size={13} />
                Active sessions are listed; compact when idle if possible.
              </div>
            )}
            {showQuickNote && (
              <div className={`storage-note${quickOk ? ' ok' : ' warn'}`}>
                {quickOk ? <CheckCircle2 size={13} /> : <AlertTriangle size={13} />}
                {db.quick_check_note}
              </div>
            )}
            <div className="storage-note">{db.explanation}</div>
            {db.error && <div className="storage-note warn">{db.error}</div>}

            {topDocs.length > 0 && (
              <div className="storage-section">
                <div className="storage-section-title">Search text</div>
                {topDocs.map((doc) => (
                  <div className="storage-row" key={`${doc.scope}:${doc.entity_type}`}>
                    <span>{docLabel(doc)}</span>
                    <span className="mono">{doc.human_size}</span>
                  </div>
                ))}
              </div>
            )}

            {topObjects.length > 0 && (
              <div className="storage-section">
                <div className="storage-section-title">Largest objects</div>
                {topObjects.map((obj) => (
                  <div className="storage-row" key={obj.name}>
                    <span className="clip">{obj.name}</span>
                    <span className="mono">{obj.human_size}</span>
                  </div>
                ))}
              </div>
            )}

            <button
              type="button"
              className="btn storage-compact"
              disabled={!canCompact}
              onClick={runCompact}
              title={db.can_compact ? 'Run SQLite VACUUM' : 'No free-list pages to reclaim'}
            >
              <RefreshCw size={14} className={busy ? 'spin' : ''} />
              {busy ? 'Compacting…' : db.can_compact ? 'Compact' : 'Already compact'}
            </button>
          </>
        )}
      </div>
    </details>
  )
}

function StorageMetric({ label, value, tone, mono = false }: { label: string; value: string; tone?: 'warn'; mono?: boolean }) {
  return (
    <div className={`storage-metric${tone ? ` ${tone}` : ''}`}>
      <span>{label}</span>
      <strong className={mono ? 'mono' : undefined}>{value}</strong>
    </div>
  )
}

function docLabel(doc: FlowDBDocStat): string {
  if (doc.scope === 'transcript') return `Transcripts · ${doc.count}`
  if (doc.scope === 'memory') return `Memories · ${doc.count}`
  if (doc.scope === 'update') return `Updates · ${doc.count}`
  if (doc.scope === 'brief') return `Briefs · ${doc.count}`
  return `${doc.scope} · ${doc.count}`
}

// Notification bell — surfaces what needs you: sessions awaiting input and
// unread inbox messages. Badge = total; the panel navigates to each.
function NotificationsBell() {
  const [, navigate] = useLocation()
  const { data: ui } = useUiData()
  const { data: inbox } = useInbox()
  const [desktopPermission, setDesktopPermission] = useState<BrowserNotificationPermission>(currentBrowserNotificationPermission)
  const waiting = (ui?.AGENTS ?? []).filter((a) => a.status === 'waiting')
  const unread = (inbox?.entries ?? []).filter((e) => e.unread).slice(0, 10)
  const count = waiting.length + unread.length

  useEffect(() => {
    const onDown = (e: globalThis.MouseEvent) => {
      document.querySelectorAll('details.menu[open]').forEach((d) => {
        if (!d.contains(e.target as Node)) d.removeAttribute('open')
      })
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [])

  const close = (e: { currentTarget: EventTarget & HTMLElement }) =>
    e.currentTarget.closest('details')?.removeAttribute('open')
  const enableDesktop = async () => {
    if (typeof window === 'undefined' || !('Notification' in window)) {
      setDesktopPermission('unsupported')
      return
    }
    const next = await Notification.requestPermission()
    setDesktopPermission(next)
  }

  return (
    <details className="menu">
      <summary className="btn icon ghost notif-trigger" title="Notifications" aria-label="Notifications">
        <Bell size={16} />
        {count > 0 && <span className="notif-badge">{count > 99 ? '99+' : count}</span>}
      </summary>
      <div className="menu-pop right notif-pop">
        <div className="notif-head">
          Notifications {count > 0 && <span className="faint">· {count}</span>}
        </div>
        {desktopPermission === 'default' && (
          <button type="button" className="notif-permission" onClick={enableDesktop}>
            Enable desktop alerts
          </button>
        )}
        {desktopPermission === 'denied' && (
          <div className="notif-permission muted">Desktop alerts blocked in browser settings.</div>
        )}
        {count === 0 && <div className="notif-empty">You're all caught up.</div>}
        {waiting.map((a) => (
          <button type="button" key={`w-${a.slug}`} className="notif-item" onClick={(e) => { close(e); navigate(`/session/${a.slug}`) }}>
            <span className="dot waiting" style={{ marginTop: 4 }} />
            <div className="lrow-main">
              <div className="notif-title clip">{a.name}</div>
              <div className="notif-sub clip">{a.waiting_for?.why || 'Awaiting your input'}</div>
            </div>
          </button>
        ))}
        {unread.map((m, i) => (
          <button type="button" key={`u-${m.task_slug}-${i}`} className="notif-item" onClick={(e) => { close(e); navigate('/inbox') }}>
            <span style={{ marginTop: 2 }}><SourceIcon source={m.source} size={13} /></span>
            <div className="lrow-main">
              <div className="notif-title clip">{m.task_name}</div>
              <div className="notif-sub clip">{m.body_snippet || 'New message'}</div>
            </div>
            <span className="faint mono" style={{ fontSize: 10 }}>{ago(m.timestamp)}</span>
          </button>
        ))}
      </div>
    </details>
  )
}
