import { test } from 'node:test'
import assert from 'node:assert/strict'

import { nextTraceWindowAnchor, traceSinceForWindow } from './traceWindow.ts'

test('trace window anchor stays stable across renders of the same window', () => {
  const anchor = { windowId: '24h', nowMs: Date.parse('2026-06-08T02:00:00.000Z') }

  assert.deepEqual(
    nextTraceWindowAnchor(anchor, '24h', Date.parse('2026-06-08T02:05:00.000Z')),
    anchor,
  )
})

test('trace window anchor resets when the operator changes windows', () => {
  const anchor = { windowId: '24h', nowMs: Date.parse('2026-06-08T02:00:00.000Z') }

  assert.deepEqual(
    nextTraceWindowAnchor(anchor, '1h', Date.parse('2026-06-08T02:05:00.000Z')),
    { windowId: '1h', nowMs: Date.parse('2026-06-08T02:05:00.000Z') },
  )
})

test('trace since is derived from the stable anchor, not render time', () => {
  const anchor = { windowId: '24h', nowMs: Date.parse('2026-06-08T02:00:00.000Z') }

  assert.equal(traceSinceForWindow(anchor, 24 * 60 * 60 * 1000), '2026-06-07T02:00:00.000Z')
})
