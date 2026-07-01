import { buildMemoryGraph } from '../src/screens/memoryGraph.js'
import type { MemorySource } from '../src/lib/types.js'

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
    source({ id: 'global', label: 'Global', content: 'See [[Project note|project]], [[connector-auth-page]], and [[Missing]].' }),
    source({ id: 'project', label: 'Project note', scope: 'project', content: 'Back to [[Global#section]].' }),
    source({ id: 'root', label: 'Codex memory MEMORY.md', path: '/tmp/memory/MEMORY.md', kind: 'auto-memory' }),
    source({
      id: 'connector',
      label: 'Codex memory connector_auth_page.md',
      path: '/tmp/memory/notes/connector-auth-page.md',
      kind: 'auto-memory',
    }),
    source({ id: 'claude', label: 'Claude', provider: 'claude', available: false, status: 'missing' }),
  ],
  { query: '', provider: '' },
)

expect(graph.nodes.length === 4, 'expected available sources as nodes')
expect(!graph.nodes.some((node) => node.id === 'claude'), 'expected missing sources to be filtered out')
expect(graph.edges.length === 4, 'expected resolved wiki links and memory hierarchy as edges')
expect(graph.edges.some((edge) => edge.source === 'global' && edge.target === 'project'), 'expected global -> project edge')
expect(graph.edges.some((edge) => edge.source === 'global' && edge.target === 'connector'), 'expected path-slug reference edge')
expect(graph.edges.some((edge) => edge.source === 'project' && edge.target === 'global'), 'expected project -> global edge')
expect(graph.edges.some((edge) => edge.source === 'root' && edge.target === 'connector' && edge.kind === 'contains'), 'expected root -> child hierarchy edge')

const filtered = buildMemoryGraph(graph.visibleSources, { query: 'global', provider: '' })
expect(filtered.nodes.length === 1, 'expected query to filter nodes')
expect(filtered.edges.length === 0, 'expected hidden targets to omit edges')

const providerFiltered = buildMemoryGraph(graph.visibleSources, { query: '', provider: 'claude' })
expect(providerFiltered.nodes.length === 0, 'expected provider filter to omit missing provider node')

console.log('memoryGraph self-check passed')
