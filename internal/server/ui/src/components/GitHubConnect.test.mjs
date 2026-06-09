import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const read = (p) => readFileSync(resolve(here, p), 'utf8')

const ghConnect = read('./GitHubConnect.tsx')
const query = read('../lib/query.ts')
const types = read('../lib/types.ts')
const connectors = read('../screens/Connectors.tsx')

// The Connect-GitHub wizard is driven entirely by GET /api/github/setup/status,
// the resumable contract the backend exposes.
test('query layer exposes the GitHub setup status hook', () => {
  assert.match(query, /function useGitHubSetupStatus\(/)
  assert.match(query, /\/api\/github\/setup\/status/)
})

test('types declare the GitHubSetupStatus contract', () => {
  assert.match(types, /export interface GitHubSetupStatus/)
  assert.match(types, /ingress_ready/)
  assert.match(types, /app_created/)
  assert.match(types, /install_url/)
})

test('wizard drives the App-manifest flow', () => {
  assert.match(ghConnect, /useGitHubSetupStatus/)
  // create-app returns {manifest, create_url}; the browser must POST the
  // manifest as a form to github.com, not fetch it.
  assert.match(ghConnect, /\/api\/github\/setup\/create-app/)
  assert.match(ghConnect, /create_url/)
  assert.match(ghConnect, /createElement\('form'\)/)
  assert.match(ghConnect, /name = 'manifest'/)
  // Gated on a running public ingress; guides install via install_url.
  assert.match(ghConnect, /ingress_ready/)
  assert.match(ghConnect, /install_url/)
})

test('wizard offers a confirmed disconnect once connected', () => {
  assert.match(ghConnect, /\/api\/github\/setup\/disconnect/)
  assert.match(ghConnect, /confirmAction/)
  assert.match(ghConnect, /Disconnect/)
})

test('org target uses a dropdown of fetched orgs', () => {
  assert.match(ghConnect, /useGitHubOrgs/)
  assert.match(ghConnect, /Select an organization/)
  assert.match(query, /function useGitHubOrgs\(/)
  assert.match(query, /\/api\/github\/setup\/orgs/)
  assert.match(types, /export interface GitHubOrgs/)
})

test('supports installing on both personal and org accounts', () => {
  // manifest must be public for multi-account install; UI surfaces installs + lets you add more
  assert.match(ghConnect, /useGitHubInstallations/)
  assert.match(ghConnect, /Install on another account/)
  assert.match(ghConnect, /personal account and any org/)
  assert.match(query, /function useGitHubInstallations\(/)
  assert.match(query, /\/api\/github\/setup\/installations/)
  assert.match(types, /export interface GitHubInstallations/)
})

test('Connectors screen mounts the GitHub wizard', () => {
  assert.match(connectors, /import \{ GitHubConnect \}/)
  assert.match(connectors, /<GitHubConnect/)
})
