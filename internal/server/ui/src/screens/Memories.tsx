import { useMemo, useState } from 'react'
import { Brain, Search, X } from 'lucide-react'
import { useMemorySources, queryClient } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { apiPost } from '../lib/api'
import { EmptyState, Loading } from '../components/ui'
import { DocEditor, type Backlink } from '../components/DocEditor'
import { wikiRefs } from '../lib/wikiRefs'
import { MemoryGraphCanvas } from '../components/memoryGraph/MemoryGraphCanvas'
import { buildMemoryGraph, isGraphableMemorySource } from './memoryGraph'
import type { MemorySource } from '../lib/types'

const EMPTY_MEMORY_SOURCES: MemorySource[] = []

export function Memories() {
  useDocumentTitle('Memories')
  const { data, isLoading } = useMemorySources()
  const sources = data ?? EMPTY_MEMORY_SOURCES
  const graphSources = useMemo(() => sources.filter(isGraphableMemorySource), [sources])
  const [selected, setSelected] = useState<string | null>(null)
  const [detailId, setDetailId] = useState<string | null>(null)
  const [q, setQ] = useState('')
  const [provider, setProvider] = useState('')

  const providers = useMemo(() => {
    const seen = new Set<string>()
    for (const source of graphSources) {
      if (source.provider) seen.add(source.provider)
    }
    return Array.from(seen).sort()
  }, [graphSources])

  const graph = useMemo(() => buildMemoryGraph(graphSources, { query: q, provider }), [graphSources, q, provider])
  const selectedId = selected && graph.nodes.some((node) => node.id === selected) ? selected : null
  const activeId = detailId && graph.nodes.some((node) => node.id === detailId) ? detailId : null
  const active = activeId ? graphSources.find((source) => source.id === activeId) : undefined

  const backlinks = useMemo<Backlink[]>(() => {
    if (!active) return []
    const name = active.label.trim().toLowerCase()
    return graphSources.flatMap((source) =>
      source.id !== active.id && wikiRefs(source.content ?? '').includes(name)
        ? [{ name: source.label, onOpen: () => setSelected(source.id) }]
        : [],
    )
  }, [graphSources, active])

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
    const hit = graphSources.find((source) => source.label.trim().toLowerCase() === t)
    if (hit) {
      setSelected(hit.id)
      setDetailId(hit.id)
    }
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
                onOpenDetails={(node) => {
                  setSelected(node.id)
                  setDetailId(node.id)
                }}
                onClearSelection={() => setSelected(null)}
              />
            )}
          </div>
          <aside className={`memory-graph-inspector${active ? ' open' : ''}`} aria-hidden={!active}>
            {active ? (
              <div className="memory-graph-inspector-inner">
                <div className="memory-graph-inspector-head">
                  <div>
                    <div className="eyebrow" style={{ marginBottom: 4 }}>{active.scope} · {active.kind}</div>
                    <div className="detail-title" style={{ fontSize: 19 }}>{active.label}</div>
                  </div>
                  <button type="button" className="memory-graph-inspector-close" aria-label="Close memory details" onClick={() => setDetailId(null)}>
                    <X size={15} />
                  </button>
                </div>
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
              <div className="memory-graph-inspector-empty">Select Details on a memory node.</div>
            )}
          </aside>
        </div>
      </div>
    </div>
  )
}
