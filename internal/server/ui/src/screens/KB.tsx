import { useEffect, useMemo, useState } from 'react'
import { BookText } from 'lucide-react'
import { useKB } from '../lib/query'
import { queryClient } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { apiPutText } from '../lib/api'
import { EmptyState, Loading } from '../components/ui'
import { DocEditor, wikiRefs, type Backlink } from '../components/DocEditor'
import type { KBFileView } from '../lib/types'

const baseName = (filename: string) => filename.replace(/\.md$/, '')

export function KnowledgeBase() {
  useDocumentTitle('Knowledge Base')
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
                <span className="pli-title clip">{baseName(f.filename)}</span>
                <span className="faint mono" style={{ fontSize: 10.5 }}>{f.entries}</span>
              </div>
              <div className="pli-snippet">{f.preview || '—'}</div>
            </div>
          ))}
        </div>
        <div className="pane-detail">
          {selected && <KBDoc files={data} filename={selected} onSelect={setSelected} />}
        </div>
      </div>
    </div>
  )
}

function KBDoc({ files, filename, onSelect }: { files: KBFileView[]; filename: string; onSelect: (f: string) => void }) {
  const file = files.find((f) => f.filename === filename)
  const name = baseName(filename).toLowerCase()

  // Docs that reference this one via [[name]] — resolved within the KB set.
  const backlinks = useMemo<Backlink[]>(() => {
    return files
      .filter((f) => f.filename !== filename && wikiRefs(f.content).includes(name))
      .map((f) => ({ name: baseName(f.filename), onOpen: () => onSelect(f.filename) }))
  }, [files, filename, name, onSelect])

  if (!file) return null

  const save = async (text: string, version?: string) => {
    await apiPutText(`/api/kb/${encodeURIComponent(filename)}`, text, { mtime: version })
    await queryClient.invalidateQueries({ queryKey: ['kb'] })
    await queryClient.invalidateQueries({ queryKey: ['md', `/api/kb/${encodeURIComponent(filename)}`] })
  }

  const onWikiLink = (target: string) => {
    const t = target.toLowerCase()
    const hit = files.find((f) => baseName(f.filename).toLowerCase() === t)
    if (hit) onSelect(hit.filename)
  }

  return (
    <div style={{ padding: '24px 28px', maxWidth: 820 }}>
      <div className="eyebrow" style={{ marginBottom: 12 }}>{filename}</div>
      <DocEditor
        content={file.content || ''}
        version={file.mtime}
        onSave={save}
        onWikiLink={onWikiLink}
        backlinks={backlinks}
      />
    </div>
  )
}
