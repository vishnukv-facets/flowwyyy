import { test } from 'node:test'
import assert from 'node:assert/strict'

import { workEventLinkHref } from './workEventLinks.ts'

test('attention work-event links open the targeted feed item', () => {
  assert.equal(
    workEventLinkHref({ kind: 'attention', target: 'feed 1' }),
    '/attention?item=feed+1',
  )
})

test('trace work-event links open the targeted trace row', () => {
  assert.equal(
    workEventLinkHref({ kind: 'trace', target: 'trace/1' }),
    '/attention?view=trace&trace=trace%2F1',
  )
})

test('task, project, source, and unknown work-event links keep existing behavior', () => {
  assert.equal(workEventLinkHref({ kind: 'task', target: 'demo task' }), '/session/demo%20task')
  assert.equal(workEventLinkHref({ kind: 'project', target: 'flow-manager' }), '/project/flow-manager')
  assert.equal(workEventLinkHref({ kind: 'source', target: 'https://example.test/x' }), 'https://example.test/x')
  assert.equal(workEventLinkHref({ kind: 'unknown', target: 'x' }), '')
})
