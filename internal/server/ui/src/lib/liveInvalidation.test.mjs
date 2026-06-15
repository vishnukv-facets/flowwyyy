import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { focusedLiveInvalidationKeys } from './liveInvalidation.ts'

const here = dirname(fileURLToPath(import.meta.url))

test('high-volume runtime events only refresh the live UI snapshot', () => {
  for (const type of ['agent_hook', 'liveness', 'runtime', 'hook_health']) {
    assert.deepEqual(focusedLiveInvalidationKeys({ type }), ['ui-data'])
  }
})

test('state mutation events still request a broad live-data refresh', () => {
  assert.equal(focusedLiveInvalidationKeys({ type: 'ui_change' }), null)
})

test('ui-data refresh is event-driven rather than interval-polled', () => {
  const querySource = readFileSync(resolve(here, 'query.ts'), 'utf8')
  const useUiData = querySource.match(/export function useUiData\(\) \{[\s\S]*?\n\}/)?.[0] ?? ''
  assert.ok(useUiData.includes("queryKey: ['ui-data']"), 'test could not find useUiData query body')
  assert.equal(useUiData.includes('refetchInterval'), false)
})
