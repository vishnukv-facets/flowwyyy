import { buildMemoryGraph } from './memoryGraph.js'
import type { MemorySource } from '../lib/types.js'

function source(input: Partial<MemorySource> & Pick<MemorySource, 'id' | 'label'>): MemorySource {
  return {
    id: input.id,
    provider: input.provider ?? 'codex',
    scope: input.scope ?? 'user',
    kind: input.kind ?? 'instructions',
    label: input.label,
    path: input.path ?? `/tmp/${input.id}.md`,
    status: input.status ?? 'available',
    available: input.available ?? true,
    content: input.content ?? '',
    error: input.error,
  }
}

function expect(condition: boolean, message: string) {
  if (!condition) throw new Error(message)
}

const graph = buildMemoryGraph(
  [
    source({ id: 'global', label: 'Global', content: 'See [[Project]] and [[Missing]].' }),
    source({ id: 'project', label: 'Project', scope: 'project', content: 'Back to [[Global]].' }),
    source({ id: 'claude', label: 'Claude', provider: 'claude', available: false, status: 'missing' }),
  ],
  { query: '', provider: '' },
)

expect(graph.nodes.length === 3, 'expected all sources as nodes')
expect(graph.edges.length === 2, 'expected only resolved wiki links as edges')
expect(graph.edges.some((edge) => edge.source === 'global' && edge.target === 'project'), 'expected global -> project edge')
expect(graph.edges.some((edge) => edge.source === 'project' && edge.target === 'global'), 'expected project -> global edge')
expect(graph.nodes.find((node) => node.id === 'claude')?.status === 'missing', 'expected unavailable source status')

const filtered = buildMemoryGraph(graph.visibleSources, { query: 'global', provider: '' })
expect(filtered.nodes.length === 1, 'expected query to filter nodes')
expect(filtered.edges.length === 0, 'expected hidden targets to omit edges')

const providerFiltered = buildMemoryGraph(graph.visibleSources, { query: '', provider: 'claude' })
expect(providerFiltered.nodes.length === 1, 'expected provider filter to keep one node')
expect(providerFiltered.nodes[0]?.id === 'claude', 'expected claude provider node')

console.log('memoryGraph self-check passed')
