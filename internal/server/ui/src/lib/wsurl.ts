// Shared WebSocket URL builder. The server inlines a data-plane session token
// into the page (window.__FLOW_TOKEN__); every /ws/* handshake must carry it or
// the server rejects the upgrade (audit P0-1). Browsers can't set custom
// headers on a WS handshake, so the token rides the query string.
// See internal/server/session_token.go.

export function sessionToken(): string {
  return (window as unknown as { __FLOW_TOKEN__?: string }).__FLOW_TOKEN__ ?? ''
}

export function wsURL(path: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const base = `${proto}//${location.host}${path}`
  const tok = sessionToken()
  if (!tok) return base
  const sep = path.includes('?') ? '&' : '?'
  return `${base}${sep}token=${encodeURIComponent(tok)}`
}
