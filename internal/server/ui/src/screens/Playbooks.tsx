import { useEffect, useMemo, useState } from 'react'
import { useLocation } from 'wouter'
import { Archive, CalendarClock, ChevronDown, Play, Plus, Repeat, Search, Trash2 } from 'lucide-react'
import { usePlaybooks, useAction, useUiData } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { confirmAction } from '../lib/confirm'
import { AgentPicker, PermissionPicker } from '../components/pickers'
import { EmptyState, ErrorNote, Loading, Sparkline } from '../components/ui'
import { clickable } from '../lib/a11y'
import { CreatePlaybookModal } from '../components/modals'
import { ago, until } from '../lib/format'
import type { ToolCapability } from '../lib/types'

const SORTS = [
  { v: 'recent', label: 'Recent' },
  { v: 'name', label: 'Name' },
  { v: 'runs', label: 'Runs · 7d' },
  { v: 'last', label: 'Last run' },
] as const
type SortKey = (typeof SORTS)[number]['v']

export function Playbooks() {
  useDocumentTitle('Playbooks')
  const [, navigate] = useLocation()
  const [q, setQ] = useState('')
  const [sort, setSort] = useState<SortKey>('recent')
  const [showArchived, setShowArchived] = useState(false)
  const { data, isLoading, error } = usePlaybooks({ include_archived: showArchived })
  const { data: ui } = useUiData()
  const action = useAction()
  const providers = ui?.CAPABILITIES?.providers ?? []
  const [createOpen, setCreateOpen] = useState(false)

  const lastRunAt = (p: { recent_runs?: { created_at: string }[] }) =>
    p.recent_runs?.[0] ? Date.parse(p.recent_runs[0].created_at) : 0
  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (data ?? [])
      .filter((p) => {
        if (!needle) return true
        return (
          p.name.toLowerCase().includes(needle) ||
          p.slug.toLowerCase().includes(needle) ||
          (p.project_slug ?? '').toLowerCase().includes(needle)
        )
      })
      .slice()
      .sort((a, b) => {
        if (sort === 'name') return a.name.localeCompare(b.name)
        if (sort === 'runs') return b.run_count_7d - a.run_count_7d
        if (sort === 'last') return lastRunAt(b) - lastRunAt(a)
        return Date.parse(b.updated_at) - Date.parse(a.updated_at)
      })
  }, [data, q, sort])

  // Close any open run-options popover when clicking outside it (same idiom as
  // the SessionDetail more-actions menu — native <details> won't self-close).
  useEffect(() => {
    const onDown = (e: globalThis.MouseEvent) => {
      document.querySelectorAll('details.menu[open]').forEach((d) => {
        if (!d.contains(e.target as Node)) d.removeAttribute('open')
      })
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [])

  const runWith = (slug: string, opts: { provider?: string; permission_mode?: string }) => {
    action.mutate(
      { kind: 'spawn-run', target: slug, ...opts },
      { onSuccess: (d) => d.agent && navigate(`/session/${d.agent.slug}`) },
    )
  }

  const trash = async (e: React.MouseEvent, slug: string, name: string) => {
    e.stopPropagation()
    const ok = await confirmAction({
      title: 'Move this playbook to trash?',
      body: `"${name}" will be soft-deleted and hidden from your lists. Past runs are unaffected and you can restore it from Trash later.`,
      confirmLabel: 'Move to trash',
      danger: true,
    })
    if (ok) action.mutate({ kind: 'delete', target: slug, entity_kind: 'playbook' })
  }

  const archive = async (e: React.MouseEvent, slug: string, name: string) => {
    e.stopPropagation()
    const ok = await confirmAction({
      title: 'Archive this playbook?',
      body: `"${name}" will leave your active list. Past runs are unaffected and you can unarchive it later.`,
      confirmLabel: 'Archive',
      danger: true,
    })
    if (ok) action.mutate({ kind: 'archive', target: slug, entity_kind: 'playbook' })
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">repeatable workflows</div>
          <h1 className="h-xl">Playbooks</h1>
        </div>
        <button type="button" className="btn primary" onClick={() => setCreateOpen(true)}>
          <Plus size={15} /> New playbook
        </button>
      </div>

      {!isLoading && !error && data && data.length > 0 && (
        <div className="row gap wrap" style={{ marginBottom: 18, gap: 14, alignItems: 'center' }}>
          <div className="input-icon" style={{ maxWidth: 280 }}>
            <Search size={14} className="dim" />
            <input
              className="input"
              placeholder="Filter by name, slug, or project…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          <div className="segmented">
            {SORTS.map((s) => (
              <button key={s.v} className={sort === s.v ? 'active' : ''} onClick={() => setSort(s.v)}>
                {s.label}
              </button>
            ))}
          </div>
          <div className="chips">
            <button
              className={`chip${showArchived ? ' active' : ''}`}
              aria-pressed={showArchived}
              onClick={() => setShowArchived((v) => !v)}
            >
              <Archive size={12} /> archived
            </button>
          </div>
          <div className="spacer" />
          <span className="faint mono" style={{ fontSize: 12 }}>
            {shown.length}
            {shown.length !== (data?.length ?? 0) ? ` / ${data?.length}` : ''}
          </span>
        </div>
      )}

      {isLoading ? (
        <Loading rows={4} />
      ) : error ? (
        <ErrorNote error={error} />
      ) : !data || data.length === 0 ? (
        <EmptyState icon={<Repeat size={30} />} title="No playbooks" hint="Playbooks are reusable task templates an agent runs on demand." />
      ) : shown.length === 0 ? (
        <EmptyState icon={<Repeat size={30} />} title="No playbooks match" hint="Adjust the filter or toggle archived." />
      ) : (
        <div className="grid cards stagger">
          {shown.map((p) => {
            const archived = !!p.archived_at
            const providerLimited = p.schedule_hold_reason === 'provider_limit' && !!p.schedule_hold_until
            return (
            <article
              key={p.slug}
              className={`card acard${archived ? ' archived' : ''}`}
              aria-label={`Open playbook ${p.name}`}
              {...clickable(() => navigate(`/playbook/${p.slug}`))}
            >
              <div className="acard-top">
                <Repeat size={16} className="dim" style={{ marginTop: 2 }} />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div className="acard-title clip">{p.name}</div>
                  <div className="acard-ref clip">{p.project_slug || 'no project'}</div>
                </div>
                {archived ? (
                  <span className="tag">archived</span>
                ) : (
                  <>
                    <PlaybookRunControl
                      providers={providers}
                      pending={action.isPending}
                      onRun={(opts) => runWith(p.slug, opts)}
                    />
                    <button
                      className="btn icon ghost sm row-action"
                      title="Archive playbook"
                      aria-label="Archive playbook"
                      onClick={(e) => archive(e, p.slug, p.name)}
                    >
                      <Archive size={14} />
                    </button>
                    <button
                      className="btn icon ghost sm row-action"
                      title="Move to trash"
                      aria-label="Move playbook to trash"
                      onClick={(e) => trash(e, p.slug, p.name)}
                    >
                      <Trash2 size={14} />
                    </button>
                  </>
                )}
              </div>
              <div className="acard-foot" style={{ borderTop: 'none', paddingTop: 0 }}>
                <span className="num" style={{ fontSize: 12.5 }}>
                  <b>{p.run_count_7d}</b> <span className="faint">runs · 7d</span>
                </span>
                <div className="spacer" />
                <Sparkline data={p.run_days_30?.slice(-14) ?? []} />
              </div>
              {p.recent_runs?.[0] && (
                <div className="faint" style={{ fontSize: 12 }}>last run {ago(p.recent_runs[0].created_at)}</div>
              )}
              {p.schedule && (
                <div className="faint" style={{ fontSize: 12, display: 'flex', alignItems: 'center', gap: 5 }}>
                  <CalendarClock size={11} />
                  {providerLimited && p.schedule_hold_until
                    ? <>limited · paused until {until(p.schedule_hold_until)}</>
                    : p.schedule_paused
                      ? <>schedule paused</>
                    : p.next_fire_at
                      ? <>{p.schedule} · next {until(p.next_fire_at)}</>
                      : <>{p.schedule}</>}
                </div>
              )}
            </article>
            )
          })}
        </div>
      )}
      <CreatePlaybookModal open={createOpen} onClose={() => setCreateOpen(false)} />
    </div>
  )
}

// Split "Run" button: a plain click spawns a run with the stored defaults
// (claude / default), the caret opens a popover to pick agent + permission mode
// before launching — surfacing spawn-run's Provider/PermissionMode inputs the
// quick button hard-defaulted. stopPropagation keeps clicks off the card's
// navigate-to-detail handler.
function PlaybookRunControl({
  providers,
  pending,
  onRun,
}: {
  providers: ToolCapability[]
  pending: boolean
  onRun: (opts: { provider?: string; permission_mode?: string }) => void
}) {
  const [provider, setProvider] = useState('claude')
  const [perm, setPerm] = useState('default')
  return (
    <div className="split-run" onClick={(e) => e.stopPropagation()}>
      <button className="btn primary sm split-main" onClick={() => onRun({})} disabled={pending}>
        <Play size={13} /> Run
      </button>
      <details className="menu">
        <summary className="btn primary sm split-caret" title="Run options">
          <ChevronDown size={13} />
        </summary>
        <div className="menu-pop right run-opts">
          <div className="run-opts-row">
            <span className="eyebrow">Agent</span>
            <AgentPicker value={provider} onChange={setProvider} providers={providers} />
          </div>
          <div className="run-opts-row">
            <span className="eyebrow">Permissions</span>
            <PermissionPicker value={perm} onChange={setPerm} />
          </div>
          <button
            className="btn primary sm"
            disabled={pending}
            onClick={(e) => {
              ;(e.currentTarget as HTMLElement).closest('details')?.removeAttribute('open')
              onRun({ provider, permission_mode: perm })
            }}
          >
            <Play size={13} /> Run with options
          </button>
        </div>
      </details>
    </div>
  )
}
