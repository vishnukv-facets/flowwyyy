import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { useLocation } from 'wouter'
import { useQueryClient } from '@tanstack/react-query'
import { apiAction } from './api'
import { useUiData } from './query'
import type { FloatingSession } from './types'

// A session's own detail page lives at /session/<slug>. When you're on that
// page its inline terminal is the live surface, so the floating window for the
// same session must step aside (see the layer's unmount + the tray's chip
// filter) — never two attaches to one tmux pane.
const SESSION_ROUTE_PREFIX = '/session/'

// Popped-out task chips are client-side only, so without this they vanish on a
// hard refresh or server restart. We mirror the set to localStorage and
// rehydrate on load (then prune any whose session no longer exists — see the
// provider). Adhoc Ask Flow sessions persist server-side already.
const POPPED_TASKS_KEY = 'flow.tray.poppedTasks'

type PoppedTask = { provider: string; title: string; seq: number }

function loadPoppedTasks(): Record<string, PoppedTask> {
  if (typeof window === 'undefined') return {}
  try {
    const parsed = JSON.parse(window.localStorage.getItem(POPPED_TASKS_KEY) || '{}') as Record<string, PoppedTask>
    if (!parsed || typeof parsed !== 'object') return {}
    const out: Record<string, PoppedTask> = {}
    for (const [slug, t] of Object.entries(parsed)) {
      if (t && typeof t.title === 'string' && typeof t.provider === 'string') {
        out[slug] = { provider: t.provider, title: t.title, seq: Number(t.seq) || 0 }
      }
    }
    return out
  } catch {
    return {}
  }
}

// A floating window is either an adhoc Ask Flow terminal (kind 'floating',
// server-tracked, ✕ ENDS the session) or a popped-out regular task session
// (kind 'task', client-only, ✕ just CLOSES the window — the task keeps running
// and still lives in Live Sessions / its detail page).
export interface FloatingWindow {
  id: string
  kind: 'floating' | 'task'
  provider: string
  title: string
  running: boolean
  waiting: boolean
  waitingWhy?: string
}

// Per-window UI state, owned client-side. See notes inline.
//   everOpened — has the window been mounted this page-session? Reload-seeded
//                adhoc sessions start NOT-opened (tray chip only) so we don't
//                reconnect every PTY at once; restoring mounts the terminal.
//   minimized  — window hidden (display:none); its terminal body unmounts so a
//                minimized window does not keep a live websocket attached.
interface WinState {
  everOpened: boolean
  minimized: boolean
  pos: { x: number; y: number }
  z: number
}

interface OpenDescriptor {
  id: string
  provider: string
  title: string
}
interface PopOutDescriptor {
  slug: string
  provider: string
  title: string
}

interface FloatingTerminalsValue {
  windows: FloatingWindow[]
  winState: Record<string, WinState>
  open: (desc: OpenDescriptor) => void
  popOut: (task: PopOutDescriptor) => void
  restore: (id: string) => void
  minimize: (id: string) => void
  focus: (id: string) => void
  close: (id: string) => void
  move: (id: string, pos: { x: number; y: number }) => void
  isOpen: (id: string) => boolean
  // Slug of the session whose own /session/<slug> page is currently open, or
  // null. The layer unmounts this window and the tray hides its chip, so the
  // page's inline terminal is the sole attach while you're viewing it.
  activeSessionSlug: string | null
}

const FloatingTerminalsContext = createContext<FloatingTerminalsValue | null>(null)

// Stacking band for floating windows: strictly above the fullscreen session
// terminal (95), attention popups (96), and the tray dock (97), strictly below
// the modal scrim (100). See zTop/nextZ in the provider. First window → 98.
const FLOATING_Z_BASE = 97
const FLOATING_Z_MAX = 99

function basePosition(seq: number): { x: number; y: number } {
  if (typeof window === 'undefined') return { x: 48, y: 96 }
  const width = Math.min(720, window.innerWidth - 24)
  const baseX = Math.max(12, window.innerWidth - width - 24)
  const baseY = Math.max(72, Math.min(112, window.innerHeight - 480))
  const step = (seq % 6) * 28 // cascade so multiples don't stack pixel-perfect
  return { x: Math.max(12, baseX - step), y: baseY + step }
}

