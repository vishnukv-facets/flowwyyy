import { useState } from 'react'
import { useLocation } from 'wouter'
import { ArrowLeft, CalendarClock, Check, ChevronLeft, ChevronRight, HelpCircle, Loader2, Pause, Pencil, Play, Trash2, X } from 'lucide-react'
import { usePlaybook, useAction } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { BriefPanel } from '../components/BriefPanel'
import { ErrorNote, Loading, ProviderIcon, StatusDot } from '../components/ui'
import { useFloatTip } from '../components/FloatTip'
import { ago, dateTime, until } from '../lib/format'
import { clickable } from '../lib/a11y'
import type { PlaybookView, RunSummary } from '../lib/types'

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

// A readable 30-day run-history bar chart. run_days_30[i] is the run count for
// (today − (29 − i)) days, so index 29 is today. Hover shows the day + count.
function RunBars({ days }: { days: number[] }) {
  const { show, hide, portal } = useFloatTip()
  const data = days.length ? days : new Array(30).fill(0)
  const max = Math.max(1, ...data)
  const dateFor = (i: number) => {
    const d = new Date()
    d.setHours(0, 0, 0, 0)
    d.setDate(d.getDate() - (data.length - 1 - i))
    return d
  }
  return (
    <div className="actbars pb-runbars">
      {portal}
      {data.map((v, i) => {
        const d = dateFor(i)
        return (
          <i
            key={i}
            className={v > 0 ? 'on' : ''}
            style={{ height: `${Math.max(4, Math.round((v / max) * 100))}%` }}
            onMouseEnter={(e) =>
              show(
                e.currentTarget,
                <div className="ftip-head">
                  <span className="ftip-count">{v === 0 ? 'No runs' : `${v} run${v === 1 ? '' : 's'}`}</span>
                  <span className="ftip-date">{`${WEEKDAYS[d.getDay()]}, ${MONTHS[d.getMonth()]} ${d.getDate()}`}</span>
                </div>,
              )
            }
            onMouseLeave={hide}
          />
        )
      })}
    </div>
  )
}

// The schedule grammar mirrors internal/schedule/schedule.go — keep these
// examples in sync with what Parse() actually accepts. Each is clickable and
// fills the editor, so they double as quick-pick presets.
const SCHEDULE_HELP: { label: string; examples: string[] }[] = [
  { label: 'Every', examples: ['every hour', 'every 6 hours', 'every 30 minutes'] },
  { label: 'Presets', examples: ['daily', 'weekly', 'monthly', 'yearly'] },
  { label: 'Time of day', examples: ['daily at 9am', 'every day at 18:00', 'daily at midnight'] },
  { label: 'Weekdays', examples: ['Wednesday at 1pm', 'mon, wed, fri at 5pm', 'monday to friday at 9am'] },
  { label: 'Shorthands', examples: ['weekdays at 9am', 'weekends at 10am'] },
  { label: 'Day of month', examples: ['on the 1st at 9am', 'the 1st and 15th at 9am', '15th of every month at midnight'] },
  { label: 'Specific date', examples: ['January 1 at midnight', 'Dec 25 at 8am'] },
  { label: 'Raw cron', examples: ['0 13 * * 1-5', '*/15 * * * *'] },
]

// ScheduleHelp is the click-toggled reference of accepted schedule phrasings.
// Clicking an example hands it back via onPick so the editor is pre-filled.
function ScheduleHelp({ onPick }: { onPick: (example: string) => void }) {
  return (
    <div
      style={{
        marginTop: 10,
        padding: '12px 14px',
        background: 'var(--bg-2)',
        border: '1px solid var(--border)',
        borderRadius: 'var(--r)',
        boxShadow: 'var(--shadow-pop)',
      }}
    >
      <div className="faint" style={{ fontSize: 11.5, marginBottom: 10 }}>
        Type plain English or a cron expression. Click an example to use it.
      </div>
      <div style={{ display: 'grid', gap: 8 }}>
        {SCHEDULE_HELP.map((row) => (
          <div key={row.label} style={{ display: 'flex', gap: 10, alignItems: 'baseline' }}>
            <span
              className="eyebrow"
              style={{ flex: '0 0 92px', fontSize: 10, textAlign: 'right', paddingTop: 2 }}
            >
              {row.label}
            </span>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
              {row.examples.map((ex) => (
                <button
                  key={ex}
                  type="button"
                  className="kbd"
                  style={{ cursor: 'pointer', background: 'var(--bg-1)' }}
                  onClick={() => onPick(ex)}
                  title={`Use "${ex}"`}
                >
                  {ex}
                </button>
              ))}
            </div>
          </div>
        ))}
      </div>
      <div className="faint" style={{ fontSize: 11, marginTop: 10 }}>
        Cron fields: <span className="mono">minute hour day-of-month month day-of-week</span>. Names like{' '}
        <span className="mono">MON-FRI</span> and <span className="mono">JAN</span> work in raw cron too.
      </div>
    </div>
  )
}

