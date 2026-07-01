import { useMemo, useState } from 'react'
import { Brain, Search } from 'lucide-react'
import { useMemorySources, queryClient } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { apiPost } from '../lib/api'
import { EmptyState, Loading } from '../components/ui'
import { DocEditor, type Backlink } from '../components/DocEditor'
import { wikiRefs } from '../lib/wikiRefs'
import { MemoryGraphCanvas } from '../components/memoryGraph/MemoryGraphCanvas'
import { buildMemoryGraph } from './memoryGraph'
import type { MemorySource } from '../lib/types'

const EMPTY_MEMORY_SOURCES: MemorySource[] = []

export function Memories() {
  useDocumentTitle('Memories')
  const { data, isLoading } = useMemorySources()
  const sources = data ?? EMPTY_MEMORY_SOURCES
  const [selected, setSelected] = useState<string | null>(null)
  const [q, setQ] = useState('')
  const [provider, setProvider] = useState('')

  const providers = useMemo(() => {
    const seen = new Set<string>()
    for (const source of sources) {
      if (source.provider) seen.add(source.provider)
    }
    return Array.from(seen).sort()
  }, [sources])

  const graph = useMemo(() => buildMemoryGraph(sources, { query: q, provider }), [sources, q, provider])
  const selectedId = selected && graph.nodes.some((node) => node.id === selected) ? selected : graph.nodes[0]?.id ?? null
  const active = selectedId ? sources.find((source) => source.id === selectedId) : undefined

  const backlinks = useMemo<Backlink[]>(() => {
    if (!active) return []
    const name = active.label.trim().toLowerCase()
    return sources.flatMap((source) =>
      source.id !== active.id && wikiRefs(source.content ?? '').includes(name)
        ? [{ name: source.label, onOpen: () => setSelected(source.id) }]
        : [],
    )
  }, [sources, active])

  if (isLoading) return <div className="page"><Loading rows={5} /></div>
  if (sources.length === 0) {
    return (
      <div className="page">
        <EmptyState icon={<Brain size={30} />} title="No agent memory sources" hint="Agent instruction files surface here." />
      </div>
    )
  }

  const saveActive = async (text: string, version?: string) => {
    if (!active) return
    await apiPost('/api/memory', { path: active.path, text, mtime: version })
    await queryClient.invalidateQueries({ queryKey: ['memory-sources'] })
  }

  const onWikiLink = (target: string) => {
    const t = target.toLowerCase()
    const hit = sources.find((source) => source.label.trim().toLowerCase() === t)
    if (hit) setSelected(hit.id)
  }

  return (
    <div className="page flush">
      <div className="memory-graph-page">
        <div className="memory-graph-toolbar">
          <div>
            <div className="eyebrow">agent memory</div>
            <div className="h-lg">
              {graph.nodes.length}
              {graph.nodes.length !== sources.length ? ` / ${sources.length}` : ''} sources
            </div>
          </div>
          <div className="memory-graph-filters">
            <div className="input-icon">
              <Search size={14} className="dim" />
              <input
                className="input"
                aria-label="Filter agent memory sources"
                placeholder="Filter by name, path, or scope..."
                value={q}
                onChange={(event) => setQ(event.target.value)}
              />
            </div>
            {providers.length > 1 ? (
              <div className="chips">
                <button type="button" className={`chip${provider === '' ? ' active' : ''}`} onClick={() => setProvider('')}>
                  all
                </button>
                {providers.map((p) => (
                  <button
                    type="button"
                    key={p}
                    className={`chip${provider === p ? ' active' : ''}`}
                    onClick={() => setProvider((cur) => (cur === p ? '' : p))}
                  >
                    {p}
                  </button>
                ))}
              </div>
            ) : null}
          </div>
        </div>

        <div className="memory-graph-shell">
          <div className="memory-graph-main">
            {graph.nodes.length === 0 ? (
              <div className="memory-graph-empty">No agent memory sources match.</div>
            ) : (
              <MemoryGraphCanvas
                graph={graph}
                selectedId={selectedId}
                onSelectNode={(node) => setSelected(node.id)}
                onClearSelection={() => setSelected(null)}
              />
            )}
          </div>
          <aside className="memory-graph-inspector">
            {active ? (
              <div className="memory-graph-inspector-inner">
                <div className="eyebrow" style={{ marginBottom: 4 }}>{active.scope} · {active.kind}</div>
                <div className="detail-title" style={{ fontSize: 19 }}>{active.label}</div>
                <div className="detail-ref" style={{ marginBottom: 16 }}>{active.path}</div>
                {active.error ? (
                  <div className="error-note">{active.error}</div>
                ) : active.available ? (
                  <DocEditor
                    key={active.id}
                    content={active.content ?? ''}
                    version={active.mtime}
                    onSave={saveActive}
                    onWikiLink={onWikiLink}
                    backlinks={backlinks}
                  />
                ) : (
                  <div className="faint">This agent memory source is unavailable for editing.</div>
                )}
              </div>
            ) : (
              <div className="memory-graph-inspector-empty">Select an agent memory source.</div>
            )}
          </aside>
        </div>
      </div>
    </div>
  )
}
