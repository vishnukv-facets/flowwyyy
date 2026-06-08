import { test } from 'node:test'
import assert from 'node:assert/strict'

import { UI_DATA_IDLE_REFETCH_MS, focusedLiveInvalidationKeys } from './liveInvalidation.ts'

test('high-volume runtime events only refresh the live UI snapshot', () => {
  for (const type of ['agent_hook', 'liveness', 'runtime', 'hook_health']) {
    assert.deepEqual(focusedLiveInvalidationKeys({ type }), ['ui-data'])
  }
})

test('state mutation events still request a broad live-data refresh', () => {
  assert.equal(focusedLiveInvalidationKeys({ type: 'ui_change' }), null)
})

test('idle ui-data polling is slow enough to avoid hot-looping large dashboards', () => {
  assert.ok(UI_DATA_IDLE_REFETCH_MS >= 30_000)
})