// SchedulePanel shows and edits a playbook's recurring schedule. All mutations
// route through the set-playbook-schedule action, which shells out to
// `flow update playbook` so parsing + next-fire computation stay server-side.
function SchedulePanel({ pb, action }: { pb: PlaybookView; action: ReturnType<typeof useAction> }) {
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState('')
  const [showHelp, setShowHelp] = useState(false)
  const scheduled = !!pb.schedule
  const providerLimited = pb.schedule_hold_reason === 'provider_limit' && !!pb.schedule_hold_until

  const save = () => {
    const v = value.trim()
    if (!v) {
      setEditing(false)
      return
    }
    action.mutate(
      { kind: 'set-playbook-schedule', target: pb.slug, schedule_op: 'set', schedule: v },
      { onSuccess: () => setEditing(false) },
    )
  }
  const op = (schedule_op: 'clear' | 'pause' | 'resume') =>
    action.mutate({ kind: 'set-playbook-schedule', target: pb.slug, schedule_op })

  // Clicking a help example pre-fills the editor and opens it.
  const pick = (example: string) => {
    setValue(example)
    setEditing(true)
    setShowHelp(false)
  }

  return (
    <section className="card" style={{ padding: '16px 18px', marginBottom: 24 }}>
      <div className="bento-head" style={{ marginBottom: 12 }}>
        <span className="eyebrow">Schedule</span>
        <button
          className="btn icon ghost sm"
          aria-label="Schedule format help"
          aria-expanded={showHelp}
          title="What can I type here?"
          onClick={() => setShowHelp((v) => !v)}
          style={{ marginLeft: 4, color: showHelp ? 'var(--accent)' : undefined }}
        >
          <HelpCircle size={14} />
        </button>
        <div className="spacer" />
        {scheduled && !editing && (
          <span className={`badge ${pb.schedule_paused ? 'warn' : 'ok'}`}>
            {providerLimited ? 'limited' : pb.schedule_paused ? 'paused' : 'active'}
          </span>
        )}
      </div>

      {showHelp && <ScheduleHelp onPick={pick} />}

      {editing ? (
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          <input
            className="input"
            style={{ flex: 1, minWidth: 240 }}
            autoFocus
            placeholder='e.g. "every 6 hours", "Wednesday at 1pm", or a cron expression'
            value={value}
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                save()
              } else if (e.key === 'Escape') {
                e.preventDefault()
                setEditing(false)
              }
            }}
          />
          <button className="btn primary sm" onClick={save} disabled={action.isPending}>
            {action.isPending ? <Loader2 size={14} className="spin" /> : <Check size={14} />} Save
          </button>
          <button className="btn ghost sm" onClick={() => setEditing(false)} disabled={action.isPending}>
            Cancel
          </button>
        </div>
      ) : scheduled ? (
        <>
          <div className="meta-v" style={{ fontSize: 15 }}>
            {pb.schedule} {pb.schedule_spec && <span className="faint mono" style={{ fontSize: 12 }}>· {pb.schedule_spec}</span>}
          </div>
          <div className="faint" style={{ fontSize: 12, marginTop: 6 }}>
            {providerLimited && pb.schedule_hold_until
              ? `Provider credits limited — paused until ${until(pb.schedule_hold_until)} · ${dateTime(pb.schedule_hold_until)}`
              : pb.schedule_paused
              ? 'Paused — will not fire until resumed.'
              : pb.next_fire_at
                ? `Next run ${until(pb.next_fire_at)} · ${dateTime(pb.next_fire_at)}`
                : 'No next run scheduled.'}
            {pb.last_fired_at && ` · last ran ${ago(pb.last_fired_at)}`}
          </div>
          <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
            <button className="btn ghost sm" onClick={() => { setValue(pb.schedule || ''); setEditing(true) }} disabled={action.isPending}>
              <Pencil size={13} /> Edit
            </button>
            {pb.schedule_paused ? (
              <button className="btn ghost sm" onClick={() => op('resume')} disabled={action.isPending}>
                <Play size={13} /> Resume
              </button>
            ) : (
              <button className="btn ghost sm" onClick={() => op('pause')} disabled={action.isPending}>
                <Pause size={13} /> Pause
              </button>
            )}
            <button className="btn ghost sm danger" onClick={() => op('clear')} disabled={action.isPending}>
              <Trash2 size={13} /> Clear
            </button>
          </div>
          <div className="faint" style={{ fontSize: 11, marginTop: 10 }}>
            Scheduled runs fire automatically in headless (--auto) mode — no terminal tab.
          </div>
        </>
      ) : (
        <>
          <div className="faint">Not scheduled — this playbook runs only when you trigger it.</div>
          <button className="btn ghost sm" style={{ marginTop: 10 }} onClick={() => { setValue(''); setEditing(true) }} disabled={action.isPending}>
            <CalendarClock size={14} /> Add schedule
          </button>
        </>
      )}
    </section>
  )
}

