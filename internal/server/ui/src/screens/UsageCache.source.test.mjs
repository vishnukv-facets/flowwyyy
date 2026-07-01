import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const read = (rel) => readFileSync(resolve(here, rel), 'utf8')

const overviewSource = read('Overview.tsx')
const sessionDetailSource = read('SessionDetail.tsx')
const agentCardSource = read('../components/AgentCard.tsx')
const typesSource = read('../lib/types.ts')
const formatSource = read('../lib/format.ts')
const appCss = read('../styles/app.css')

test('shared format helpers expose cache-aware token labels and split tooltips', () => {
  assert.match(formatSource, /function cacheAwareTokens/)
  assert.match(formatSource, /\(\+ \$\{compactTokens\(cached\)\} cached\)/)
  assert.match(formatSource, /function tokenUsageTitle/)
  assert.match(formatSource, /cache-read tokens/)
  assert.match(formatSource, /cache-creation tokens/)
  assert.match(formatSource, /cost_cache_read|cache read/)
})

test('UiAgent and UiStats include cache token and cost split fields', () => {
  for (const field of [
    'cache_read_tokens',
    'cache_creation_tokens',
    'cost_fresh',
    'cost_cache_read',
    'cost_cache_creation',
    'cache_read_total',
    'cache_creation_total',
    'cost_fresh_total',
    'cost_cache_read_total',
    'cost_cache_creation_total',
    'cache_read_steering',
    'cache_creation_steering',
    'cost_cache_read_steering',
    'cost_cache_creation_steering',
  ]) {
    assert.match(typesSource, new RegExp(`${field}\\??:`))
  }
})

test('session token pills show cached reads and use the split tooltip', () => {
  for (const source of [agentCardSource, sessionDetailSource]) {
    assert.match(source, /cacheAwareTokens\(agent\.tokens_session, agent\.cache_read_tokens\)/)
    assert.match(source, /tokenUsageTitle\(\{/)
    assert.match(source, /cacheCreationTokens: agent\.cache_creation_tokens/)
    assert.match(source, /costCacheRead: agent\.cost_cache_read/)
    assert.match(source, /costCacheCreation: agent\.cost_cache_creation/)
  }
})

test('Mission Control stats rows show cached reads for providers, combined, and Steering', () => {
  for (const field of [
    'stats.cache_read_claude',
    'stats.cache_read_codex',
    'stats.cache_read_total',
    'stats.cache_read_steering',
    'stats.cache_creation_steering',
    'stats.cost_cache_read_steering',
    'stats.cost_cache_creation_steering',
  ]) {
    assert.match(overviewSource, new RegExp(field.replace('.', '\\.')))
  }
  assert.match(overviewSource, /function statsTokenValue/)
  assert.match(overviewSource, /stats-tok-main/)
  assert.match(overviewSource, /stats-tok-cache/)
  assert.match(overviewSource, /statsTokenValue\(stats\.tokens_claude, stats\.cache_read_claude\)/)
  assert.match(overviewSource, /statsTokenValue\(stats\.tokens_codex, stats\.cache_read_codex\)/)
  assert.match(overviewSource, /statsTokenValue\(stats\.tokens_total, stats\.cache_read_total\)/)
  assert.match(overviewSource, /statsTokenValue\(stats\.tokens_steering \?\? 0, stats\.cache_read_steering\)/)
  assert.match(appCss, /\.stats-tok-cache/)
  assert.match(appCss, /white-space: nowrap/)
})
