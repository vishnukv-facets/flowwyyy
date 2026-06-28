import assert from 'node:assert/strict'
import test from 'node:test'

import { extractFlowTokenFromHTML } from './devicetoken.ts'

test('extractFlowTokenFromHTML reads injected local token', () => {
  const token = 'a'.repeat(64)
  assert.equal(extractFlowTokenFromHTML(`<script>window.__FLOW_TOKEN__="${token}";</script>`), token)
})

test('extractFlowTokenFromHTML ignores pages without a token', () => {
  assert.equal(extractFlowTokenFromHTML('<html></html>'), '')
})
