import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const connectorsScreen = readFileSync(resolve(here, 'Connectors.tsx'), 'utf8')
const registry = readFileSync(resolve(here, '../lib/connectors.ts'), 'utf8')
const querySource = readFileSync(resolve(here, '../lib/query.ts'), 'utf8')
const typesSource = readFileSync(resolve(here, '../lib/types.ts'), 'utf8')

test('ClickUp connector is live and backed by runtime status hooks', () => {
  assert.match(registry, /id: 'clickup'[\s\S]*capabilityId: 'clickup'/)
  assert.doesNotMatch(registry, /id: 'clickup'[\s\S]{0,220}soon: true/)
  assert.match(querySource, /function useClickUpSetupStatus\(/)
  assert.match(querySource, /function useClickUpWebhookStatus\(/)
  assert.match(typesSource, /interface ClickUpSetupStatus/)
  assert.match(typesSource, /interface ClickUpWebhookStatus/)
})

test('ClickUp setup panel exposes OAuth, token fallback, webhook, register, and disconnect controls', () => {
  assert.match(connectorsScreen, /function ClickUpConnect\(/)
  assert.match(connectorsScreen, /\/api\/clickup\/setup\/oauth\/start/)
  assert.match(connectorsScreen, /Connect to ClickUp/)
  assert.match(connectorsScreen, /\/api\/clickup\/setup\/token/)
  assert.match(connectorsScreen, /Save token/)
  assert.match(connectorsScreen, /\/api\/clickup\/setup\/register-webhook/)
  assert.match(connectorsScreen, /\/api\/clickup\/setup\/disconnect/)
  assert.match(connectorsScreen, /ClickUpWebhookTransport/)
})
