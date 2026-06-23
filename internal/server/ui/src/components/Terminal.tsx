import { useCallback, useEffect, useRef, useState } from 'react'
import { Terminal as XTerm, type IDisposable, type ITheme } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { ArrowDownToLine } from 'lucide-react'
import '@xterm/xterm/css/xterm.css'
import { pushToast } from '../lib/toast'
import { uploadTerminalAttachments } from '../lib/api'
import { authToken } from '../lib/devicetoken'

// Live PTY terminal powered by xterm.js, bound to flow's terminal JSON
// protocol: server pushes {type:"output"|"status"|"error"}, client sends
// {type:"input"|"resize"}. Sessions run inside tmux, so on (re)attach the
// server resizes the pane to our URL cols/rows and replays the full pane
// history — which is why we FIT BEFORE CONNECTING: connecting at the real
// measured size makes that replay render at the right width (no rewrap/overlap)
// before native tmux mouse scroll/copy-mode takes over user navigation.
//
// NATIVE TERMINAL MODEL. tmux owns the pane and repaints it to our PTY. The
// browser xterm is the terminal emulator attached to the same tmux session, so
// mouse wheel and drag selection are passed through to tmux copy-mode instead
// of being simulated with browser-local scrollback.
//
// This keeps the stable xterm pieces: FitAddon + Unicode11Addon, OSC 52 →
// system clipboard, image paste/drop attachment support, and DA-response
// stripping on input.

const DEFAULT_TERMINAL_SCROLLBACK_LINES = 1_000_000
const MAX_TERMINAL_SCROLLBACK_LINES = 1_000_000

function terminalScrollbackLines(): number {
  const env = (import.meta as unknown as { env?: Record<string, string | undefined> }).env
  const raw = env?.VITE_FLOW_TERMINAL_SCROLLBACK_LINES
  if (!raw) return DEFAULT_TERMINAL_SCROLLBACK_LINES
  const n = Number.parseInt(raw, 10)
  if (!Number.isFinite(n) || n < 10_000) return DEFAULT_TERMINAL_SCROLLBACK_LINES
  return Math.min(n, MAX_TERMINAL_SCROLLBACK_LINES)
}

const TERMINAL_SCROLLBACK_LINES = terminalScrollbackLines()

// Re-fit at these millisecond offsets after mount / socket-open / font load.
// Layout (flex, fullscreen toggle, font swap) settles asynchronously; a single
// fit at t=0 measures a not-yet-final box. Fitting again at increasing delays
// converges on the correct grid without spamming resizes forever.
const TERMINAL_FIT_DELAYS_MS = [0, 40, 160, 420, 900]
const TERMINAL_RECONNECT_INITIAL_MS = 400
const TERMINAL_RECONNECT_MAX_MS = 8000

