import { X } from 'lucide-react'
import { ProviderIcon } from './ui'
import { FloatingTerminalWindow } from './FloatingTerminalWindow'
import { useFloatingTerminals } from '../lib/floatingTerminals'

// FloatingTerminalLayer mounts the persistent adhoc-session UI — the floating
// windows plus the tray dock. It lives in Shell (outside the routed Switch) so
// navigating between pages never unmounts an open Ask Flow terminal.
export function FloatingTerminalLayer() {
  const { sessions, windows, minimize, close, focus, move } = useFloatingTerminals()
  return (
    <>
      {sessions.map((s) => {
        const w = windows[s.id]
        if (!w || !w.everOpened) return null
        return (
          <FloatingTerminalWindow
            key={s.id}
            session={s}
            pos={w.pos}
            z={w.z}
            hidden={w.minimized}
            onMove={(p) => move(s.id, p)}
            onFocus={() => focus(s.id)}
            onMinimize={() => minimize(s.id)}
            onClose={() => close(s.id)}
          />
        )
      })}
      <FloatingTerminalTray />
    </>
  )
}

// Bottom-right dock listing every adhoc Ask Flow session. Survives navigation
// and reload because the session set is server-backed. Click a chip to restore
// (and focus) its window; ✕ ends the underlying session.
export function FloatingTerminalTray() {
  const { sessions, windows, restore, close } = useFloatingTerminals()
  if (sessions.length === 0) return null
  return (
    <div className="adhoc-tray" role="group" aria-label="Ask Flow sessions">
      <div className="adhoc-tray-label eyebrow">Ask Flow</div>
      {sessions.map((s) => {
        const w = windows[s.id]
        const shown = !!w && w.everOpened && !w.minimized
        return (
          <div key={s.id} className={`adhoc-chip${shown ? ' active' : ''}`}>
            <button
              type="button"
              className="adhoc-chip-main"
              title={shown ? 'Focus session' : 'Restore session'}
              onClick={() => restore(s.id)}
            >
              <span className={`dot ${s.running ? 'running' : 'idle'}`} />
              <ProviderIcon provider={s.provider} size={13} />
              <span className="clip">{s.title || 'Ask Flow'}</span>
            </button>
            <button type="button" className="btn icon sm" title="End session" onClick={() => close(s.id)}>
              <X size={14} />
            </button>
          </div>
        )
      })}
    </div>
  )
}
