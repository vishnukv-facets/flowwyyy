import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { AlertTriangle, BookText, Check, Loader2, Moon, Plus, RotateCcw, ShieldCheck, Sparkles, Trash2 } from 'lucide-react'
import { useKB, useKBDream, useBackupStatus, useBackupLog } from '../lib/query'
import { queryClient } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { apiPost, apiPutText } from '../lib/api'
import { pushToast } from '../lib/toast'
import { ago, countdown, dateTime } from '../lib/format'
import { useNow } from '../lib/useNow'
import { EmptyState, Loading } from '../components/ui'
import { DocEditor, wikiRefs, type Backlink } from '../components/DocEditor'
import { clickable } from '../lib/a11y'
import { CreateKBModal } from '../components/modals'
import type { KBDreamStatus, KBFileView, BackupStatus, BackupCommit, BackupRunRecord } from '../lib/types'

const baseName = (filename: string) => filename.replace(/\.md$/, '')

// flaggedRe matches a "Pending removal" bullet ("- [flagged YYYY-MM-DD] …").
// Mirrors the server's flaggedBulletRe so the Dreaming panel can show how many
// entries are currently flagged and gate the "Clean up flagged" action.
const flaggedRe = /^\s*-\s*\[flagged \d{4}-\d{2}-\d{2}\]/gm

function countFlagged(files: KBFileView[]): number {
  let n = 0
  for (const f of files) n += (f.content || '').match(flaggedRe)?.length ?? 0
  return n
}

// The KB is the five durable things flow knows about. Each file maps to a
// pillar with its own accent so the taxonomy reads at a glance. Unknown files
// fall back to indigo (the page's primary accent).
type PillarKey = 'accent' | 'accent-2' | 'warn' | 'info' | 'ok'
const PILLARS: Record<string, PillarKey> = {
  user: 'accent',
  org: 'accent-2',
  products: 'warn',
  processes: 'info',
  business: 'ok',
}
function pillarOf(filename: string): PillarKey {
  return PILLARS[baseName(filename).toLowerCase()] ?? 'accent'
}
const titleCase = (s: string) => (s ? s[0].toUpperCase() + s.slice(1) : s)

// A tinted rounded-square glyph: the uppercased first letter of the basename in
// mono, colored per pillar via CSS custom props consumed by `.kb-glyph`.
function PillarGlyph({ filename, size = 28 }: { filename: string; size?: number }) {
  const key = pillarOf(filename)
  const letter = (baseName(filename)[0] || '?').toUpperCase()
  return (
    <span
      className="kb-glyph"
      aria-hidden
      style={{
        width: size,
        height: size,
        ['--glyph' as string]: `var(--${key})`,
        ['--glyph-soft' as string]: `color-mix(in srgb, var(--${key}) 16%, transparent)`,
        ['--glyph-line' as string]: `color-mix(in srgb, var(--${key}) 45%, transparent)`,
      }}
    >
      {letter}
    </span>
  )
}

