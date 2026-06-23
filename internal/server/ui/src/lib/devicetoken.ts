// In remote (PWA-over-zrok) mode the page is served WITHOUT window.__FLOW_TOKEN__
// (the shared session token must never leave the laptop). Instead the device
// stores its own 12h token, obtained by redeeming a pairing code from the QR.
const KEY = 'flow.device.token'

declare global {
  interface Window {
    __FLOW_TOKEN__?: string
    __FLOW_REMOTE__?: boolean
  }
}

export function isRemoteMode(): boolean {
  return window.__FLOW_REMOTE__ === true
}

export function getDeviceToken(): string | null {
  try { return localStorage.getItem(KEY) } catch { return null }
}
export function setDeviceToken(t: string): void {
  try { localStorage.setItem(KEY, t) } catch { /* ignore */ }
}
export function clearDeviceToken(): void {
  try { localStorage.removeItem(KEY) } catch { /* ignore */ }
}

// authToken is the token to put on /ws/* URLs and RPC. Remote mode uses the
// stored device token; local mode uses the injected session token.
export function authToken(): string {
  if (isRemoteMode()) return getDeviceToken() ?? ''
  return window.__FLOW_TOKEN__ ?? ''
}

// deviceLabel derives a friendly, human-readable device label from the
// user-agent so the laptop's paired-devices list reads "iPad" / "iPhone" /
// "Android phone" rather than a raw UA string. Best-effort; falls back to a
// generic label. The operator can tell at a glance which device is paired.
export function deviceLabel(): string {
  const ua = navigator.userAgent
  if (/iPad/i.test(ua) || (/Macintosh/i.test(ua) && navigator.maxTouchPoints > 1)) return 'iPad'
  if (/iPhone/i.test(ua)) return 'iPhone'
  if (/Android/i.test(ua)) return /Mobile/i.test(ua) ? 'Android phone' : 'Android tablet'
  if (/Macintosh/i.test(ua)) return 'Mac'
  if (/Windows/i.test(ua)) return 'Windows PC'
  return 'Paired device'
}

// maybePairFromUrl redeems a ?pair=<code> query param (the QR target) into a
// device token, stores it, and strips the param from the URL. No-op otherwise.
// The ?pair= param is ALWAYS stripped — even on a network error — so the code
// never appears in browser history or gets accidentally retried on reload.
export async function maybePairFromUrl(): Promise<void> {
  const url = new URL(window.location.href)
  const code = url.searchParams.get('pair')
  if (!code) return
  try {
    const res = await fetch('/api/remote/pair', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code, label: deviceLabel() }),
    })
    if (res.ok) {
      const data = await res.json()
      if (data.token) setDeviceToken(data.token)
    }
  } finally {
    url.searchParams.delete('pair')
    window.history.replaceState({}, '', url.toString())
  }
}