// Device-Attributes responses the terminal *emits* in reply to DA queries
// (ESC [ ? … c / ESC [ > … c). If the host echoes them back to us as input we
// must drop them, or they get sent to the PTY as bogus keystrokes.
// eslint-disable-next-line no-control-regex
const TERMINAL_GENERATED_INPUT_RE = /\x1b\[(?:\?[0-9;]*|>[0-9;]*)c/g
const stripTerminalGeneratedInput = (data = ''): string => data.replace(TERMINAL_GENERATED_INPUT_RE, '')

const TERMINAL_FONT =
  "'JetBrains Mono', 'JetBrainsMono Nerd Font', 'FiraCode Nerd Font', ui-monospace, 'SF Mono', Menlo, Monaco, monospace"

// Pinned dark palette — the terminal stays dark in every app theme (IDE
// convention; a dark panel reads as intentional in light mode). Background
// matches .flow-term so there's no seam between chrome and grid.
const TERMINAL_THEME: ITheme = {
  background: '#0a0b0e',
  foreground: '#d7d4cd',
  cursor: '#5eead4',
  cursorAccent: '#0a0b0e',
  selectionBackground: '#3f3a87',
  black: '#0a0b0e',
  red: '#e25757',
  green: '#2eb672',
  yellow: '#d6a84c',
  blue: '#645df6',
  magenta: '#b584ff',
  cyan: '#5eead4',
  white: '#d8d8e8',
  brightBlack: '#57576a',
  brightRed: '#ff7b7b',
  brightGreen: '#63d797',
  brightYellow: '#f1ca73',
  brightBlue: '#8b87f8',
  brightMagenta: '#cfaaff',
  brightCyan: '#8ff7e8',
  brightWhite: '#ffffff',
}

interface Props {
  slug: string
  kind?: 'task' | 'floating'
  restartKey?: number
  // Carried for callers (SessionDetail / FloatingTerminalWindow) and future use;
  // the native tmux model treats every provider the same.
  provider?: string
  onStatus?: (kind: 'status' | 'error' | 'closed' | 'open', message: string) => void
}

function termWsURL(slug: string, cols: number, rows: number, kind: 'task' | 'floating'): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const key = kind === 'floating' ? 'id' : 'slug'
  const path = kind === 'floating' ? '/ws/floating-terminal' : '/ws/terminal'
  // The token gate on the WS handshake (audit P0-1) reads ?token=; browsers
  // can't set custom headers on a WebSocket. See lib/wsurl.ts.
  const tok = authToken()
  const tokenParam = tok ? `&token=${encodeURIComponent(tok)}` : ''
  return `${proto}//${location.host}${path}?${key}=${encodeURIComponent(slug)}&cols=${cols}&rows=${rows}${tokenParam}`
}

// ---- Accessory key row (mobile only) ------------------------------------
// Shown at ≤640px. Sends control sequences through the same `send` function
// the keyboard uses. Ctrl is sticky: tap it, then tap a printable key to
// send the control char (e.g. Ctrl+C → \x03).

interface AccessoryKeyRowProps {
  sendData: (data: string) => void
}

function AccessoryKeyRow({ sendData }: AccessoryKeyRowProps) {
  const [ctrlActive, setCtrlActive] = useState(false)

  const tap = useCallback(
    (seq: string) => {
      sendData(seq)
    },
    [sendData],
  )

  // When Ctrl is sticky-active and a printable key is tapped, send the
  // control character (charCode & 0x1f) and clear the modifier.
  const tapWithCtrl = useCallback(
    (char: string) => {
      if (ctrlActive) {
        const code = char.toUpperCase().charCodeAt(0) & 0x1f
        sendData(String.fromCharCode(code))
        setCtrlActive(false)
      } else {
        sendData(char)
      }
    },
    [ctrlActive, sendData],
  )

  const handleCtrl = useCallback(() => {
    setCtrlActive((v) => !v)
  }, [])

  return (
    <div className="term-accessory-row" aria-label="Terminal accessory keys">
      <button
        type="button"
        className="term-acc-key"
        aria-label="Escape"
        onPointerDown={(e) => {
          e.preventDefault()
          tap('\x1b')
          setCtrlActive(false)
        }}
      >
        Esc
      </button>
      <button
        type="button"
        className="term-acc-key"
        aria-label="Tab"
        onPointerDown={(e) => {
          e.preventDefault()
          tap('\t')
          setCtrlActive(false)
        }}
      >
        Tab
      </button>
      <button
        type="button"
        className={`term-acc-key term-acc-ctrl${ctrlActive ? ' term-acc-ctrl--active' : ''}`}
        aria-label="Control modifier"
        aria-pressed={ctrlActive}
        onPointerDown={(e) => {
          e.preventDefault()
          handleCtrl()
        }}
      >
        Ctrl
      </button>
      <button
        type="button"
        className="term-acc-key"
        aria-label="Arrow up"
        onPointerDown={(e) => {
          e.preventDefault()
          tap('\x1b[A')
          setCtrlActive(false)
        }}
      >
        ↑
      </button>
      <button
        type="button"
        className="term-acc-key"
        aria-label="Arrow down"
        onPointerDown={(e) => {
          e.preventDefault()
          tap('\x1b[B')
          setCtrlActive(false)
        }}
      >
        ↓
      </button>
      <button
        type="button"
        className="term-acc-key"
        aria-label="Arrow left"
        onPointerDown={(e) => {
          e.preventDefault()
          tap('\x1b[D')
          setCtrlActive(false)
        }}
      >
        ←
      </button>
      <button
        type="button"
        className="term-acc-key"
        aria-label="Arrow right"
        onPointerDown={(e) => {
          e.preventDefault()
          tap('\x1b[C')
          setCtrlActive(false)
        }}
      >
        →
      </button>
      <button
        type="button"
        className="term-acc-key"
        aria-label="Slash"
        onPointerDown={(e) => {
          e.preventDefault()
          tapWithCtrl('/')
        }}
      >
        /
      </button>
      <button
        type="button"
        className="term-acc-key"
        aria-label="Pipe"
        onPointerDown={(e) => {
          e.preventDefault()
          tapWithCtrl('|')
        }}
      >
        |
      </button>
    </div>
  )
}

