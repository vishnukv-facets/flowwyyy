import { useEffect, useState } from 'react'
import { Link2, Loader2, Pencil, Save } from 'lucide-react'
import { Md } from './Markdown'
import { pushToast } from '../lib/toast'

export interface Backlink {
  name: string
  onOpen: () => void
}

// Inline markdown viewer/editor for KB docs and agent memories. View mode
// renders markdown with [[wiki-link]] cross-references + a "Linked from"
// backlinks strip; Edit mode is a plain textarea saved via onSave.
export function DocEditor({
  content,
  onSave,
  onWikiLink,
  backlinks,
}: {
  content: string
  onSave: (text: string) => Promise<void>
  onWikiLink?: (name: string) => void
  backlinks?: Backlink[]
}) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(content)
  const [saving, setSaving] = useState(false)

  // Keep the draft in sync with the source while not actively editing (e.g.
  // when the user switches to a different doc).
  useEffect(() => {
    if (!editing) setDraft(content)
  }, [content, editing])

  const save = async () => {
    setSaving(true)
    try {
      await onSave(draft)
      pushToast('ok', 'Saved')
      setEditing(false)
    } catch (e) {
      pushToast('error', e instanceof Error ? e.message : 'save failed')
    } finally {
      setSaving(false)
    }
  }

  if (editing) {
    return (
      <div className="col" style={{ gap: 10 }}>
        <textarea
          className="textarea doc-editor"
          value={draft}
          autoFocus
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') save()
            if (e.key === 'Escape') {
              setEditing(false)
              setDraft(content)
            }
          }}
        />
        <div className="row gap">
          <span className="faint" style={{ fontSize: 11.5 }}>
            Markdown · <span className="mono">[[name]]</span> links to another doc
          </span>
          <div className="spacer" />
          <button className="btn sm" onClick={() => { setEditing(false); setDraft(content) }}>
            Cancel
          </button>
          <button className="btn primary sm" disabled={saving} onClick={save}>
            {saving ? <Loader2 size={13} className="spin" /> : <Save size={13} />} Save
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="col" style={{ gap: 14 }}>
      <div className="row">
        <div className="spacer" />
        <button className="btn ghost sm" onClick={() => { setDraft(content); setEditing(true) }}>
          <Pencil size={13} /> Edit
        </button>
      </div>
      {content.trim() ? <Md source={content} onWikiLink={onWikiLink} /> : <div className="faint">This document is empty.</div>}
      {backlinks && backlinks.length > 0 && (
        <div className="backlinks">
          <div className="eyebrow" style={{ marginBottom: 8 }}><Link2 size={12} /> Linked from</div>
          <div className="row gap wrap">
            {backlinks.map((b) => (
              <button key={b.name} className="backlink-chip" onClick={b.onOpen}>{b.name}</button>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

// Extract the [[wiki-link]] target names referenced in a markdown body.
export function wikiRefs(content: string): string[] {
  const out: string[] = []
  const re = /\[\[([^\]\n]+)\]\]/g
  let m: RegExpExecArray | null
  while ((m = re.exec(content || '')) !== null) out.push(m[1].trim().toLowerCase())
  return out
}