export function KnowledgeBase() {
  useDocumentTitle('Knowledge Base')
  const { data, isLoading } = useKB()
  const [selected, setSelected] = useState<string | null>(null)
  const [createOpen, setCreateOpen] = useState(false)

  useEffect(() => {
    if (!selected && data && data.length) setSelected(data[0].filename)
  }, [data, selected])

  // How many entries are sitting in "Pending removal" across all files — drives
  // the count badge on the Dreaming panel's "Clean up flagged" action.
  const flaggedCount = useMemo(() => countFlagged(data ?? []), [data])

  if (isLoading) return <div className="page"><Loading rows={5} /></div>
  const files = data ?? []
  if (files.length === 0) {
    return (
      <div className="page">
        <div className="page-head">
          <div>
            <div className="eyebrow">knowledge base</div>
            <h1 className="h-xl">Knowledge</h1>
            <div className="page-sub">Durable context flow carries into every session.</div>
          </div>
          <span className="spacer" />
          <button type="button" className="btn primary" onClick={() => setCreateOpen(true)}>
            <Plus size={15} /> New document
          </button>
        </div>
        <div className="kb-sys">
          <DreamPanel flaggedCount={flaggedCount} />
          <BackupPanel />
        </div>
        <EmptyState icon={<BookText size={30} />} title="Knowledge base empty" hint="flow seeds KB files under ~/.flow/kb." />
        <CreateKBModal open={createOpen} onClose={() => setCreateOpen(false)} onCreated={setSelected} />
      </div>
    )
  }

  return (
    <div className="page flush">
      <div className="page-head" style={{ padding: '22px 28px 0', marginBottom: 0 }}>
        <div>
          <div className="eyebrow">knowledge base</div>
          <h1 className="h-xl">Knowledge</h1>
          <div className="page-sub">Durable context flow carries into every session.</div>
        </div>
        <span className="spacer" />
        <button type="button" className="btn primary" onClick={() => setCreateOpen(true)}>
          <Plus size={15} /> New document
        </button>
      </div>
      <div className="kb-sys" style={{ padding: '16px 28px 0' }}>
        <DreamPanel flaggedCount={flaggedCount} />
        <BackupPanel />
      </div>
      <div className="twopane" style={{ marginTop: 14 }}>
        <div className="pane-list">
          <div className="pane-list-head kb-list-head">
            <div className="eyebrow">documents</div>
            <span className="kb-count">{files.length}</span>
          </div>
          {files.map((f) => (
            <div
              key={f.filename}
              className={`pli${selected === f.filename ? ' active' : ''}`}
              aria-pressed={selected === f.filename}
              {...clickable(() => setSelected(f.filename))}
            >
              <div className="pli-top">
                <PillarGlyph filename={f.filename} />
                <span className="pli-title clip">{titleCase(baseName(f.filename))}</span>
                <span className="kb-count">{f.entries}</span>
              </div>
              <div className="pli-snippet">{f.preview || '—'}</div>
            </div>
          ))}
        </div>
        <div className="pane-detail">
          {selected && <KBDoc files={files} filename={selected} onSelect={setSelected} />}
        </div>
      </div>
      <CreateKBModal open={createOpen} onClose={() => setCreateOpen(false)} onCreated={setSelected} />
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
      <div className="kb-doc-head">
        <PillarGlyph filename={filename} size={30} />
        <div className="kb-doc-headtext">
          <div className="h-lg">{titleCase(baseName(filename))}</div>
          <div className="eyebrow">{filename}</div>
        </div>
        <span className="kb-count">{file.entries}</span>
      </div>
      <DocEditor
        content={file.content || ''}
        version={file.mtime}
        onSave={save}
        onWikiLink={onWikiLink}
        backlinks={backlinks}
      />
      <FileHistory relpath={`kb/${filename}`} onRestored={() => queryClient.invalidateQueries({ queryKey: ['kb'] })} />
    </div>
  )
}