export function TaskTerminal({ slug, kind = 'task', restartKey = 0, onStatus }: Props) {
  const hostRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const jumpToBottomRef = useRef<(() => void) | null>(null)
  const onStatusRef = useRef<Props['onStatus']>(onStatus)
  onStatusRef.current = onStatus
  const terminalInstanceKey = `${kind}:${slug}:${restartKey}`
  const [bottomJumpState, setBottomJumpState] = useState({ key: terminalInstanceKey, visible: false })
  if (bottomJumpState.key !== terminalInstanceKey) {
    setBottomJumpState({ key: terminalInstanceKey, visible: false })
  }
  const showBottomJump = bottomJumpState.key === terminalInstanceKey && bottomJumpState.visible

  // Ref to the PTY send function — populated inside the effect so AccessoryKeyRow
  // can call it without needing to be inside the effect closure.
  const sendRef = useRef<((data: string) => void) | null>(null)
  const sendData = useCallback((data: string) => {
    sendRef.current?.(data)
  }, [])

  useEffect(() => {
    const host = hostRef.current
    if (!host) return
    host.innerHTML = ''
    jumpToBottomRef.current = null

    const term = new XTerm({
      cols: 120,
      rows: 32,
      allowProposedApi: true, // required for unicode11 + parser.registerOscHandler
      allowTransparency: true,
      altClickMovesCursor: true,
      customGlyphs: true,
      cursorBlink: true,
      convertEol: false,
      drawBoldTextInBrightColors: true,
      fontFamily: TERMINAL_FONT,
      fontSize: 12.5,
      letterSpacing: 0,
      lineHeight: 1.18,
      macOptionIsMeta: true,
      minimumContrastRatio: 1,
      rescaleOverlappingGlyphs: true,
      rightClickSelectsWord: true,
      scrollOnUserInput: true,
      scrollSensitivity: 1,
      scrollback: TERMINAL_SCROLLBACK_LINES,
      smoothScrollDuration: 0,
      theme: TERMINAL_THEME,
    })

    const unicode = new Unicode11Addon()
    term.loadAddon(unicode)
    term.unicode.activeVersion = '11'
    const fit = new FitAddon()
    term.loadAddon(fit)

    let destroyed = false
    let ws: WebSocket | null = null
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null
    let reconnectBackoff = TERMINAL_RECONNECT_INITIAL_MS
    let lastSize = ''
    let sawFirstOutput = false // first output frame is the history replay
    // Gate the FIRST socket connect until the terminal font has settled. If we
    // connect with fallback metrics and then refit wider once JetBrains Mono
    // loads, the grid width changes → we resize the tmux pane → tmux freezes a
    // stale-width copy of the live input box into its history (it never reflows),
    // and that stale copy replays as a DUPLICATE footer on the next attach. Only
    // connecting once the width is final means no post-open resize, no stale copy.
    let fontReadyDone = false
    // Follow-tail. While true, every drained write and every (re)fit re-pins the
    // viewport to the bottom so the live input box + footer stay visible. We drop
    // follow the moment the user scrolls up to read history, and restore it when
    // they scroll (or jump) back to the bottom — native terminal behavior.
    let follow = true
    const atBottom = () => term.buffer.active.viewportY >= term.buffer.active.baseY

    const send = (obj: unknown) => {
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj))
    }
    // Expose the PTY input path to AccessoryKeyRow (outside the effect closure).
    sendRef.current = (data: string) => send({ type: 'input', data })
    const notifyStatus = (nextKind: 'status' | 'error' | 'closed' | 'open', message: string) => {
      onStatusRef.current?.(nextKind, message)
    }

    // ---- scroll-to-bottom affordance -----------------------------------
    // A small button appears whenever the viewport is detached from the live
    // tail, so there's always an obvious one-click way back to the prompt.
    let bottomJumpVisible = false
    const syncBottomJump = () => {
      if (destroyed) return
      const visible = !atBottom()
      if (visible === bottomJumpVisible) return
      bottomJumpVisible = visible
      setBottomJumpState({ key: terminalInstanceKey, visible })
    }

    // ---- OSC 52 → system clipboard -------------------------------------
    // tmux (set-clipboard on) emits OSC 52 with the selected text base64-encoded
    // when the user drag-selects inside a mouse-mode session. xterm doesn't
    // surface OSC 52 to the OS clipboard by default (security), so plug it in.
    // Payload is "<Pc>;<Pd>"; Pd="?" is a read query we can't honor without a
    // user gesture, so ignore it.
    const osc52: IDisposable | null = term.parser.registerOscHandler(52, (data) => {
      const semi = data.indexOf(';')
      if (semi < 0) return false
      const payload = data.slice(semi + 1)
      if (!payload || payload === '?') return false
      if (!navigator.clipboard?.writeText) return true
      let text: string
      try {
        text = atob(payload)
      } catch {
        return false
      }
      if (!text) return true
      navigator.clipboard
        .writeText(text)
        .then(() => pushToast('ok', 'copied to clipboard'))
        .catch(() => pushToast('error', 'clipboard copy failed'))
      return true
    })

    term.open(host)
    term.focus()

    // ---- Shift + PageUp/PageDown/Home/End → xterm scrollback nav --------
    term.attachCustomKeyEventHandler((event) => {
      if (event.type !== 'keydown') return true
      if (!event.shiftKey || event.altKey || event.ctrlKey || event.metaKey) return true
      if (event.code === 'PageUp') {
        term.scrollPages(-1)
        follow = atBottom()
        syncBottomJump()
      } else if (event.code === 'PageDown') {
        term.scrollPages(1)
        follow = atBottom()
        syncBottomJump()
      } else if (event.code === 'Home') {
        term.scrollToTop()
        follow = false
        syncBottomJump()
      } else if (event.code === 'End') {
        term.scrollToBottom()
        follow = true
        syncBottomJump()
      } else return true
      event.preventDefault()
      return false
    })

    // Keep the follow flag + bottom-jump button honest for xterm-local scroll
    // sources such as Shift+PageUp or the bottom jump button.
    const scrollDisposable = term.onScroll(() => {
      follow = atBottom()
      syncBottomJump()
    })

    // ---- image attach: drop / paste ------------------------------------
    // Drag, drop, or paste an image onto the terminal to attach it to the live
    // agent. The server stores it under the task and returns provider-framed
    // insert text (see attachmentInsertText in routes.go): Claude gets the path
    // wrapped in a bracketed paste so its image detector fires; Codex gets the
    // bare path. We forward those bytes to the PTY verbatim and the agent
    // collapses the path to an `[Image #N]` attachment.
    const attachImageFiles = async (files: File[]) => {
      const images = files.filter((f) => f.type.startsWith('image/'))
      if (images.length === 0) return
      try {
        const insert = await uploadTerminalAttachments(slug, images)
        if (insert) {
          send({ type: 'input', data: insert + ' ' })
          pushToast('ok', images.length > 1 ? `attached ${images.length} images` : 'image attached')
          term.focus()
        }
      } catch (err) {
        pushToast('error', err instanceof Error ? err.message : 'image attach failed')
      }
    }
    const onHostPaste = (e: ClipboardEvent) => {
      const files = Array.from(e.clipboardData?.files ?? [])
      if (!files.some((f) => f.type.startsWith('image/'))) return // text paste → let xterm handle it
      // xterm's own paste handler (on its root element, inside this host) calls
      // stopPropagation, so we must intercept in the CAPTURE phase and stop it
      // here to claim the image before xterm swallows the event.
      e.preventDefault()
      e.stopPropagation()
      void attachImageFiles(files)
    }
    const onHostDragOver = (e: DragEvent) => {
      if (Array.from(e.dataTransfer?.types ?? []).includes('Files')) e.preventDefault()
    }
    const onHostDrop = (e: DragEvent) => {
      const files = Array.from(e.dataTransfer?.files ?? [])
      if (!files.some((f) => f.type.startsWith('image/'))) return
      e.preventDefault()
      void attachImageFiles(files)
    }
    host.addEventListener('paste', onHostPaste, true) // capture: beat xterm's stopPropagation
    host.addEventListener('dragover', onHostDragOver)
    host.addEventListener('drop', onHostDrop)

    // ---- touch-drag → scrollback (phones / tablets) --------------------
    // xterm's DOM renderer only wires WHEEL events to its viewport. The
    // scrollable .xterm-viewport is a SIBLING *behind* the touch-receiving
    // .xterm-screen, so the touch target's ancestor chain has no scroller and
    // native touch-drag can't reach it — scrollback is unreachable on a phone
    // (the `touch-action` CSS alone does nothing). Bridge it: translate a
    // one-finger vertical drag into viewport.scrollTop, mirroring wheel. xterm's
    // Viewport listens to the element's native 'scroll' event, so this re-syncs
    // the buffer and fires term.onScroll (follow/bottom-jump stay honest).
    const viewportEl = host.querySelector<HTMLElement>('.xterm-viewport')
    let touchScrollY = 0
    let touchScrollTop = 0
    let touchTracking = false
    let touchDragging = false
    const onTouchStart = (e: TouchEvent) => {
      if (!viewportEl || e.touches.length !== 1) return
      touchTracking = true
      touchDragging = false
      touchScrollY = e.touches[0].clientY
      touchScrollTop = viewportEl.scrollTop
    }
    const onTouchMove = (e: TouchEvent) => {
      if (!touchTracking || !viewportEl || e.touches.length !== 1) return
      const dy = e.touches[0].clientY - touchScrollY
      // Ignore sub-threshold jitter so a tap still focuses + raises the keyboard.
      if (!touchDragging && Math.abs(dy) < 6) return
      touchDragging = true
      // Drag down (dy>0) pulls older lines into view → scrollTop decreases.
      viewportEl.scrollTop = touchScrollTop - dy
      // Claim the gesture: no page scroll, no text-selection / synthetic mouse drag.
      if (e.cancelable) e.preventDefault()
    }
    const endTouchScroll = () => {
      touchTracking = false
      touchDragging = false
    }
    host.addEventListener('touchstart', onTouchStart, { passive: true })
    host.addEventListener('touchmove', onTouchMove, { passive: false })
    host.addEventListener('touchend', endTouchScroll, { passive: true })
    host.addEventListener('touchcancel', endTouchScroll, { passive: true })

    // ---- input → PTY ---------------------------------------------------
    const dataDisposable = term.onData((data) => {
      const input = stripTerminalGeneratedInput(data)
      if (input) send({ type: 'input', data: input })
    })

    // ---- fit + resize --------------------------------------------------
    const sendResize = () => {
      if (!ws || ws.readyState !== WebSocket.OPEN) return
      const key = `${term.cols}x${term.rows}`
      if (key === lastSize) return
      lastSize = key
      send({ type: 'resize', cols: term.cols, rows: term.rows })
    }
    const fitNow = () => {
      try {
        // Re-measure the cell BEFORE fitting. xterm only measures the cell at
        // open() and on resize — NOT when a webfont finishes loading. So after
        // JetBrains Mono loads, xterm keeps the fallback metrics it measured at
        // open(); FitAddon then divides the host by that stale (smaller) cell
        // height, computes too many rows, and the bottom rows — the live input
        // box — overflow the host and clip below the fold. (A zoom "fixed" it
        // only because the DPR change forced this same re-measure.) measure()
        // updates the render service's dimensions synchronously, so the very
        // next fit.fit() reads the correct cell height. This is the root fix the
        // earlier font-race patches missed: they re-fit but never re-measured.
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        ;(term as any)._core?._charSizeService?.measure?.()
        fit.fit()
      } catch {
        /* host not measurable yet */
      }
    }
    // term.onResize is the authoritative "grid dimensions changed" signal — fire
    // a resize to the PTY (deduped) whenever fit (or anything) changes the grid.
    const resizeDisposable = term.onResize(() => sendResize())

    let resizeFrame = 0
    const resize = () => {
      if (resizeFrame) cancelAnimationFrame(resizeFrame)
      resizeFrame = requestAnimationFrame(() => {
        resizeFrame = 0
        fitNow()
        term.refresh(0, Math.max(0, term.rows - 1))
        // A fit can change cols and reflow the whole buffer; xterm preserves the
        // top visible line, not the bottom, so the tail slides below the fold.
        // Re-pinning here while following is the step the one-shot version
        // missed — it's exactly why a manual resize used to be the workaround.
        if (follow) term.scrollToBottom()
        syncBottomJump()
        openWS()
      })
    }

    const clearReconnect = () => {
      if (!reconnectTimer) return
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }

    const scheduleReconnect = () => {
      if (destroyed || reconnectTimer) return
      const delay = reconnectBackoff
      reconnectBackoff = Math.min(Math.round(reconnectBackoff * 1.7), TERMINAL_RECONNECT_MAX_MS)
      notifyStatus('status', `reconnecting in ${Math.max(1, Math.ceil(delay / 1000))}s`)
      reconnectTimer = setTimeout(() => {
        reconnectTimer = null
        openWS()
      }, delay)
    }

    // Connect the socket once, at the real fitted size. If the host isn't laid
    // out yet (fit yields a degenerate grid), bail — the ResizeObserver's first
    // real callback re-enters here with a valid size.
    const openWS = () => {
      if (
        destroyed ||
        (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING))
      )
        return
      // Don't connect before the font has settled — even if the ResizeObserver's
      // first layout callback re-enters here early. Connecting at fallback-font
      // width would force a wider refit once the font loads, resizing the pane
      // and planting the stale-width duplicate (see fontReadyDone above). The
      // font handler / stall-guard below re-enter openWS once it's safe.
      if (!fontReadyDone) return
      fitNow()
      if (term.cols < 2 || term.rows < 2) return
      lastSize = `${term.cols}x${term.rows}`
      if (sawFirstOutput) {
        term.clear()
        sawFirstOutput = false
      }
      // A reconnect replays the full history again — land at the live tail.
      follow = true
      const sock = new WebSocket(termWsURL(slug, term.cols, term.rows, kind))
      ws = sock
      sock.onopen = () => {
        clearReconnect()
        reconnectBackoff = TERMINAL_RECONNECT_INITIAL_MS
        notifyStatus('open', 'connected')
        scheduleFits()
      }
      sock.onmessage = (ev) => {
        let m: { type: string; data?: string; message?: string }
        try {
          m = JSON.parse(ev.data as string)
        } catch {
          return
        }
        if (m.type === 'output' && m.data != null) {
          term.write(m.data, () => {
            // The first output frame is the history replay — refit to it.
            if (!sawFirstOutput) {
              sawFirstOutput = true
              fitNow()
            }
            // Re-pin on EVERY drained write while following — not just the first.
            // The replay's tail (live prompt + footer) parses after the initial
            // pin, so pinning here keeps the input box glued to the bottom as the
            // stream settles. No-op once already at the bottom.
            if (follow) term.scrollToBottom()
            else syncBottomJump()
          })
        } else if (m.type === 'status') notifyStatus('status', m.message ?? '')
        else if (m.type === 'error') notifyStatus('error', m.message ?? 'terminal error')
      }
      sock.onclose = () => {
        if (ws === sock) ws = null
        if (!destroyed) {
          notifyStatus('closed', 'terminal disconnected')
          scheduleReconnect()
        }
      }
      sock.onerror = () => {
        if (!destroyed) {
          notifyStatus('error', 'connection error')
          scheduleReconnect()
        }
      }
    }

    const fitTimers: ReturnType<typeof setTimeout>[] = []
    const scheduleFits = () => {
      for (const delay of TERMINAL_FIT_DELAYS_MS) fitTimers.push(setTimeout(resize, delay))
    }

    jumpToBottomRef.current = () => {
      follow = true
      term.scrollToBottom()
      syncBottomJump()
      term.focus()
    }

    const observer = new ResizeObserver(resize)
    observer.observe(host)
    window.addEventListener('resize', resize)
    // A tab that was hidden can come back with stale cell metrics; refit on
    // re-show so the grid matches the viewport (and the prompt stays reachable).
    const onVisible = () => {
      if (document.visibilityState === 'visible') resize()
    }
    document.addEventListener('visibilitychange', onVisible)

    // ---- visualViewport: soft-keyboard handling (mobile) -----------------
    // When the soft keyboard opens on phones it shrinks the visual viewport
    // (the CSS viewport stays full-height). We cap the terminal container's
    // height to the visible region so the prompt/input row is never hidden
    // behind the keyboard, then call the existing refit path so xterm reflows.
    // Guard: if visualViewport is absent (desktop / old browsers) we fall back
    // to current behavior (no-op).
    const container = containerRef.current
    const applyViewportHeight = () => {
      if (!container || !window.visualViewport) return
      const vv = window.visualViewport
      // vv.height is the visible region (keyboard already subtracted).
      // vv.offsetTop accounts for any scroll of the layout viewport above the
      // visual one (rare but can happen when scrolled). We use offsetHeight of
      // the outermost shell to avoid going below the bottom of the document.
      const visibleH = vv.height
      // Only constrain on narrow (phone) viewports — ≤640px.
      if (window.innerWidth <= 640) {
        container.style.maxHeight = `${visibleH}px`
      } else {
        container.style.maxHeight = ''
      }
      resize()
    }
    const clearViewportHeight = () => {
      if (container) container.style.maxHeight = ''
      resize()
    }
    if (window.visualViewport) {
      window.visualViewport.addEventListener('resize', applyViewportHeight)
      window.visualViewport.addEventListener('scroll', applyViewportHeight)
    }

    // The fold bug: if we fit+connect BEFORE the real mono font loads, xterm
    // measures the fallback font's cell size, computes the wrong row count, and
    // ends up with a grid taller than the viewport — so the live prompt sits
    // clipped below the fold, unreachable by scrolling (only a zoom/refresh,
    // which re-measures, brings it back). document.fonts.load() resolves
    // specifically when "JetBrains Mono" is ready (more reliable than
    // fonts.ready, which can resolve before the glyph is actually loaded).
    const TERM_FONT = '"JetBrains Mono"'
    const fontReady: Promise<unknown> = document.fonts?.load
      ? Promise.all([
          document.fonts.load(`1em ${TERM_FONT}`),
          document.fonts.load(`bold 1em ${TERM_FONT}`),
        ]).catch(() => undefined)
      : Promise.resolve()

    // Gate the first fit+connect on the font (with a hard timeout so a stalled
    // font load can never leave the terminal unconnected).
    let started = false
    const startOnce = () => {
      if (started || destroyed) return
      started = true
      fitNow()
      openWS()
      scheduleFits()
    }
    void fontReady.then(() => {
      fontReadyDone = true
      startOnce()
    })
    // Stall-guard only. The old value here was 250ms, which lost the race to the
    // webfont on cold loads — so we connected with fallback metrics, refit wider
    // once the font arrived, resized the pane, and tmux froze a stale-width copy
    // of the input box that replayed as a duplicate footer. fontReady already
    // resolves-or-catches on its own, so this timer now only fires in the
    // pathological "font never resolves" case; a few seconds there is harmless.
    fitTimers.push(
      setTimeout(() => {
        fontReadyDone = true
        startOnce()
      }, 4000),
    )

    // And always refit once the font resolves, in case the timeout fired first
    // and we connected with fallback metrics — this corrects the grid + pins to
    // the bottom so the prompt is reattached to the visible row.
    void fontReady.then(() => {
      if (destroyed) return
      fitNow()
      if (follow) term.scrollToBottom()
      syncBottomJump()
      scheduleFits()
    })

    const focusTimer = setTimeout(() => {
      if (!destroyed) term.focus()
    }, 120)

    return () => {
      destroyed = true
      clearReconnect()
      clearTimeout(focusTimer)
      fitTimers.forEach(clearTimeout)
      if (resizeFrame) cancelAnimationFrame(resizeFrame)
      observer.disconnect()
      window.removeEventListener('resize', resize)
      document.removeEventListener('visibilitychange', onVisible)
      if (window.visualViewport) {
        window.visualViewport.removeEventListener('resize', applyViewportHeight)
        window.visualViewport.removeEventListener('scroll', applyViewportHeight)
      }
      clearViewportHeight()
      host.removeEventListener('paste', onHostPaste, true)
      host.removeEventListener('dragover', onHostDragOver)
      host.removeEventListener('drop', onHostDrop)
      host.removeEventListener('touchstart', onTouchStart)
      host.removeEventListener('touchmove', onTouchMove)
      host.removeEventListener('touchend', endTouchScroll)
      host.removeEventListener('touchcancel', endTouchScroll)
      dataDisposable.dispose()
      resizeDisposable.dispose()
      scrollDisposable.dispose()
      osc52?.dispose()
      jumpToBottomRef.current = null
      sendRef.current = null
      try {
        ws?.close()
      } catch {
        /* noop */
      }
      ws = null
      term.dispose()
    }
  }, [slug, kind, restartKey, terminalInstanceKey])

  return (
    <div className="flow-term-wrapper" ref={containerRef}>
      <div className="flow-term">
        <div className="xterm-host" ref={hostRef} />
        {showBottomJump ? (
          <button
            type="button"
            className="terminal-bottom-jump"
            aria-label="Scroll terminal to bottom"
            title="Scroll to bottom"
            onClick={() => jumpToBottomRef.current?.()}
          >
            <ArrowDownToLine size={16} strokeWidth={2.2} />
          </button>
        ) : null}
      </div>
      <AccessoryKeyRow sendData={sendData} />
    </div>
  )
}