export function FloatingTerminalsProvider({ children }: { children: ReactNode }) {
  const { data: ui } = useUiData()
  const qc = useQueryClient()
  const [location] = useLocation()
  const activeSessionSlug = useMemo(
    () => (location.startsWith(SESSION_ROUTE_PREFIX) ? location.slice(SESSION_ROUTE_PREFIX.length).split('/')[0] || null : null),
    [location],
  )
  // Kept in a ref so callbacks (restore) can read the live route without being
  // re-created — and thus re-memoized — on every navigation.
  const activeRef = useRef(activeSessionSlug)
  activeRef.current = activeSessionSlug
  const serverSessions = useMemo<FloatingSession[]>(() => ui?.FLOATING_SESSIONS ?? [], [ui?.FLOATING_SESSIONS])
  // Agent waiting-status by slug, so popped-out task windows can show when their
  // session is awaiting input (adhoc sessions carry their own waiting flag).
  const agentBySlug = useMemo(() => {
    const m = new Map<string, { waiting: boolean; why?: string }>()
    for (const a of ui?.AGENTS ?? []) m.set(a.slug, { waiting: a.status === 'waiting', why: a.waiting_for?.why })
    return m
  }, [ui?.AGENTS])

  // Optimistic overlays for adhoc sessions, reconciled against the server list.
  const [pendingOpen, setPendingOpen] = useState<Record<string, FloatingSession>>({})
  const [pendingClose, setPendingClose] = useState<Record<string, true>>({})
  // Popped-out regular task sessions — purely client-side (the task already
  // exists server-side; popping out just floats a view of it).
  // Rehydrated from localStorage so popped-out chips survive a hard refresh /
  // server restart; pruned against the live agent list once ui-data loads.
  const [poppedTasks, setPoppedTasks] = useState<Record<string, PoppedTask>>(loadPoppedTasks)
  const [winState, setWinState] = useState<Record<string, WinState>>({})
  // Floating windows live in a band ABOVE the fullscreen session terminal
  // (.term-shell.fullscreen, z-index 95) and the attention popups (96) so they
  // surface even in full-view mode, but BELOW the modal/palette scrim (100) so
  // a ⌘K/⌘N dialog still covers them. zTop increments on focus to raise the
  // active window; nextZ() caps it inside the band so it can't climb over a
  // modal after many focus clicks.
  const zTop = useRef(FLOATING_Z_BASE)
  const nextZ = () => {
    zTop.current = Math.min(zTop.current + 1, FLOATING_Z_MAX)
    return zTop.current
  }
  const openSeq = useRef(0)
  const taskSeq = useRef(0)
  // Seed the sequence counter past any rehydrated chips (once, on first render)
  // so newly popped tasks sort after the restored ones.
  const seqSeeded = useRef(false)
  if (!seqSeeded.current) {
    seqSeeded.current = true
    taskSeq.current = Object.values(poppedTasks).reduce((max, t) => Math.max(max, t.seq + 1), 0)
  }

  // Mirror popped-out chips to localStorage so they survive reloads/restarts.
  useEffect(() => {
    if (typeof window === 'undefined') return
    try {
      window.localStorage.setItem(POPPED_TASKS_KEY, JSON.stringify(poppedTasks))
    } catch {
      // private mode / quota — the tray just won't persist this session.
    }
  }, [poppedTasks])

  // After a reload/restart, drop rehydrated chips whose session no longer
  // exists server-side (task finished or was removed). Gated on ui-data having
  // loaded so we never wipe chips before the agent list arrives. react-query
  // keeps prior data during refetch, so AGENTS won't transiently blink empty.
  useEffect(() => {
    if (!ui) return
    const known = new Set((ui.AGENTS ?? []).map((a) => a.slug))
    setPoppedTasks((prev) => {
      let changed = false
      const next: Record<string, PoppedTask> = {}
      for (const [slug, t] of Object.entries(prev)) {
        if (known.has(slug)) next[slug] = t
        else changed = true
      }
      return changed ? next : prev
    })
  }, [ui])

  useEffect(() => {
    const ids = new Set(serverSessions.map((s) => s.id))
    setPendingOpen((prev) => {
      let changed = false
      const next: Record<string, FloatingSession> = {}
      for (const [id, s] of Object.entries(prev)) {
        if (ids.has(id)) changed = true
        else next[id] = s
      }
      return changed ? next : prev
    })
    setPendingClose((prev) => {
      let changed = false
      const next: Record<string, true> = {}
      for (const id of Object.keys(prev)) {
        if (ids.has(id)) next[id] = true
        else changed = true
      }
      return changed ? next : prev
    })
  }, [serverSessions])

  const windows = useMemo<FloatingWindow[]>(() => {
    const byId = new Map<string, FloatingWindow>()
    const adhoc: FloatingSession[] = []
    for (const s of serverSessions) if (!pendingClose[s.id]) adhoc.push(s)
    for (const [id, s] of Object.entries(pendingOpen)) if (!serverSessions.some((x) => x.id === id)) adhoc.push(s)
    adhoc.sort((a, b) => (a.created_at < b.created_at ? -1 : a.created_at > b.created_at ? 1 : 0))
    for (const s of adhoc) {
      if (!byId.has(s.id)) {
        byId.set(s.id, { id: s.id, kind: 'floating', provider: s.provider, title: s.title, running: s.running, waiting: !!s.waiting, waitingWhy: s.waiting_why })
      }
    }
    const tasks = Object.entries(poppedTasks).sort((a, b) => a[1].seq - b[1].seq)
    for (const [slug, t] of tasks) {
      if (!byId.has(slug)) {
        const a = agentBySlug.get(slug)
        byId.set(slug, { id: slug, kind: 'task', provider: t.provider, title: t.title, running: true, waiting: !!a?.waiting, waitingWhy: a?.why })
      }
    }
    return [...byId.values()]
  }, [serverSessions, pendingOpen, pendingClose, poppedTasks, agentBySlug])

  const ensureWindow = useCallback((id: string, opened: boolean) => {
    setWinState((prev) => {
      if (prev[id]) {
        if (opened && (prev[id].minimized || !prev[id].everOpened)) {
          return { ...prev, [id]: { ...prev[id], everOpened: true, minimized: false, z: nextZ() } }
        }
        return prev
      }
      return {
        ...prev,
        [id]: { everOpened: opened, minimized: !opened, pos: basePosition(openSeq.current++), z: nextZ() },
      }
    })
  }, [])

  // Seed window slots for reload-listed adhoc sessions (minimized → tray chip
  // only, no mass reconnect).
  useEffect(() => {
    for (const s of serverSessions) ensureWindow(s.id, false)
  }, [serverSessions, ensureWindow])

  // Visiting a tray session's own page "expands it into the page": the layer
  // unmounts its floating window, so we mark it minimized here. That way, once
  // you navigate away the session returns to the tray as a chip (not as the
  // popped window it may have been before you opened its page).
  useEffect(() => {
    if (!activeSessionSlug) return
    setWinState((prev) => {
      const ws = prev[activeSessionSlug]
      if (!ws || ws.minimized) return prev
      return { ...prev, [activeSessionSlug]: { ...ws, minimized: true } }
    })
  }, [activeSessionSlug])

  const open = useCallback((desc: OpenDescriptor) => {
    setPendingOpen((prev) => ({
      ...prev,
      [desc.id]: { id: desc.id, provider: desc.provider, title: desc.title, running: false, created_at: new Date().toISOString() },
    }))
    setPendingClose((prev) => {
      if (!prev[desc.id]) return prev
      const next = { ...prev }
      delete next[desc.id]
      return next
    })
    ensureWindow(desc.id, true)
  }, [ensureWindow])

  const popOut = useCallback((task: PopOutDescriptor) => {
    setPoppedTasks((prev) =>
      prev[task.slug]
        ? prev
        : { ...prev, [task.slug]: { provider: task.provider, title: task.title, seq: taskSeq.current++ } },
    )
    ensureWindow(task.slug, true)
  }, [ensureWindow])

  const restore = useCallback((id: string) => {
    // On a session's own page the inline terminal is the surface — popping a
    // floating window for it would attach the same tmux pane twice.
    if (id === activeRef.current) return
    ensureWindow(id, true)
  }, [ensureWindow])

  const minimize = useCallback((id: string) => {
    setWinState((prev) => (prev[id] ? { ...prev, [id]: { ...prev[id], minimized: true } } : prev))
  }, [])

  const focus = useCallback((id: string) => {
    setWinState((prev) => (prev[id] ? { ...prev, [id]: { ...prev[id], z: nextZ() } } : prev))
  }, [])

  const move = useCallback((id: string, pos: { x: number; y: number }) => {
    setWinState((prev) => (prev[id] ? { ...prev, [id]: { ...prev[id], pos } } : prev))
  }, [])

  const dropWindow = useCallback((id: string) => {
    setWinState((prev) => {
      if (!prev[id]) return prev
      const next = { ...prev }
      delete next[id]
      return next
    })
  }, [])

  const close = useCallback(
    (id: string) => {
      // Popped-out task: just close the floating view — never kill the task.
      if (poppedTasks[id]) {
        setPoppedTasks((prev) => {
          if (!prev[id]) return prev
          const next = { ...prev }
          delete next[id]
          return next
        })
        dropWindow(id)
        return
      }
      // Adhoc Ask Flow session: end the backend PTY + forget the launch.
      setPendingClose((prev) => ({ ...prev, [id]: true }))
      setPendingOpen((prev) => {
        if (!prev[id]) return prev
        const next = { ...prev }
        delete next[id]
        return next
      })
      dropWindow(id)
      apiAction({ kind: 'close-floating-terminal', slug: id })
        .catch(() => {})
        .finally(() => qc.invalidateQueries({ queryKey: ['ui-data'] }))
    },
    [poppedTasks, dropWindow, qc],
  )

  const isOpen = useCallback(
    (id: string) => !!winState[id] && winState[id].everOpened && !winState[id].minimized,
    [winState],
  )

  const value = useMemo<FloatingTerminalsValue>(
    () => ({ windows, winState, open, popOut, restore, minimize, focus, close, move, isOpen, activeSessionSlug }),
    [windows, winState, open, popOut, restore, minimize, focus, close, move, isOpen, activeSessionSlug],
  )

  return <FloatingTerminalsContext.Provider value={value}>{children}</FloatingTerminalsContext.Provider>
}

export function useFloatingTerminals(): FloatingTerminalsValue {
  const ctx = useContext(FloatingTerminalsContext)
  if (!ctx) throw new Error('useFloatingTerminals must be used within FloatingTerminalsProvider')
  return ctx
}
