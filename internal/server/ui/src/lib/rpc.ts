// WS-RPC client — the single data plane. Every read and mutation the UI makes
// rides this socket to /ws/rpc; the server replays it through the REST handler
// mux and returns a correlated response. No fetch() anywhere in the app.

import { wsURL } from './wsurl'

export type ConnStatus = 'connecting' | 'open' | 'closed'

export interface RpcResponse {
  type: string
  id: string
  status: number
  content_type?: string
  json?: unknown
  text?: string
  error?: string
}

interface RpcOutFrame {
  id: string
  method: string
  path: string
  body?: unknown
  text?: string
  content_type?: string
  form?: Record<string, string>
  files?: { field?: string; name: string; content_type: string; data: string }[]
}

export interface RpcFile {
  field?: string
  name: string
  content_type: string
  data: string // base64
}

export interface RpcRequestInit {
  method?: string
  path: string
  body?: unknown
  text?: string
  contentType?: string
  form?: Record<string, string>
  files?: RpcFile[]
  timeoutMs?: number
}

type Pending = {
  resolve: (r: RpcResponse) => void
  reject: (e: unknown) => void
  timer: ReturnType<typeof setTimeout>
}

class RpcClient {
  private ws: WebSocket | null = null
  private status: ConnStatus = 'connecting'
  private pending = new Map<string, Pending>()
  private outbox: string[] = []
  private seq = 0
  private backoff = 400
  private statusListeners = new Set<(s: ConnStatus) => void>()
  private readyListeners = new Set<() => void>()

  constructor() {
    this.connect()
    // Reconnect promptly when the tab is refocused after sleep/disconnect.
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible' && this.status === 'closed') this.connect()
    })
  }

  onStatus(fn: (s: ConnStatus) => void): () => void {
    this.statusListeners.add(fn)
    fn(this.status)
    return () => this.statusListeners.delete(fn)
  }

  /** Fires every time the socket (re)opens — used to resync caches. */
  onReady(fn: () => void): () => void {
    this.readyListeners.add(fn)
    return () => this.readyListeners.delete(fn)
  }

  getStatus(): ConnStatus {
    return this.status
  }

  private setStatus(s: ConnStatus) {
    if (this.status === s) return
    this.status = s
    this.statusListeners.forEach((fn) => fn(s))
  }

  private connect() {
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING))
      return
    this.setStatus('connecting')
    let ws: WebSocket
    try {
      ws = new WebSocket(wsURL('/ws/rpc'))
    } catch {
      this.scheduleReconnect()
      return
    }
    this.ws = ws

    ws.onmessage = (ev) => {
      let msg: RpcResponse
      try {
        msg = JSON.parse(ev.data as string)
      } catch {
        return
      }
      if (msg.type === 'ready') {
        this.backoff = 400
        this.setStatus('open')
        this.flush()
        this.readyListeners.forEach((fn) => fn())
        return
      }
      if (!msg.id) return
      const p = this.pending.get(msg.id)
      if (!p) return
      this.pending.delete(msg.id)
      clearTimeout(p.timer)
      p.resolve(msg)
    }

    ws.onclose = () => {
      this.setStatus('closed')
      this.failAll(new Error('rpc socket closed'))
      this.scheduleReconnect()
    }
    ws.onerror = () => {
      try {
        ws.close()
      } catch {
        /* noop */
      }
    }
  }

  private scheduleReconnect() {
    const delay = this.backoff
    this.backoff = Math.min(this.backoff * 1.7, 8000)
    setTimeout(() => this.connect(), delay)
  }

  private flush() {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return
    const queued = this.outbox
    this.outbox = []
    for (const f of queued) this.ws.send(f)
  }

  private failAll(err: unknown) {
    for (const [, p] of this.pending) {
      clearTimeout(p.timer)
      p.reject(err)
    }
    this.pending.clear()
  }

  private rawSend(frame: string) {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) this.ws.send(frame)
    else {
      this.outbox.push(frame)
      this.connect()
    }
  }

  request(init: RpcRequestInit): Promise<RpcResponse> {
    const id = `r${++this.seq}`
    const frame: RpcOutFrame = {
      id,
      method: init.method ?? 'GET',
      path: init.path,
    }
    if (init.files && init.files.length) frame.files = init.files
    if (init.form) frame.form = init.form
    if (init.text !== undefined) {
      frame.text = init.text
      if (init.contentType) frame.content_type = init.contentType
    } else if (init.body !== undefined) {
      frame.body = init.body
    }

    return new Promise<RpcResponse>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id)
        reject(new Error(`rpc timeout: ${init.method ?? 'GET'} ${init.path}`))
      }, init.timeoutMs ?? 30000)
      this.pending.set(id, { resolve, reject, timer })
      this.rawSend(JSON.stringify(frame))
    })
  }
}

export const rpc = new RpcClient()
