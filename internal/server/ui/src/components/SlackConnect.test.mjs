import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const read = (p) => readFileSync(resolve(here, p), 'utf8')

const slackConnect = read('./SlackConnect.tsx')
const types = read('../lib/types.ts')

test('Slack wizard surfaces stale app scopes before reinstall', () => {
  assert.match(types, /needs_reinstall/)
  assert.match(slackConnect, /st\.needs_reinstall/)
  assert.match(slackConnect, /Slack app scopes changed/)
})