// A playbook accrues a run per scheduled/manual fire, so the list grows without
// bound (a 5-min schedule = ~288/day). Page client-side, 10 at a time, and show
// each run's provider — mirrors ProjectTaskList on the project page.
const PLAYBOOK_RUNS_PAGE_SIZE = 10

function PlaybookRunList({ runs, onOpen }: { runs: RunSummary[]; onOpen: (slug: string) => void }) {
  const [page, setPage] = useState(0)

  if (runs.length === 0) {
    return (
      <div className="rows">
        <div className="lrow"><span className="faint">No runs yet. Hit “Run playbook”.</span></div>
      </div>
    )
  }

  const pageCount = Math.ceil(runs.length / PLAYBOOK_RUNS_PAGE_SIZE)
  // Clamp on render so a shrinking list never strands us on an empty page.
  const safePage = Math.min(page, pageCount - 1)
  const start = safePage * PLAYBOOK_RUNS_PAGE_SIZE
  const visible = runs.slice(start, start + PLAYBOOK_RUNS_PAGE_SIZE)

  return (
    <>
      <div className="rows">
        {visible.map((r) => (
          <div key={r.slug} className="lrow" aria-label={`Open run ${r.name}`} {...clickable(() => onOpen(r.slug))}>
            <StatusDot status={r.status} />
            <ProviderIcon provider={r.provider} size={14} />
            <div className="lrow-main">
              <div className="lrow-title clip">{r.name}</div>
              <div className="lrow-sub clip">{r.status} · {ago(r.created_at)}</div>
            </div>
            <span className={`prio ${r.priority}`} />
          </div>
        ))}
      </div>
      {pageCount > 1 && (
        <div className="row gap" style={{ justifyContent: 'space-between', alignItems: 'center', marginTop: 10 }}>
          <button className="btn icon ghost sm" aria-label="Previous runs" disabled={safePage === 0} onClick={() => setPage(safePage - 1)}>
            <ChevronLeft size={14} />
          </button>
          <span className="faint" style={{ fontSize: 11 }}>
            {start + 1}–{Math.min(start + PLAYBOOK_RUNS_PAGE_SIZE, runs.length)} of {runs.length}
          </span>
          <button className="btn icon ghost sm" aria-label="Next runs" disabled={safePage >= pageCount - 1} onClick={() => setPage(safePage + 1)}>
            <ChevronRight size={14} />
          </button>
        </div>
      )}
    </>
  )
}

