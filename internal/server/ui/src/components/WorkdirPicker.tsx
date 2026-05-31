import { useEffect, useState } from 'react'
import {
  Folder,
  FolderGit2,
  FolderPlus,
  ChevronRight,
  CornerLeftUp,
  Check,
  X,
  Loader2,
} from 'lucide-react'
import { apiGet, apiPost } from '../lib/api'
import { pushToast } from '../lib/toast'
import type { FSEntriesView, FSEntryView } from '../lib/types'

// Universal working-directory picker: a path input paired with an inline
// directory browser. Folders are navigable; a new folder can be created in
// place (POST /api/fs/mkdir). The selected workdir is always the directory
// currently being browsed, so navigating == choosing.
export function WorkdirPicker({
  value,
  onChange,
  placeholder,
}: {
  value: string
  onChange: (path: string) => void
  placeholder?: string
}) {
  const [open, setOpen] = useState(false)
  const [view, setView] = useState<FSEntriesView | null>(null)
  const [loading, setLoading] = useState(false)
  const [creating, setCreating] = useState(false)
  const [newName, setNewName] = useState('')
  const [showHidden, setShowHidden] = useState(false)

  const load = async (path: string, select = true) => {
    setLoading(true)
    try {
      const v = await apiGet<FSEntriesView>(`/api/fs/entries?path=${encodeURIComponent(path || '~')}`)
      setView(v)
      if (select) onChange(v.path)
    } catch (e) {
      pushToast('error', e instanceof Error ? e.message : 'cannot open directory')
    } finally {
      setLoading(false)
    }
  }

  // Open the browser at the typed path (or home) the first time it expands.
  useEffect(() => {
    if (open && !view) void load(value.trim() || '~', false)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  const createDir = async () => {
    const name = newName.trim()
    if (!name || !view) return
    try {
      const made = await apiPost<FSEntryView>('/api/fs/mkdir', { parent: view.path, name })
      setNewName('')
      setCreating(false)
      await load(made.path)
      pushToast('ok', `created ${made.display_path}`)
    } catch (e) {
      pushToast('error', e instanceof Error ? e.message : 'could not create folder')
    }
  }

  const dirs = (view?.entries ?? []).filter((e) => e.is_dir && (showHidden || !e.hidden))

  return (
    <div className="wdpick">
      <div className="wdpick-control">
        <input
          className="input mono"
          placeholder={placeholder || '/Users/you/code/project'}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
        <button
          type="button"
          className={`btn icon ghost wdpick-toggle${open ? ' active' : ''}`}
          title="Browse folders"
          aria-label="Browse folders"
          onClick={() => setOpen((o) => !o)}
        >
          <Folder size={16} />
        </button>
      </div>

      {open && (
        <div className="wdpick-panel">
          <div className="wdpick-crumbs">
            {(view?.breadcrumbs ?? []).map((b, i) => (
              <span key={b.path} className="wdpick-crumb">
                {i > 0 && <ChevronRight size={12} className="faint" />}
                <button type="button" onClick={() => load(b.path)}>
                  {b.name}
                </button>
              </span>
            ))}
            <div className="spacer" />
            {view?.parent && (
              <button
                type="button"
                className="btn icon ghost sm"
                title="Up one level"
                onClick={() => load(view.parent!)}
              >
                <CornerLeftUp size={14} />
              </button>
            )}
          </div>

          <div className="wdpick-list">
            {loading ? (
              <div className="wdpick-empty">
                <Loader2 size={14} className="spin" /> loading…
              </div>
            ) : dirs.length === 0 ? (
              <div className="wdpick-empty">No subfolders here.</div>
            ) : (
              dirs.map((d) => (
                <button type="button" key={d.path} className="wdpick-row" onClick={() => load(d.path)}>
                  {d.is_git ? (
                    <FolderGit2 size={15} style={{ color: 'var(--accent)' }} />
                  ) : (
                    <Folder size={15} className="dim" />
                  )}
                  <span className="clip">{d.name}</span>
                  {d.is_git && <span className="tag">git</span>}
                  <div className="spacer" />
                  <ChevronRight size={13} className="faint" />
                </button>
              ))
            )}
          </div>

          <div className="wdpick-foot">
            {creating ? (
              <div className="wdpick-newdir">
                <FolderPlus size={14} className="dim" />
                <input
                  className="input sm mono"
                  autoFocus
                  placeholder="new-folder-name"
                  value={newName}
                  onChange={(e) => setNewName(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault()
                      void createDir()
                    } else if (e.key === 'Escape') {
                      setCreating(false)
                      setNewName('')
                    }
                  }}
                />
                <button type="button" className="btn icon primary sm" title="Create" onClick={createDir}>
                  <Check size={14} />
                </button>
                <button
                  type="button"
                  className="btn icon ghost sm"
                  title="Cancel"
                  onClick={() => {
                    setCreating(false)
                    setNewName('')
                  }}
                >
                  <X size={14} />
                </button>
              </div>
            ) : (
              <button type="button" className="btn ghost sm" onClick={() => setCreating(true)}>
                <FolderPlus size={14} /> New folder
              </button>
            )}
            <div className="spacer" />
            <label className="wdpick-hidden">
              <input
                type="checkbox"
                checked={showHidden}
                onChange={(e) => setShowHidden(e.target.checked)}
              />
              hidden
            </label>
            <button type="button" className="btn primary sm" onClick={() => setOpen(false)}>
              <Check size={14} /> Use this folder
            </button>
          </div>

          <div className="wdpick-current mono" title={view?.path}>
            {view?.display_path || value || '—'}
          </div>
        </div>
      )}
    </div>
  )
}
