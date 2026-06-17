import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const connectorsSource = readFileSync(resolve(here, 'Connectors.tsx'), 'utf8')
const querySource = readFileSync(resolve(here, '../lib/query.ts'), 'utf8')

// Ingress auth/status moved off the Settings page onto the Connectors screen
// (the Network connector card). It must still render live ingress status from
// the runtime endpoint, including the discovered base URL and any last error.
test('connectors screen renders live ingress status from the runtime endpoint', () => {
  assert.match(querySource, /function useIngressStatus\(/)
  assert.match(connectorsSource, /useIngressStatus/)
  assert.match(connectorsSource, /IngressDetail/)
  assert.match(connectorsSource, /base_url/)
  assert.match(connectorsSource, /last_error/)
})

test('network connector copy covers keep-awake connectivity', () => {
  const registry = readFileSync(resolve(here, '../lib/connectors.ts'), 'utf8')
  assert.match(registry, /keep-awake/i)
  assert.match(registry, /Slack Socket Mode/)
  assert.match(registry, /GitHub webhooks/)
})
