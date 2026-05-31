import { useEffect, useRef } from 'react'
import { Terminal as WTerminal, type TerminalHandle } from '@wterm/react'
import '@wterm/dom/css'

// Live PTY terminal powered by wterm (DOM renderer, Zig/WASM core) bound to the
// flow /ws/terminal JSON protocol: server pushes {type:"output"|"status"|
// "error"}, client sends {type:"input"|"resize"}. Output that arrives before
// the WASM core is ready is buffered and flushed on onReady.

interface Props {
  slug: string
  restartKey?: number
  onStatus?: (kind: 'status' | 'error' | 'closed' | 'open', message: string) => void
}

// wterm 0.3.0's lightweight Zig core mis-renders SGR 2 (faint/dim) as a solid
// green background — and agent TUIs (Codex especially) emit faint constantly,
// which paints the whole grid green. Strip the faint intensity param from SGR
// sequences before writing. Color and other attributes are preserved; the
// numeric args of 38/48 extended colors are skipped so a color *index* of 2
// (e.g. 38;5;2 = green text) is never confused with the faint attribute.
function sanitizeSGR(data: string): string {
  // eslint-disable-next-line no-control-regex
  return data.replace(/\x1b\[([0-9;]*)m/g, (full, params: string) => {
    if (!params) return full
    const parts = params.split(';')
    const out: string[] = []
    for (let i = 0; i < parts.length; i++) {
      const p = parts[i]
      if (p === '38' || p === '48') {
        out.push(p)
        const mode = parts[i + 1]
        if (mode === '5') {
          out.push('5')
          if (parts[i + 2] !== undefined) out.push(parts[i + 2])
          i += 2
        } else if (mode === '2') {
          out.push('2')
          for (let k = 2; k <= 4; k++) if (parts[i + k] !== undefined) out.push(parts[i + k])
          i += 4
        }
        continue
      }
      if (p === '2') continue // drop faint
      out.push(p)
    }
    return out.length ? `\x1b[${out.join(';')}m` : ''
  })
}

function termWsURL(slug: string, cols: number, rows: number): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${location.host}/ws/terminal?slug=${encodeURIComponent(
    slug,
  )}&cols=${cols}&rows=${rows}`
}

export function TaskTerminal({ slug, restartKey = 0, onStatus }: Props) {
  const term = useRef<TerminalHandle | null>(null)
  const ws = useRef<WebSocket | null>(null)
  const ready = useRef(false)
  const buffer = useRef<string[]>([])
  const size = useRef({ cols: 120, rows: 32 })
  const resizeTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    let closed = false
    ready.current = false
    buffer.current = []

    const send = (obj: unknown) => {
      const s = ws.current
      if (s && s.readyState === WebSocket.OPEN) s.send(JSON.stringify(obj))
    }

    const sock = new WebSocket(termWsURL(slug, size.current.cols, size.current.rows))
    ws.current = sock
    sock.onopen = () => onStatus?.('open', 'connected')
    sock.onmessage = (ev) => {
      let m: { type: string; data?: string; message?: string }
      try {
        m = JSON.parse(ev.data as string)
      } catch {
        return
      }
      if (m.type === 'output' && m.data != null) {
        const data = sanitizeSGR(m.data)
        if (ready.current && term.current) term.current.write(data)
        else buffer.current.push(data)
      } else if (m.type === 'status') {
        onStatus?.('status', m.message ?? '')
      } else if (m.type === 'error') {
        onStatus?.('error', m.message ?? 'terminal error')
      }
    }
    sock.onclose = () => {
      if (!closed) onStatus?.('closed', 'terminal disconnected')
    }

    // Bind keystroke + resize forwarding once the handle resolves.
    bound.current = { send }

    return () => {
      closed = true
      try {
        sock.close()
      } catch {
        /* noop */
      }
      ws.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [slug, restartKey])

  const bound = useRef<{ send: (o: unknown) => void }>({ send: () => {} })

  useEffect(() => () => {
    if (resizeTimer.current) clearTimeout(resizeTimer.current)
  }, [])

  // The terminal is intentionally always dark — a dark panel that blends into
  // the light theme (like an IDE terminal). We deliberately do NOT switch the
  // wterm theme prop on app theme change: toggling it re-renders the WASM grid,
  // which broke scroll and mangled the column layout. Dark-in-both is pinned in
  // CSS (.flow-term .wterm --term-bg/--term-fg).
  return (
    <div className="flow-term">
      <WTerminal
        ref={term}
        wasmUrl="/wterm.wasm"
        autoResize
        cursorBlink
        onReady={() => {
          ready.current = true
          if (term.current) {
            for (const d of buffer.current) term.current.write(d)
            buffer.current = []
            term.current.focus()
          }
        }}
        onData={(d) => bound.current.send({ type: 'input', data: d })}
        onResize={(cols, rows) => {
          size.current = { cols, rows }
          // Debounce the PTY resize: autoResize's ResizeObserver fires many
          // times while a fullscreen/side toggle or window drag is in flight.
          // Sending each one makes the TUI (tmux/Claude) repaint repeatedly and
          // stack duplicate frames. Coalesce to a single resize at the settled
          // size so it repaints once, cleanly.
          if (resizeTimer.current) clearTimeout(resizeTimer.current)
          resizeTimer.current = setTimeout(() => {
            bound.current.send({ type: 'resize', cols: size.current.cols, rows: size.current.rows })
          }, 130)
        }}
      />
    </div>
  )
}
