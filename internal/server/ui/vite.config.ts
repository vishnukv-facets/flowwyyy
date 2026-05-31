import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { resolve } from 'node:path'

// The built UI is committed and go:embed'd into internal/server/static, then
// served by the Go binary (Node-free at runtime). Absolute base ('/') so deep
// client-routed links like /session/<slug> still resolve /assets/* correctly —
// the server falls back to index.html for unknown paths.
export default defineConfig({
  plugins: [react()],
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
  // uses this — it talks to the same-origin Go server.
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8787', changeOrigin: true },
      '/ws': { target: 'ws://127.0.0.1:8787', ws: true },
    },
  },
})
