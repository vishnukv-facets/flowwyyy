// Live event channel — server→client push over /ws/events. Drives cache
// invalidation so the UI stays live without polling. Pure push; all
// request/response traffic goes through rpc.ts instead.

import { wsURL } from './wsurl'

export interface EventEnvelope {
  type: string
  timestamp?: string
  session_id?: string
  task_slug?: string
  seq?: number
  data?: unknown
  hook?: Record<string, unknown>
  liveness?: { provider: string; slug?: string; status: string; reason?: string }
  runtime?: { provider: string; status: string; kind?: string }
}

class EventsClient {
  private ws: WebSocket | null = null
  private backoff = 400
  private listeners = new Set<(env: EventEnvelope) => void>()

  constructor() {
    this.connect()
  }

  on(fn: (env: EventEnvelope) => void): () => void {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  private connect() {
    let ws: WebSocket
    try {
      ws = new WebSocket(wsURL('/ws/events'))
    } catch {
      this.scheduleReconnect()
      return
    }
    this.ws = ws
    ws.onmessage = (ev) => {
      let env: EventEnvelope
      try {
        env = JSON.parse(ev.data as string)
      } catch {
        return
      }
      if (env.type === 'ping' || env.type === 'subscribed') return
      this.backoff = 400
      this.listeners.forEach((fn) => fn(env))
    }
    ws.onclose = () => this.scheduleReconnect()
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
}

export const events = new EventsClient()
