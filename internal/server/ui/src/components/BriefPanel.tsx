import { useState } from 'react'
import { Loader2, Pencil } from 'lucide-react'
import { Md } from './Markdown'
import { Loading } from './ui'
import { apiPutText } from '../lib/api'
import { pushToast } from '../lib/toast'
import { queryClient, useMarkdown } from '../lib/query'

// Renders a markdown brief; if putPath is given, supports inline editing that
// PUTs the raw markdown back over the WS-RPC channel (raw-text body).
export function BriefPanel({
  getPath,
  putPath,
  empty = 'No brief written yet.',
}: {
  getPath: string
  putPath?: string
  empty?: string
}) {
  const { data, isLoading } = useMarkdown(getPath)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const [saving, setSaving] = useState(false)

  const startEdit = () => {
    setDraft(data ?? '')
    setEditing(true)
  }
  const save = async () => {
    if (!putPath) return
    setSaving(true)
    try {
      await apiPutText(putPath, draft)
      await queryClient.invalidateQueries({ queryKey: ['md', getPath] })
      pushToast('ok', 'Brief saved')
      setEditing(false)
    } catch (e) {
      pushToast('error', e instanceof Error ? e.message : 'save failed')
    } finally {
      setSaving(false)
    }
  }

  if (isLoading) return <Loading rows={3} />

  if (editing) {
    return (
      <div className="col" style={{ gap: 10 }}>
        <textarea
          className="textarea mono"
          style={{ minHeight: 320, fontSize: 12.5 }}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
        />
        <div className="row gap">
          <div className="spacer" />
          <button className="btn sm" onClick={() => setEditing(false)}>Cancel</button>
          <button className="btn primary sm" disabled={saving} onClick={save}>
            {saving ? <Loader2 size={14} className="spin" /> : null} Save brief
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="brief-panel">
      {putPath && (
        <button className="btn ghost sm brief-edit" onClick={startEdit}>
          <Pencil size={13} /> Edit
        </button>
      )}
      {data?.trim() ? <Md source={data} /> : <div className="faint">{empty}</div>}
    </div>
  )
}
