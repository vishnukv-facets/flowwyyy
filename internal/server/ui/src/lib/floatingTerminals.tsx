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
import { useQueryClient } from '@tanstack/react-query'
import { apiAction } from './api'
import { useUiData } from './query'
import type { FloatingSession } from './types'

// Per-window UI state, owned client-side. The SET of sessions is server-backed
// (UiData.FLOATING_SESSIONS); this layer tracks how each one is presented.
//
//   everOpened — has this session's window been mounted this page-session? On a
//                fresh reload, server sessions start NOT-opened so we show only
//                a tray chip and don't reconnect every PTY at once. Restoring
//                from the tray flips it true and mounts the terminal.
//   minimized  — window hidden (display:none) but kept mounted, so the live
//                terminal survives a minimize/restore without reconnecting.
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

interface FloatingTerminalsValue {
  sessions: FloatingSession[]
  windows: Record<string, WinState>
  open: (desc: OpenDescriptor) => void
  restore: (id: string) => void
  minimize: (id: string) => void
  focus: (id: string) => void
  close: (id: string) => void
  move: (id: string, pos: { x: number; y: number }) => void
}

const FloatingTerminalsContext = createContext<FloatingTerminalsValue | null>(null)

function basePosition(seq: number): { x: number; y: number } {
  if (typeof window === 'undefined') return { x: 48, y: 96 }
  const width = Math.min(720, window.innerWidth - 24)
  const baseX = Math.max(12, window.innerWidth - width - 24)
  const baseY = Math.max(72, Math.min(112, window.innerHeight - 480))
  // Cascade each newly-opened window so multiples don't stack pixel-perfect.
  const step = (seq % 6) * 28
  return { x: Math.max(12, baseX - step), y: baseY + step }
}

export function FloatingTerminalsProvider({ children }: { children: ReactNode }) {
  const { data: ui } = useUiData()
  const qc = useQueryClient()
  const serverSessions = useMemo<FloatingSession[]>(() => ui?.FLOATING_SESSIONS ?? [], [ui?.FLOATING_SESSIONS])

  // Optimistic overlays so open/close feel instant before the next ui-data
  // refetch reconciles with the server (the source of truth for existence).
  const [pendingOpen, setPendingOpen] = useState<Record<string, FloatingSession>>({})
  const [pendingClose, setPendingClose] = useState<Record<string, true>>({})
  const [windows, setWindows] = useState<Record<string, WinState>>({})
  // Base matches the floating-terminal stacking context (CSS z-index: 90) so
  // windows stay above page content; increments bring the focused one forward.
  const zTop = useRef(90)
  const openSeq = useRef(0)

  // Reconcile overlays against the server list: once the server confirms an
  // opened session, drop its optimistic entry; once it stops listing a closed
  // session, forget the close. Prevents the overlays from leaking.
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

  const sessions = useMemo<FloatingSession[]>(() => {
    const byId = new Map<string, FloatingSession>()
    for (const s of serverSessions) byId.set(s.id, s)
    for (const [id, s] of Object.entries(pendingOpen)) if (!byId.has(id)) byId.set(id, s)
    for (const id of Object.keys(pendingClose)) byId.delete(id)
    return [...byId.values()].sort((a, b) => (a.created_at < b.created_at ? -1 : a.created_at > b.created_at ? 1 : 0))
  }, [serverSessions, pendingOpen, pendingClose])

  const ensureWindow = useCallback((id: string, opened: boolean) => {
    setWindows((prev) => {
      if (prev[id]) {
        if (opened && (prev[id].minimized || !prev[id].everOpened)) {
          return { ...prev, [id]: { ...prev[id], everOpened: true, minimized: false, z: ++zTop.current } }
        }
        return prev
      }
      return {
        ...prev,
        [id]: { everOpened: opened, minimized: !opened, pos: basePosition(openSeq.current++), z: ++zTop.current },
      }
    })
  }, [])

  // Seed window slots for server-listed sessions on first sight (minimized, so a
  // reload shows tray chips without mounting/reconnecting every terminal).
  useEffect(() => {
    for (const s of serverSessions) ensureWindow(s.id, false)
  }, [serverSessions, ensureWindow])

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

  const restore = useCallback((id: string) => ensureWindow(id, true), [ensureWindow])

  const minimize = useCallback((id: string) => {
    setWindows((prev) => (prev[id] ? { ...prev, [id]: { ...prev[id], minimized: true } } : prev))
  }, [])

  const focus = useCallback((id: string) => {
    setWindows((prev) => (prev[id] ? { ...prev, [id]: { ...prev[id], z: ++zTop.current } } : prev))
  }, [])

  const move = useCallback((id: string, pos: { x: number; y: number }) => {
    setWindows((prev) => (prev[id] ? { ...prev, [id]: { ...prev[id], pos } } : prev))
  }, [])

  const close = useCallback(
    (id: string) => {
      setPendingClose((prev) => ({ ...prev, [id]: true }))
      setPendingOpen((prev) => {
        if (!prev[id]) return prev
        const next = { ...prev }
        delete next[id]
        return next
      })
      setWindows((prev) => {
        if (!prev[id]) return prev
        const next = { ...prev }
        delete next[id]
        return next
      })
      // End the backend PTY + forget the launch; refetch reconciles the list.
      apiAction({ kind: 'close-floating-terminal', slug: id })
        .catch(() => {})
        .finally(() => qc.invalidateQueries({ queryKey: ['ui-data'] }))
    },
    [qc],
  )

  const value = useMemo<FloatingTerminalsValue>(
    () => ({ sessions, windows, open, restore, minimize, focus, close, move }),
    [sessions, windows, open, restore, minimize, focus, close, move],
  )

  return <FloatingTerminalsContext.Provider value={value}>{children}</FloatingTerminalsContext.Provider>
}

export function useFloatingTerminals(): FloatingTerminalsValue {
  const ctx = useContext(FloatingTerminalsContext)
  if (!ctx) throw new Error('useFloatingTerminals must be used within FloatingTerminalsProvider')
  return ctx
}
