import { useEffect, useState } from 'react'
import { BookText } from 'lucide-react'
import { useKB, useMarkdown } from '../lib/query'
import { EmptyState, Loading } from '../components/ui'
import { Md } from '../components/Markdown'

export function KnowledgeBase() {
  const { data, isLoading } = useKB()
  const [selected, setSelected] = useState<string | null>(null)

  useEffect(() => {
    if (!selected && data && data.length) setSelected(data[0].filename)
  }, [data, selected])

  if (isLoading) return <div className="page"><Loading rows={5} /></div>
  if (!data || data.length === 0) {
    return (
      <div className="page">
        <EmptyState icon={<BookText size={30} />} title="Knowledge base empty" hint="flow seeds KB files under ~/.flow/kb." />
      </div>
    )
  }

  return (
    <div className="page flush">
      <div className="twopane">
        <div className="pane-list">
          <div className="pane-list-head">
            <div className="eyebrow">knowledge base</div>
            <div className="h-lg">{data.length} documents</div>
          </div>
          {data.map((f) => (
            <div key={f.filename} className={`pli${selected === f.filename ? ' active' : ''}`} onClick={() => setSelected(f.filename)}>
              <div className="pli-top">
                <BookText size={14} className="dim" />
                <span className="pli-title clip">{f.filename.replace(/\.md$/, '')}</span>
                <span className="faint mono" style={{ fontSize: 10.5 }}>{f.entries}</span>
              </div>
              <div className="pli-snippet">{f.preview || '—'}</div>
            </div>
          ))}
        </div>
        <div className="pane-detail">{selected && <KBDoc filename={selected} />}</div>
      </div>
    </div>
  )
}

function KBDoc({ filename }: { filename: string }) {
  const { data, isLoading } = useMarkdown(`/api/kb/${encodeURIComponent(filename)}`)
  return (
    <div style={{ padding: '24px 28px', maxWidth: 820 }}>
      <div className="eyebrow" style={{ marginBottom: 12 }}>{filename}</div>
      {isLoading ? <Loading rows={4} /> : <Md source={data || ''} />}
    </div>
  )
}
