import { useEffect, useRef, useState } from 'react'
import { AlertTriangle, Link2, Loader2, Pencil, RotateCcw, Save } from 'lucide-react'
import { Md } from './Markdown'
import { confirmAction } from '../lib/confirm'
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
  version,
  onSave,
  onWikiLink,
  backlinks,
}: {
  content: string
  version?: string
  onSave: (text: string, version?: string) => Promise<void>
  onWikiLink?: (name: string) => void
  backlinks?: Backlink[]
}) {
  const rootRef = useRef<HTMLDivElement>(null)
  const skipNextNavigationRef = useRef(false)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(content)
  const [base, setBase] = useState({ content, version: version ?? '' })
  const [conflict, setConflict] = useState(false)
  const [saving, setSaving] = useState(false)
  const dirty = editing && draft !== base.content
  const sourceChanged = editing && (content !== base.content || (version ?? '') !== base.version)

  // Keep the draft in sync with the source while not actively editing (e.g.
  // when the user switches to a different doc).
  useEffect(() => {
    if (!editing) {
      setDraft(content)
      setBase({ content, version: version ?? '' })
      setConflict(false)
    }
  }, [content, version, editing])

  useEffect(() => {
    if (sourceChanged) setConflict(true)
  }, [sourceChanged])

  useEffect(() => {
    if (!dirty) return
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault()
      event.returnValue = ''
    }
    const onClick = (event: globalThis.MouseEvent) => {
      const target = event.target as Element | null
      if (!target || rootRef.current?.contains(target)) return
      if (target.closest('.modal')) return
      const interactive = target.closest('a, button, summary, [role="button"], .pli')
      if (!interactive) return
      if (skipNextNavigationRef.current) {
        skipNextNavigationRef.current = false
        return
      }
      event.preventDefault()
      event.stopPropagation()
      void confirmAction({
        title: 'Discard unsaved changes?',
        body: 'This draft has not been saved. Leave the editor and discard it?',
        confirmLabel: 'Discard',
        cancelLabel: 'Keep editing',
        danger: true,
      }).then((ok) => {
        if (!ok) return
        skipNextNavigationRef.current = true
        setEditing(false)
        setDraft(content)
        ;(interactive as HTMLElement).click()
      })
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    document.addEventListener('click', onClick, true)
    return () => {
      window.removeEventListener('beforeunload', onBeforeUnload)
      document.removeEventListener('click', onClick, true)
    }
  }, [dirty])

  const beginEdit = () => {
    setBase({ content, version: version ?? '' })
    setDraft(content)
    setConflict(false)
    setEditing(true)
  }

  const reloadLatest = () => {
    setBase({ content, version: version ?? '' })
    setDraft(content)
    setConflict(false)
  }

  const save = async () => {
    if (conflict || sourceChanged) {
      setConflict(true)
      pushToast('error', 'File changed on disk. Reload latest before saving.')
      return
    }
    setSaving(true)
    try {
      await onSave(draft, base.version || undefined)
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
      <div className="col" style={{ gap: 10 }} ref={rootRef}>
        {conflict && (
          <div className="error-note row gap" style={{ alignItems: 'center' }}>
            <AlertTriangle size={14} />
            <span style={{ flex: 1 }}>This file changed on disk after editing started. Reload latest before saving.</span>
            <button className="btn sm" onClick={reloadLatest}>
              <RotateCcw size={13} /> Reload
            </button>
          </div>
        )}
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
            {dirty ? 'Unsaved changes' : 'Markdown'} · <span className="mono">[[name]]</span> links to another doc
          </span>
          <div className="spacer" />
          <button className="btn sm" onClick={() => { setEditing(false); setDraft(content) }}>
            Cancel
          </button>
          <button className="btn primary sm" disabled={saving || conflict || sourceChanged} onClick={save}>
            {saving ? <Loader2 size={13} className="spin" /> : <Save size={13} />} Save
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="col" style={{ gap: 14 }} ref={rootRef}>
      <div className="row">
        <div className="spacer" />
        <button className="btn ghost sm" onClick={beginEdit}>
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
