// Shared WebSocket URL builder. Every /ws/* handshake must carry an auth token
// or the server rejects the upgrade (audit P0-1). Browsers can't set custom
// headers on a WS handshake, so the token rides the query string.
// In local mode the token is the session token injected by the server
// (window.__FLOW_TOKEN__). In remote (PWA-over-zrok) mode it is the device
// token stored in localStorage. authToken() handles both cases.
// See internal/server/session_token.go and src/lib/devicetoken.ts.

import { authToken } from './devicetoken'

export function wsURL(path: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const base = `${proto}//${location.host}${path}`
  const tok = authToken()
  if (!tok) return base
  const sep = path.includes('?') ? '&' : '?'
  return `${base}${sep}token=${encodeURIComponent(tok)}`
}