// FileHistory shows the backup version history for one curated file and lets the
// operator restore any prior version — the recovery path for an accidental wipe
// or a bad edit, without scraping transcripts.
function FileHistory({ relpath, onRestored }: { relpath: string; onRestored: () => void }) {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)
  const { data, isLoading } = useBackupLog(relpath, open)
  const versions = data ?? []

  const restore = async (rev: string) => {
    if (!window.confirm('Restore this version? Your current content is checkpointed first, so this is reversible.')) return
    setBusy(rev)
    try {
      await apiPost('/api/backup/restore', { file: relpath, rev })
      pushToast('ok', 'restored from backup')
      await queryClient.invalidateQueries({ queryKey: ['backup-log', relpath] })
      onRestored()
    } catch {
      pushToast('error', 'restore failed')
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="kb-dream" style={{ marginTop: 16 }}>
      <div className="kb-dream-row">
        <span className="kb-dream-icon"><RotateCcw size={15} /></span>
        <div className="kb-dream-text">
          <div className="kb-dream-title">Version history</div>
          <div className="kb-dream-sub dim">Every backed-up version of this file. Restore rolls it back (reversibly).</div>
        </div>
        <div className="kb-dream-actions">
          <button type="button" className="btn ghost sm" onClick={() => setOpen((v) => !v)}>
            {open ? 'Hide' : 'Show versions'}
          </button>
        </div>
      </div>
      {open && (
        <div className="kb-dream-history">
          {isLoading ? (
            <div className="kb-dream-empty"><Loader2 size={18} className="spin" /></div>
          ) : versions.length === 0 ? (
            <div className="kb-dream-empty">
              <RotateCcw size={22} />
              <div className="kb-dream-empty-title">No versions yet</div>
              <div className="kb-dream-empty-hint">A version is saved before every change and on a schedule.</div>
            </div>
          ) : (
            <ul className="kb-dream-list">
              {versions.map((c: BackupCommit, i: number) => (
                <li key={c.rev} className="kb-dream-item">
                  <span className="kb-dream-when" title={dateTime(c.when)}>{ago(c.when)}</span>
                  <span className="kb-dream-detail clip">{c.subject}</span>
                  <button
                    type="button"
                    className="btn ghost sm"
                    disabled={busy === c.rev || i === 0}
                    title={i === 0 ? 'This is the current version' : 'Restore this version'}
                    onClick={() => restore(c.rev)}
                  >
                    {busy === c.rev ? <Loader2 size={13} className="spin" /> : <RotateCcw size={13} />} Restore
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  )
}

// BackupPanel surfaces the ~/.flow backup safety net: schedule, last/next run,
// commit + db-snapshot counts, offsite status, and a manual "Back up now".
function BackupPanel() {
  const { data, isLoading } = useBackupStatus()
  const [showHistory, setShowHistory] = useState(false)
  const [busy, setBusy] = useState(false)
  useNow(1000)

  if (isLoading || !data) return null
  const b: BackupStatus = data
  const history = b.history ?? []

  const backupNow = async () => {
    setBusy(true)
    try {
      await apiPost('/api/backup/now', {})
      pushToast('ok', 'backup taken')
      await queryClient.invalidateQueries({ queryKey: ['backup-status'] })
    } catch {
      pushToast('error', 'backup failed')
    } finally {
      setBusy(false)
    }
  }

  let statusChip: ReactNode
  let line: ReactNode
  if (!b.enabled) {
    statusChip = <span className="chip">off</span>
    line = <>Backups are disabled (<code>FLOW_BACKUP_ENABLED</code>).</>
  } else if (b.running || busy) {
    statusChip = <span className="chip active">backing up</span>
    line = <><Loader2 size={12} className="spin" /> Backing up…</>
  } else if (b.next_run_at) {
    statusChip = <span className="chip ok">active</span>
    line = (
      <>
        Next backup <strong>{countdown(b.next_run_at)}</strong>
        {b.last_run_at && <span className="dim"> · last {ago(b.last_run_at)}</span>}
      </>
    )
  } else {
    statusChip = <span className="chip ok">active</span>
    line = <span className="dim">{b.commits} checkpoint{b.commits === 1 ? '' : 's'} · {b.db_snapshots} db snapshot{b.db_snapshots === 1 ? '' : 's'}</span>
  }

  return (
    <div className="kb-dream kb-sys-card">
      <div className="kb-dream-row">
        <span className="kb-dream-icon"><ShieldCheck size={15} /></span>
        <div className="kb-dream-text">
          <div className="kb-dream-title">
            Backups {statusChip}
          </div>
          <div className="kb-dream-sub">{line}</div>
        </div>
        <div className="kb-dream-actions">
          {history.length > 0 && (
            <button type="button" className="btn ghost sm" onClick={() => setShowHistory((v) => !v)}>
              History · {history.length}
            </button>
          )}
          {b.enabled && (
            <button type="button" className="btn ok sm" onClick={backupNow} disabled={busy || b.running}>
              {busy || b.running ? <Loader2 size={14} className="spin" /> : <ShieldCheck size={14} />} Back up now
            </button>
          )}
        </div>
      </div>
      <div className="kb-sys-meta dim">
        {b.schedule ? `${b.schedule} · ` : ''}{b.commits} checkpoints · {b.db_snapshots} db snapshots · {b.remote_configured ? 'offsite on' : 'local only'}
        {b.remote_url ? (
          <>
            {' · '}
            <a href={b.remote_url.replace(/\.git$/, '')} target="_blank" rel="noreferrer">
              {b.remote_url.replace(/^https?:\/\/github\.com\//, '').replace(/\.git$/, '')}
            </a>
            {b.last_push_at ? <> · pushed {ago(b.last_push_at)}</> : <> · not pushed yet</>}
          </>
        ) : b.remote_configured ? (
          <> · offsite syncs on next backup</>
        ) : null}
      </div>
      {showHistory && history.length > 0 && (
        <div className="kb-dream-history">
          <ul className="kb-dream-list">
            {history.map((rec: BackupRunRecord, i: number) => (
              <li key={`${rec.at}-${i}`} className="kb-dream-item">
                <span className={`kb-dream-status ${rec.status}`} title={rec.status}>
                  {rec.status === 'error' ? <AlertTriangle size={13} /> : <Check size={13} />}
                </span>
                <span className="kb-dream-when" title={dateTime(rec.at)}>{ago(rec.at)}</span>
                <span className="kb-dream-detail">
                  {rec.status === 'error' ? rec.detail || 'backup failed' : `${rec.committed ? 'checkpoint' : 'no changes'}${rec.db_snapshot ? ' + db snapshot' : ''}${rec.pushed ? ' + pushed' : ''}`}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}

// DreamPanel surfaces the KB "dreaming" hygiene worker: when the next pass
// runs (live countdown), what recent passes did, and a manual trigger. The
// dreamer flags stale KB entries for removal and prunes ones left flagged too
// long — invisible background work that the operator otherwise can't see.
function DreamPanel({ flaggedCount }: { flaggedCount: number }) {
  const { data, isLoading } = useKBDream()
  const [showHistory, setShowHistory] = useState(false)
  const [busy, setBusy] = useState(false)
  const [purging, setPurging] = useState(false)
  useNow(1000) // tick the countdown live

  if (isLoading || !data) return null
  const d: KBDreamStatus = data
  const history = d.history ?? []
  // Fixed schedule label when set ("daily at 3am"), else the interval cadence.
  const cadence = d.schedule || `every ${Math.round(d.interval_ms / 3_600_000)}h`

  const dreamNow = async () => {
    setBusy(true)
    try {
      await apiPost<KBDreamStatus>('/api/kb/dream', {})
      pushToast('ok', 'dream pass started')
      await queryClient.invalidateQueries({ queryKey: ['kb-dream'] })
    } catch {
      pushToast('error', 'a dream pass is already running')
    } finally {
      setBusy(false)
    }
  }

  const cleanUpFlagged = async () => {
    setPurging(true)
    try {
      const res = await apiPost<{ pruned: number }>('/api/kb/prune', {})
      pushToast('ok', res.pruned > 0 ? `cleared ${res.pruned} flagged entr${res.pruned === 1 ? 'y' : 'ies'}` : 'nothing flagged to clear')
      await queryClient.invalidateQueries({ queryKey: ['kb'] })
      await queryClient.invalidateQueries({ queryKey: ['kb-dream'] })
    } catch {
      pushToast('error', 'could not clean up flagged entries')
    } finally {
      setPurging(false)
    }
  }

  // Primary status line.
  let statusChip: ReactNode
  let line: ReactNode
  if (!d.enabled) {
    statusChip = <span className="chip">off</span>
    line = <>KB hygiene is disabled (<code>FLOW_KB_DREAM_ENABLED</code>).</>
  } else if (d.running || busy) {
    statusChip = <span className="chip active">dreaming</span>
    line = (
      <>
        <Loader2 size={12} className="spin" /> Dreaming now — flagging stale entries…
      </>
    )
  } else if (d.next_run_at) {
    statusChip = <span className="chip ok">active</span>
    line = (
      <>
        Next pass <strong>{countdown(d.next_run_at)}</strong>
        {d.last_run_at && <span className="dim"> · last pass {ago(d.last_run_at)}</span>}
      </>
    )
  } else {
    statusChip = <span className="chip">idle</span>
    line = <span className="dim">No pass scheduled.</span>
  }

  return (
    <div className="kb-dream kb-sys-card">
      <div className="kb-dream-row">
        <span className="kb-dream-icon">
          <Moon size={15} />
        </span>
        <div className="kb-dream-text">
          <div className="kb-dream-title">
            Dreaming {statusChip}
          </div>
          <div className="kb-dream-sub">{line}</div>
        </div>
        <div className="kb-dream-actions">
          {flaggedCount > 0 && (
            <button
              type="button"
              className="btn ghost sm"
              onClick={cleanUpFlagged}
              disabled={purging}
              title="Remove all entries currently in 'Pending removal' across every KB file"
            >
              {purging ? <Loader2 size={14} className="spin" /> : <Trash2 size={14} />} Clean up {flaggedCount} flagged
            </button>
          )}
          <button type="button" className="btn ghost sm" onClick={() => setShowHistory((v) => !v)}>
            History{history.length ? ` · ${history.length}` : ''}
          </button>
          {d.enabled && (
            <button type="button" className="btn ok sm" onClick={dreamNow} disabled={busy || d.running}>
              {busy || d.running ? <Loader2 size={14} className="spin" /> : <Sparkles size={14} />} Dream now
            </button>
          )}
        </div>
      </div>
      <div className="kb-sys-meta dim">{cadence} · prunes after {d.max_age_days}d flagged</div>

      {showHistory && (
        <div className="kb-dream-history">
          {history.length === 0 ? (
            <div className="kb-dream-empty">
              <Moon size={22} />
              <div className="kb-dream-empty-title">No dream passes yet</div>
              <div className="kb-dream-empty-hint">
                {d.enabled
                  ? <>The first hygiene pass runs <strong>{d.next_run_at ? countdown(d.next_run_at) : 'soon'}</strong>. It flags stale or superseded KB entries for removal and prunes ones left flagged over {d.max_age_days} days — completed passes will be logged here.</>
                  : <>Enable dreaming (<code>FLOW_KB_DREAM_ENABLED</code>) to have flow tidy the knowledge base ({cadence}).</>}
              </div>
            </div>
          ) : (
            <ul className="kb-dream-list">
              {history.map((rec, i) => (
                <li key={`${rec.at}-${i}`} className="kb-dream-item">
                  <span className={`kb-dream-status ${rec.status}`} title={rec.status}>
                    {rec.status === 'error' ? <AlertTriangle size={13} /> : <Check size={13} />}
                  </span>
                  <span className="kb-dream-when" title={dateTime(rec.at)}>{ago(rec.at)}</span>
                  <span className="kb-dream-detail">
                    {rec.status === 'error'
                      ? rec.detail || 'pass failed'
                      : rec.pruned > 0
                        ? `pruned ${rec.pruned} stale entr${rec.pruned === 1 ? 'y' : 'ies'}`
                        : 'no stale entries to prune'}
                  </span>
                  {rec.duration_ms > 0 && <span className="dim kb-dream-dur">{(rec.duration_ms / 1000).toFixed(1)}s</span>}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  )
}
