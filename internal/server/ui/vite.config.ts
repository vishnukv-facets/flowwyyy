import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import { resolve, join } from 'node:path'
import { readFileSync } from 'node:fs'
import { homedir } from 'node:os'

// Dev-only: hand the page the running backend's data-plane session token, the
// same way the Go server does in production. Every /ws/* handshake is rejected
// without window.__FLOW_TOKEN__ (audit P0-1), and the SPA talks to the backend
// ENTIRELY over that token-gated WebSocket — so without this, `pnpm dev` loads a
// token-less shell and every RPC fails. The backend mints the token at boot and
// persists it 0600 to $FLOW_ROOT/.ui-session-token, so we read it from disk and
// inject it — no backend restart needed to iterate the UI with HMR.
//
// apply:'serve' → this NEVER runs in `vite build`; the production embed is
// untouched and keeps getting its token from the Go server. Read fresh on each
// transform so a backend restart (new token) is picked up on the next reload.
function injectDevSessionToken(): Plugin {
  return {
    name: 'flow-dev-session-token',
    apply: 'serve',
    transformIndexHtml() {
      const root = process.env.FLOW_ROOT?.trim() || join(homedir(), '.flow')
      let token = ''
      try {
        token = readFileSync(join(root, '.ui-session-token'), 'utf8').trim()
      } catch {
        // Backend not running / no token yet — inject nothing. The UI will show
        // the same RPC error until `flow ui serve` is up and the page reloads.
        return []
      }
      if (!token) return []
      return [{ tag: 'script', injectTo: 'head-prepend', children: `window.__FLOW_TOKEN__=${JSON.stringify(token)};` }]
    },
  }
}

// The built UI is committed and go:embed'd into internal/server/static, then
// served by the Go binary (Node-free at runtime). Absolute base ('/') so deep
// client-routed links like /session/<slug> still resolve /assets/* correctly —
// the server falls back to index.html for unknown paths.
export default defineConfig({
  plugins: [react(), injectDevSessionToken()],
  base: '/',
  build: {
    outDir: resolve(__dirname, '../static'),
    emptyOutDir: true,
    target: 'es2022',
    sourcemap: false,
    chunkSizeWarningLimit: 2500,
  },
  // Dev convenience: `pnpm dev` proxies data + sockets to a running
  // `flow ui serve` so the UI can be iterated with HMR. Production never
  // uses this — it talks to the same-origin Go server. NOTE: no changeOrigin on
  // /ws — forwarding the original Host (localhost:5173) keeps it equal to the
  // browser's Origin, satisfying the backend's strict same-origin WS gate.
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8787', changeOrigin: true },
      '/ws': { target: 'ws://127.0.0.1:8787', ws: true },
    },
  },
})
