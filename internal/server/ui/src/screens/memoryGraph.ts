import type { MemorySource } from '../lib/types.js'
import { wikiRefs } from '../lib/wikiRefs.js'

export interface MemoryGraphFilters {
  query: string
  provider: string
}

export interface MemoryGraphNode extends Record<string, unknown> {
  id: string
  label: string
  provider: string
  scope: string
  kind: string
  path: string
  status: string
  available: boolean
  error?: string
  summary: string
  badges: string[]
  focus?: 'selected' | 'connected' | 'dimmed'
}

export interface MemoryGraphEdge {
  id: string
  source: string
  target: string
  label: string
  kind: 'references' | 'contains'
}

export interface MemoryGraph {
  nodes: MemoryGraphNode[]
  edges: MemoryGraphEdge[]
  visibleSources: MemorySource[]
}

function normalized(value: string | undefined) {
  return (value ?? '').trim().toLowerCase()
}

export function isGraphableMemorySource(source: MemorySource) {
  return source.available && source.status !== 'missing'
}

function slug(value: string | undefined) {
  return normalized(value)
    .replace(/\.[a-z0-9]+$/i, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}

function basename(path: string | undefined) {
  return (path ?? '').split('/').filter(Boolean).pop() ?? ''
}

function dirname(path: string | undefined) {
  return (path ?? '').replace(/\/[^/]*$/, '')
}

function sourceAliases(source: MemorySource) {
  const aliases = new Set<string>()
  for (const value of [source.id, source.label, basename(source.path), source.path]) {
    const alias = slug(value)
    if (alias) aliases.add(alias)
  }
  for (const prefix of ['codex-memory-', 'claude-auto-memory-', 'codex-global-', 'claude-global-', 'codex-project-', 'claude-project-']) {
    for (const alias of [...aliases]) {
      if (alias.startsWith(prefix)) aliases.add(alias.slice(prefix.length))
    }
  }
  return aliases
}

const GENERIC_REF_SLUGS = new Set([
  'brief',
  'context',
  'id',
  'memory',
  'memories',
  'name',
  'path',
  'project',
  'project-slug',
  'projects',
  'slug',
  'source',
  'sources',
  'task',
  'task-slug',
  'tasks',
  'todo',
  'updates',
  'wiki-link',
])

function filterMemorySources(sources: MemorySource[], filters: MemoryGraphFilters): MemorySource[] {
  const needle = normalized(filters.query)
  return sources.filter((source) => {
    if (!isGraphableMemorySource(source)) return false
    if (filters.provider && source.provider !== filters.provider) return false
    if (!needle) return true
    return (
      normalized(source.label).includes(needle) ||
      normalized(source.path).includes(needle) ||
      normalized(source.scope).includes(needle)
    )
  })
}

function appendEdge(edges: MemoryGraphEdge[], seenEdges: Set<string>, edge: MemoryGraphEdge) {
  if (edge.source === edge.target || seenEdges.has(edge.id)) return
  seenEdges.add(edge.id)
  edges.push(edge)
}

function buildAliasMap(sources: MemorySource[]) {
  const aliases = new Map<string, MemorySource[]>()
  for (const source of sources) {
    for (const alias of sourceAliases(source)) {
      const bucket = aliases.get(alias) ?? []
      bucket.push(source)
      aliases.set(alias, bucket)
    }
  }
  return aliases
}

function resolveRefTarget(ref: string, aliases: Map<string, MemorySource[]>) {
  const refSlug = slug(ref)
  if (!refSlug || refSlug.length < 4 || GENERIC_REF_SLUGS.has(refSlug)) return null

  const exact = aliases.get(refSlug)
  if (exact?.length) return exact[0]

  let best: { alias: string; source: MemorySource } | null = null
  for (const [alias, sources] of aliases) {
    if (alias.length < refSlug.length || !alias.includes(refSlug)) continue
    if (!best || alias.length < best.alias.length) best = { alias, source: sources[0] }
  }
  return best?.source ?? null
}

function appendReferenceEdges(visibleSources: MemorySource[], edges: MemoryGraphEdge[], seenEdges: Set<string>) {
  const aliases = buildAliasMap(visibleSources)
  for (const source of visibleSources) {
    for (const ref of wikiRefs(source.content ?? '')) {
      const target = resolveRefTarget(ref, aliases)
      if (!target) continue
      appendEdge(edges, seenEdges, {
        id: `memory-edge:references:${source.id}->${target.id}`,
        source: source.id,
        target: target.id,
        label: 'references',
        kind: 'references',
      })
    }
  }
}

function appendHierarchyEdges(visibleSources: MemorySource[], edges: MemoryGraphEdge[], seenEdges: Set<string>) {
  const roots = visibleSources.filter((source) => normalized(basename(source.path)) === 'memory.md')
  for (const source of visibleSources) {
    let nearestRoot: MemorySource | null = null
    let nearestDir = ''

    for (const root of roots) {
      if (root.id === source.id) continue
      const rootDir = dirname(root.path)
      if (!rootDir || rootDir.length <= nearestDir.length) continue
      if ((source.path ?? '').startsWith(`${rootDir}/`)) {
        nearestRoot = root
        nearestDir = rootDir
      }
    }

    if (!nearestRoot) continue
    appendEdge(edges, seenEdges, {
      id: `memory-edge:contains:${nearestRoot.id}->${source.id}`,
      source: nearestRoot.id,
      target: source.id,
      label: 'contains',
      kind: 'contains',
    })
  }
}

export function buildMemoryGraph(sources: MemorySource[], filters: MemoryGraphFilters): MemoryGraph {
  const visibleSources = filterMemorySources(sources, filters)
  const edges: MemoryGraphEdge[] = []
  const seenEdges = new Set<string>()
  appendReferenceEdges(visibleSources, edges, seenEdges)
  appendHierarchyEdges(visibleSources, edges, seenEdges)

  return {
    visibleSources,
    edges,
    nodes: visibleSources.map((source) => ({
      id: source.id,
      label: source.label,
      provider: source.provider,
      scope: source.scope,
      kind: source.kind,
      path: source.path,
      status: source.available ? 'available' : source.error ? 'error' : source.status || 'missing',
      available: source.available,
      error: source.error,
      summary: source.path,
      badges: [source.scope, source.kind].filter(Boolean),
    })),
  }
}
