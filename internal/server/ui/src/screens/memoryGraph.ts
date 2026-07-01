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
}

export interface MemoryGraphEdge {
  id: string
  source: string
  target: string
  label: string
}

export interface MemoryGraph {
  nodes: MemoryGraphNode[]
  edges: MemoryGraphEdge[]
  visibleSources: MemorySource[]
}

function normalized(value: string | undefined) {
  return (value ?? '').trim().toLowerCase()
}

export function filterMemorySources(sources: MemorySource[], filters: MemoryGraphFilters): MemorySource[] {
  const needle = normalized(filters.query)
  return sources.filter((source) => {
    if (filters.provider && source.provider !== filters.provider) return false
    if (!needle) return true
    return (
      normalized(source.label).includes(needle) ||
      normalized(source.path).includes(needle) ||
      normalized(source.scope).includes(needle)
    )
  })
}

export function buildMemoryGraph(sources: MemorySource[], filters: MemoryGraphFilters): MemoryGraph {
  const visibleSources = filterMemorySources(sources, filters)
  const byLabel = new Map<string, MemorySource>()
  for (const source of visibleSources) {
    const key = normalized(source.label)
    if (key && !byLabel.has(key)) byLabel.set(key, source)
  }

  const edges: MemoryGraphEdge[] = []
  const seenEdges = new Set<string>()
  for (const source of visibleSources) {
    for (const ref of wikiRefs(source.content ?? '')) {
      const target = byLabel.get(ref)
      if (!target || target.id === source.id) continue
      const id = `memory-edge:${source.id}->${target.id}:${ref}`
      if (seenEdges.has(id)) continue
      seenEdges.add(id)
      edges.push({ id, source: source.id, target: target.id, label: 'links to' })
    }
  }

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
