import { useState } from 'react'
import { ChevronDown, X } from 'lucide-react'
import { ProviderIcon } from './ui'
import { FloatingTerminalWindow } from './FloatingTerminalWindow'
import { AttentionWidget } from './AttentionWidget'
import { useFloatingTerminals } from '../lib/floatingTerminals'

// Remember the collapsed/expanded choice across reloads.
const TRAY_COLLAPSED_KEY = 'flow.tray.collapsed'

// FloatingTerminalLayer mounts the persistent floating-session UI — the windows
// plus the tray dock. It lives in Shell (outside the routed Switch) so
// navigating between pages never unmounts an open window.
export function FloatingTerminalLayer() {
  const { windows, winState, minimize, close, focus, move, activeSessionSlug } = useFloatingTerminals()
  return (
    <>
      {windows.map((w) => {
        // You're on this session's own page — let its inline page terminal be
        // the live surface and UNMOUNT the floating one (closing its socket),
        // so the same tmux pane is never attached twice.
        if (w.kind === 'task' && w.id === activeSessionSlug) return null
        const ws = winState[w.id]
        if (!ws || !ws.everOpened) return null
        return (
          <FloatingTerminalWindow
            key={w.id}
            win={w}
            pos={ws.pos}
            z={ws.z}
            hidden={ws.minimized}
            onMove={(p) => move(w.id, p)}
            onFocus={() => focus(w.id)}
            onMinimize={() => minimize(w.id)}
            onClose={() => close(w.id)}
          />
        )
      })}
      <FloatingTerminalTray />
      <AttentionWidget />
    </>
  )
}

// Bottom-right dock of floating session windows — adhoc Ask Flow terminals plus
// any regular session you've popped out. Universal (every page) and survives
// navigation/reload. Click a chip to restore + focus; ✕ ends an adhoc session
// or just closes a popped-out task's window (the task keeps running).
function FloatingTerminalTray() {
  const { windows, restore, close, isOpen, activeSessionSlug } = useFloatingTerminals()
  const [collapsed, setCollapsed] = useState(
    () => typeof window !== 'undefined' && window.localStorage.getItem(TRAY_COLLAPSED_KEY) === '1',
  )
  // The session you're currently viewing has "expanded into its page", so it
  // drops out of the tray until you navigate away (then it returns as a chip).
  const chips = windows.filter((w) => !(w.kind === 'task' && w.id === activeSessionSlug))
  if (chips.length === 0) return null

  const toggle = () =>
    setCollapsed((c) => {
      const next = !c
      try {
        window.localStorage.setItem(TRAY_COLLAPSED_KEY, next ? '1' : '0')
      } catch {
        // localStorage can be unavailable (private mode); collapse still works
        // for the session, just won't persist.
      }
      return next
    })

  return (
    <div className={`adhoc-tray${collapsed ? ' collapsed' : ''}`} role="group" aria-label="Floating sessions">
      <button
        type="button"
        className="adhoc-tray-toggle"
        onClick={toggle}
        aria-expanded={!collapsed}
        title={collapsed ? 'Show sessions' : 'Hide sessions'}
      >
        <span className="eyebrow">Sessions</span>
        <span className="adhoc-tray-count">{chips.length}</span>
        <ChevronDown size={14} className="adhoc-tray-caret" />
      </button>
      {/* grid 1fr→0fr animates to the real list height with no JS measuring */}
      <div className="adhoc-tray-list">
        <div className="adhoc-tray-list-inner">
          {chips.map((w) => {
            const shown = isOpen(w.id)
            return (
              <div key={w.id} className={`adhoc-chip${shown ? ' active' : ''}`}>
                <button
                  type="button"
                  className="adhoc-chip-main"
                  title={shown ? 'Focus session' : 'Restore session'}
                  onClick={() => restore(w.id)}
                >
                  <span className={`dot ${w.running ? 'running' : 'idle'}`} />
                  <ProviderIcon provider={w.provider} size={13} />
                  <span className="clip">{w.title || 'Ask Flow'}</span>
                </button>
                <button
                  type="button"
                  className="btn icon sm"
                  title={w.kind === 'task' ? 'Close window (session keeps running)' : 'End session'}
                  onClick={() => close(w.id)}
                >
                  <X size={14} />
                </button>
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}
