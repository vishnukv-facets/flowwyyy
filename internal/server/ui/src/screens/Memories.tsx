import { useMemo, useState } from 'react'
import { Brain, Search } from 'lucide-react'
import { useMemorySources, queryClient } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { apiPost } from '../lib/api'
import { EmptyState, Loading } from '../components/ui'
import { DocEditor, wikiRefs, type Backlink } from '../components/DocEditor'
import { clickable } from '../lib/a11y'
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
  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return sources.filter((s) => {
      if (provider && s.provider !== provider) return false
      if (!needle) return true
      return (
        s.label.toLowerCase().includes(needle) ||
        s.path.toLowerCase().includes(needle) ||
        (s.scope ?? '').toLowerCase().includes(needle)
      )
    })
  }, [sources, provider, q])

  const selectedId = selected && filtered.some((s) => s.id === selected) ? selected : filtered[0]?.id ?? null
  const active = filtered.find((s) => s.id === selectedId)

  // Memories that reference the active one via [[label]] — resolved across the
  // full source set (not just the filtered view). Computed before any early
  // return so the hook order stays stable.
  const backlinks = useMemo<Backlink[]>(() => {
    if (!active) return []
    const name = active.label.trim().toLowerCase()
    return sources.flatMap((s) =>
      s.id !== active.id && wikiRefs(s.content ?? '').includes(name)
        ? [{ name: s.label, onOpen: () => setSelected(s.id) }]
        : [],
    )
  }, [sources, active])

  if (isLoading) return <div className="page"><Loading rows={5} /></div>
  if (sources.length === 0) {
    return (
      <div className="page">
        <EmptyState icon={<Brain size={30} />} title="No memory sources" hint="Agent memory files (CLAUDE.md, AGENTS.md, flow memories) surface here." />
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
    const hit = sources.find((s) => s.label.trim().toLowerCase() === t)
    if (hit) setSelected(hit.id)
  }

  return (
    <div className="page flush">
      <div className="twopane">
        <div className="pane-list">
          <div className="pane-list-head">
            <div className="eyebrow">agent memory</div>
            <div className="h-lg">
              {filtered.length}
              {filtered.length !== sources.length ? ` / ${sources.length}` : ''} sources
            </div>
            <div className="pane-filter">
              <div className="input-icon">
                <Search size={14} className="dim" />
                <input
                  className="input"
                  aria-label="Filter memory sources"
	                  placeholder="Filter by name, path, or scope…"
	                  value={q}
	                  onChange={(e) => setQ(e.target.value)}
	                />
              </div>
              {providers.length > 1 && (
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
              )}
            </div>
          </div>
          {filtered.length === 0 ? (
            <div className="faint" style={{ padding: '18px 14px', fontSize: 13 }}>No sources match.</div>
          ) : (
            filtered.map((s) => (
              <div
                key={s.id}
                className={`pli${selectedId === s.id ? ' active' : ''}`}
	                aria-pressed={selectedId === s.id}
	                {...clickable(() => setSelected(s.id))}
	              >
	                <div className="pli-top">
	                  <span className={`dot ${s.available ? 'running' : 'idle'}`} />
	                  <span className="pli-title clip">{s.label}</span>
	                  <span className="faint mono" style={{ fontSize: 12 }}>{s.provider}</span>
                </div>
                <div className="pli-snippet mono">{s.path}</div>
              </div>
            ))
          )}
        </div>
        <div className="pane-detail">
          {active && (
            <div style={{ padding: '24px 28px', maxWidth: 820 }}>
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
                <div className="faint">This source is unavailable for editing.</div>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
