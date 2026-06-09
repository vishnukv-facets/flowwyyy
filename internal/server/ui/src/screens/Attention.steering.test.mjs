import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const read = (rel) => readFileSync(resolve(here, rel), 'utf8')

const attentionSource = read('Attention.tsx')
const settingsSource = read('Settings.tsx')
const steeringSource = read('../components/SteeringConfig.tsx')
const pickerSource = read('../components/ChannelPicker.tsx')

// Steering (attention-router) config moved off the Settings page onto a third
// `config` view of the Attention screen, co-located with the feed it governs.
test('attention screen hosts a config view that renders SteeringConfig', () => {
  assert.match(attentionSource, /VIEWS = \['feed', 'trace', 'config'\]/)
  assert.match(attentionSource, /import \{ SteeringConfig \}/)
  assert.match(attentionSource, /<SteeringConfig \/>/)
  // The third segment is reachable + deep-linkable via ?view=config.
  assert.match(attentionSource, /view=\$\{next\}|view=config/)
})

// Settings no longer owns any steering UI: the rich section is gone and the
// generic config form excludes the whole Steering group (so only workspace
// groups like General remain).
test('settings page no longer renders steering controls', () => {
  assert.doesNotMatch(settingsSource, /WatchedChannels/)
  assert.doesNotMatch(settingsSource, /AutonomyPanel/)
  assert.doesNotMatch(settingsSource, /title="Steering"/)
  assert.match(settingsSource, /f\.group !== 'Steering'/)
})

// SteeringConfig groups every relocated steering key (Triage scope / Autonomy /
// Performance) and reuses the shared picker + autonomy components.
test('steering config consolidates every steering key', () => {
  assert.match(steeringSource, /Triage scope/)
  assert.match(steeringSource, /Autonomy/)
  assert.match(steeringSource, /Performance/)
  assert.match(steeringSource, /import \{ ChannelPicker \}/)
  assert.match(steeringSource, /import \{ AutonomyPanel \}/)
  // Both watched + muted channels render as the same picker.
  assert.match(steeringSource, /FLOW_STEERING_WATCH_CHANNELS/)
  assert.match(steeringSource, /FLOW_STEERING_MUTED_CHANNELS/)
  // The generic-field groups carry the rest.
  for (const key of [
    'FLOW_STEERING_MUTED_KEYWORDS',
    'FLOW_STEERING_AUTO_RESOLVE_WAITING',
    'FLOW_STEERING_SEND_MODEL',
    'FLOW_STEERING_CLASSIFIER_BUDGET_PER_HOUR',
    'FLOW_STEERING_CLASSIFIER_FAILURE_COOLDOWN',
  ]) {
    assert.match(steeringSource, new RegExp(key))
  }
})

// The shared picker is key-agnostic and preserves channel IDs absent from the
// live Slack list (it saves Array.from(current), seeded from the saved set).
test('channel picker is reusable and preserves off-list ids', () => {
  assert.match(pickerSource, /settingKey/)
  assert.match(pickerSource, /Array\.from\(current\)\.join\(','\)/)
})
