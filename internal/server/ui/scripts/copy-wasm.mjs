// Copies the wterm Zig/WASM terminal core into public/ so Vite emits it to
// internal/server/static/wterm.wasm, where the Go server serves it at
// /wterm.wasm (mime application/wasm). wterm's WasmBridge.load() fetches this
// URL at terminal init — there is no bundled default, so this copy is required.
import { copyFileSync, mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const src = resolve(here, '../node_modules/@wterm/core/wasm/wterm.wasm')
const dest = resolve(here, '../public/wterm.wasm')

mkdirSync(dirname(dest), { recursive: true })
copyFileSync(src, dest)
console.log(`[copy-wasm] ${src} -> ${dest}`)