export function PlaybookDetail({ slug }: { slug: string }) {
  const [, navigate] = useLocation()
  const { data: pb, isLoading, error } = usePlaybook(slug)
  const action = useAction()
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState('')
  useDocumentTitle(pb?.name)

  if (isLoading) return <div className="page"><Loading /></div>
  if (error) return <div className="page"><ErrorNote error={error} /></div>
  if (!pb) return null

  const runPlaybook = () => {
    action.mutate(
      { kind: 'spawn-run', target: slug },
      { onSuccess: (data) => data.agent && navigate(`/session/${data.agent.slug}`) },
    )
  }

  const startRename = () => {
    setName(pb.name)
    setEditing(true)
  }
  const saveRename = () => {
    const trimmed = name.trim()
    if (!trimmed || trimmed === pb.name) {
      setEditing(false)
      return
    }
    action.mutate(
      { kind: 'update-playbook', slug: pb.slug, name: trimmed },
      { onSuccess: () => setEditing(false) },
    )
  }

  return (
    <div className="page">
      <button className="btn ghost sm" style={{ marginBottom: 14 }} onClick={() => navigate('/playbooks')}>
        <ArrowLeft size={14} /> Playbooks
      </button>

      <div className="detail-head">
        <div style={{ flex: 1 }}>
          <div className="eyebrow">playbook</div>
          {editing ? (
            <input
              className="input inline-rename"
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  saveRename()
                } else if (e.key === 'Escape') {
                  e.preventDefault()
                  setEditing(false)
                }
              }}
            />
          ) : (
            <div className="detail-title">{pb.name}</div>
          )}
          <div className="detail-ref">{pb.slug}{pb.project_slug ? ` · ${pb.project_slug}` : ''}</div>
        </div>
        {editing ? (
          <>
            <button className="btn icon ghost sm" title="Save" aria-label="Save" onClick={saveRename} disabled={action.isPending}>
              {action.isPending ? <Loader2 size={14} className="spin" /> : <Check size={14} />}
            </button>
            <button className="btn icon ghost sm" title="Cancel" aria-label="Cancel" onClick={() => setEditing(false)} disabled={action.isPending}>
              <X size={14} />
            </button>
          </>
        ) : (
          <>
            <button className="btn icon ghost sm" title="Rename playbook" aria-label="Rename playbook" onClick={startRename}>
              <Pencil size={13} />
            </button>
            <button className="btn primary" disabled={action.isPending} onClick={runPlaybook}>
              <Play size={15} /> Run playbook
            </button>
          </>
        )}
      </div>

      <div className="meta-grid" style={{ marginBottom: 16 }}>
        <div className="meta-cell"><div className="meta-k">working dir</div><div className="meta-v mono clip" title={pb.work_dir}>{pb.work_dir}</div></div>
        <div className="meta-cell"><div className="meta-k">runs · 7d</div><div className="meta-v num">{pb.run_count_7d}</div></div>
        <div className="meta-cell">
          <div className="meta-k">runs · 30d</div>
          <div className="meta-v num">{(pb.run_days_30 ?? []).reduce((a, b) => a + b, 0)}</div>
        </div>
      </div>

      <section className="card" style={{ padding: '16px 18px', marginBottom: 24 }}>
        <div className="bento-head" style={{ marginBottom: 12 }}>
          <span className="eyebrow">Run activity</span>
          <div className="spacer" />
          <span className="faint mono" style={{ fontSize: 10 }}>last 30 days</span>
        </div>
        <RunBars days={pb.run_days_30 ?? []} />
      </section>

      <SchedulePanel pb={pb} action={action} />

      <div className="grid two">
        <section className="section">
          <div className="section-head"><span className="eyebrow">Definition</span></div>
          <div className="card" style={{ padding: 18 }}>
            <BriefPanel
              getPath={`/api/playbooks/${encodeURIComponent(slug)}/brief`}
              putPath={`/api/playbooks/${encodeURIComponent(slug)}/brief`}
              empty="No playbook definition yet."
            />
          </div>
        </section>

        <section className="section">
          <div className="section-head">
            <span className="eyebrow">Recent runs</span>
            <span className="section-count">{pb.recent_runs?.length ?? 0}</span>
          </div>
          <PlaybookRunList runs={pb.recent_runs ?? []} onOpen={(s) => navigate(`/session/${s}`)} />
        </section>
      </div>
    </div>
  )
}
