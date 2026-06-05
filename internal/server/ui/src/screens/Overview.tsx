import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Link, useLocation } from 'wouter'
import { ArrowRight, Activity, BarChart3, CalendarClock, Coins, Repeat, AlertTriangle, Snowflake, TerminalSquare, TrendingUp, Flame, Inbox as InboxIcon, Loader2, SendHorizontal, Sparkles } from 'lucide-react'
import { useAction, useInbox, useOverview, useQuote, useTasks, useUiData } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { AgentCard } from '../components/AgentCard'
import { AgentPicker } from '../components/pickers'
import { useFloatingTerminals } from '../lib/floatingTerminals'
import { EmptyState, ErrorNote, Loading, ProviderIcon, SourceIcon, Sparkline, Stat } from '../components/ui'
import { useFloatTip } from '../components/FloatTip'
import { ago, compact, compactTokens, dueTone } from '../lib/format'
import { agendaCount, bucketByDue, type DueBuckets } from '../lib/agenda'
import { throughputByWeek, timeToDone, tokensByWeek } from '../lib/analytics'
import { clickable } from '../lib/a11y'
import type { ActivityDay, InboxFeedEntry, PlaybookRun, ProjectMC, TaskView, TokenDay, UiStats } from '../lib/types'

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

// Weekday + month-day from a YYYY-MM-DD string, built from local date parts so
// the weekday is correct (parsing the bare string as a Date would treat it as
// UTC midnight).
function fmtDay(iso: string): string {
  const [y, m, d] = iso.split('-').map(Number)
  const dt = new Date(y, m - 1, d)
  return `${WEEKDAYS[dt.getDay()]}, ${MONTHS[m - 1]} ${d}`
}

// Themed tooltip body for the daily task-activity bar chart.
function dayTip(d: ActivityDay): ReactNode {
  const head = (
    <div className="ftip-head">
      <span className="ftip-count">
        {d.count ? `${d.count} task${d.count === 1 ? '' : 's'} active` : 'No activity'}
      </span>
      <span className="ftip-date">{fmtDay(d.date)}</span>
    </div>
  )
  if (!d.tasks?.length) return head
  return (
    <>
      {head}
      <div className="ftip-tasks">
        {d.tasks.map((t) => (
          <span key={t} className="ftip-task clip">{t}</span>
        ))}
        {d.count > d.tasks.length && <span className="ftip-more">+{d.count - d.tasks.length} more</span>}
      </div>
    </>
  )
}

// Themed tooltip body for the 12-week token-usage heatmap: the day's total
// fresh "work" tokens plus which task burned how many.
function tokenDayTip(d: TokenDay): ReactNode {
  const head = (
    <div className="ftip-head">
      <span className="ftip-count">
        {d.tokens ? `${compactTokens(d.tokens)} tokens` : 'No tokens'}
      </span>
      <span className="ftip-date">{fmtDay(d.date)}</span>
    </div>
  )
  if (!d.tasks?.length) return head
  const more = (d.task_count ?? d.tasks.length) - d.tasks.length
  return (
    <>
      {head}
      <div className="ftip-tasks">
        {d.tasks.map((t) => (
          <span key={t.name} className="ftip-task ftip-task-tok">
            <span className="ftip-task-name clip">{t.name}</span>
            <span className="ftip-task-val mono">{compactTokens(t.tokens)}</span>
          </span>
        ))}
        {more > 0 && <span className="ftip-more">+{more} more</span>}
      </div>
    </>
  )
}

// Local YYYY-MM-DD for "today" — heatmap dates are YYYY-MM-DD, so string
// comparison is chronological. Dates after this are in the future.
function todayISO(): string {
  const n = new Date()
  return `${n.getFullYear()}-${String(n.getMonth() + 1).padStart(2, '0')}-${String(n.getDate()).padStart(2, '0')}`
}

// Greeting headline (time-of-day) plus the hour "bucket" that keys the anime
// quote. The headline still tracks morning/afternoon/evening/night, but the
// quote now rotates hourly: hourKey is "YYYY-MM-DD-HH", matching the server's
// quoteBucket() so the two stay in lockstep and the same quote is served for
// every refresh within the hour.
function greetingInfo(): { text: string; bucket: string; hourKey: string } {
  const d = new Date()
  const h = d.getHours()
  const pad = (n: number) => String(n).padStart(2, '0')
  const hourKey = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}-${pad(h)}`
  const text =
    h >= 5 && h < 12
      ? 'Good morning'
      : h >= 12 && h < 17
        ? 'Good afternoon'
        : h >= 17 && h < 21
          ? 'Good evening'
          : 'Good night' // 21:00–04:59
  const bucket = text.replace('Good ', '')
  return { text, bucket, hourKey }
}

// Re-evaluates each minute; re-renders only when the greeting bucket OR the
// hour flips, so the headline and its (hourly) quote update live without a
// page reload.
function useGreeting() {
  const [info, setInfo] = useState(greetingInfo)
  useEffect(() => {
    const id = setInterval(() => {
      setInfo((prev) => {
        const next = greetingInfo()
        return next.bucket === prev.bucket && next.hourKey === prev.hourKey ? prev : next
      })
    }, 60_000)
    return () => clearInterval(id)
  }, [])
  return info
}

// Mission Control analytics: activity-day streaks (the same active days that
// light up the 12-week calendar above) and per-provider tokens-used totals
// across every tracked session — the SUM of each session's "tok" pill, so the
// panel and the cards agree. Uses the real Claude/Codex logos so the split
// reads at a glance.
function StatsPanel({ stats }: { stats: UiStats }) {
  return (
    <div className="stats-panel">
      <div className="eyebrow stats-cap"><Flame size={13} /> Streak &amp; tokens</div>
      <div className="stats-streaks">
        <div className="stats-blk">
          <div className="num stats-num">{stats.current_streak}</div>
          <div className="stats-sub">current streak</div>
        </div>
        <div className="stats-blk">
          <div className="num stats-num">{stats.longest_streak}</div>
          <div className="stats-sub">longest streak</div>
        </div>
        <div className="stats-blk">
          <div className="num stats-num">{stats.active_days}</div>
          <div className="stats-sub">active · 12 wk</div>
        </div>
      </div>
      <div className="stats-tok-cap">tokens used · all sessions</div>
      <div className="stats-tokens">
        <div className="stats-tok-row">
          <span className="stats-tok-name"><ProviderIcon provider="claude" size={14} /> Claude</span>
          <span className="mono stats-tok-val">{compactTokens(stats.tokens_claude)}</span>
          <span className="faint mono stats-tok-sess">{stats.sessions_claude} sess</span>
        </div>
        <div className="stats-tok-row">
          <span className="stats-tok-name"><ProviderIcon provider="codex" size={14} /> Codex</span>
          <span className="mono stats-tok-val">{compactTokens(stats.tokens_codex)}</span>
          <span className="faint mono stats-tok-sess">{stats.sessions_codex} sess</span>
        </div>
        <div className="stats-tok-row stats-tok-total">
          <span className="stats-tok-name">Combined</span>
          <span className="mono stats-tok-val">{compactTokens(stats.tokens_total)}</span>
          <span className="faint mono stats-tok-sess">{stats.sessions_total} sess</span>
        </div>
      </div>
    </div>
  )
}

function heatLevel(count: number, max: number): number {
  if (count <= 0) return 0
  const r = count / Math.max(1, max)
  if (r > 0.66) return 4
  if (r > 0.4) return 3
  if (r > 0.15) return 2
  return 1
}

// GitHub-style contribution heatmap, coloured by daily token usage. Hovering a
// cell shows the day's total tokens and which task burned how many.
function MiniCalendar({ days }: { days: TokenDay[] }) {
  const today = todayISO()
  const { show, hide, portal } = useFloatTip()
  const max = Math.max(1, ...days.map((d) => d.tokens))
  const weeks: TokenDay[][] = []
  for (let i = 0; i < days.length; i += 7) weeks.push(days.slice(i, i + 7))
  const monthLabels = weeks.map((w, i) => {
    const first = w[0]
    if (!first) return ''
    const m = new Date(first.date).getMonth()
    const prev = i > 0 && weeks[i - 1][0] ? new Date(weeks[i - 1][0].date).getMonth() : -1
    return m !== prev ? MONTHS[m] : ''
  })
  return (
    <div className="cal">
      {portal}
      <div className="cal-months" style={{ gridTemplateColumns: `repeat(${weeks.length}, 15px)` }}>
        {monthLabels.map((m, i) => (
          <span key={i}>{m}</span>
        ))}
      </div>
      <div className="cal-body">
        {/* 7 row-slots aligned to the Sunday-first grid (row 1 = Sun … row 7 = Sat);
            labels only on Mon/Wed/Fri so they line up with the actual cell rows. */}
        <div className="cal-days">
          <span />
          <span>Mon</span>
          <span />
          <span>Wed</span>
          <span />
          <span>Fri</span>
          <span />
        </div>
        <div className="cal-grid">
          {weeks.map((w, wi) => (
            <div className="cal-col" key={wi}>
              {w.map((d) =>
                d.date > today ? (
                  // Future day in the current week — render an invisible spacer
                  // so the grid keeps its shape without implying inactivity.
                  <div key={d.date} className="cal-cell future" />
                ) : (
                  <div
                    key={d.date}
                    className={`cal-cell l${heatLevel(d.tokens, max)}`}
                    onMouseEnter={(e) => show(e.currentTarget, tokenDayTip(d))}
                    onMouseLeave={hide}
                  />
                ),
              )}
            </div>
          ))}
        </div>
      </div>
      <div className="cal-legend">
        <span>Less</span>
        <i className="cal-cell l0" />
        <i className="cal-cell l1" />
        <i className="cal-cell l2" />
        <i className="cal-cell l3" />
        <i className="cal-cell l4" />
        <span>More</span>
      </div>
    </div>
  )
}

// Daily activity trend — a compact bar chart of the last 28 days' action counts
// (distinct tasks touched per day). The 12-week calendar below is token-based.
function ActivityBars({ days }: { days: ActivityDay[] }) {
  const today = todayISO()
  const { show, hide, portal } = useFloatTip()
  // Drop future days so the chart ends at today (no trailing flat bars that
  // read as "inactive").
  const recent = days.filter((d) => d.date <= today).slice(-28)
  const max = Math.max(1, ...recent.map((d) => d.count))
  return (
    <div className="actbars">
      {portal}
      {recent.map((d) => (
        <i
          key={d.date}
          className={d.count > 0 ? 'on' : ''}
          style={{ height: `${Math.max(3, Math.round((d.count / max) * 100))}%` }}
          onMouseEnter={(e) => show(e.currentTarget, dayTip(d))}
          onMouseLeave={hide}
        />
      ))}
    </div>
  )
}

// Playbook run statuses → outcome label + colour. Runs are tasks, so `done`
// is the only terminal success; the rest report their live state honestly.
const RUN_TONE: Record<string, { label: string; cls: string }> = {
  done: { label: 'Succeeded', cls: 'ok' },
  'in-progress': { label: 'Running', cls: 'run' },
  waiting: { label: 'Waiting', cls: 'warn' },
  blocked: { label: 'Blocked', cls: 'danger' },
  backlog: { label: 'Queued', cls: 'muted' },
}
function runTone(status: string) {
  return RUN_TONE[status] ?? { label: status || 'Unknown', cls: 'muted' }
}

// Weekday, month-day and 24h time from an RFC3339 timestamp.
function fmtRunTime(iso: string): string {
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  return `${WEEKDAYS[d.getDay()]}, ${MONTHS[d.getMonth()]} ${d.getDate()} · ${hh}:${mm}`
}

// A run-history strip: one hoverable bar per recent run, coloured by outcome.
// Hover reveals when it ran and how it ended. Falls back to the count
// sparkline when per-run detail isn't available.
function PlaybookSpark({ runs, fallback }: { runs?: PlaybookRun[]; fallback: number[] }) {
  const { show, hide, portal } = useFloatTip()
  if (!runs || runs.length === 0) return <Sparkline data={fallback} />
  return (
    <span className="pbspark" onClick={(e) => e.stopPropagation()}>
      {portal}
      {runs.map((r, i) => {
        const tone = runTone(r.status)
        return (
          <i
            key={i}
            className={`pbrun ${tone.cls}`}
            onMouseEnter={(e) =>
              show(
                e.currentTarget,
                <>
                  <div className="ftip-head">
                    <span className="ftip-count">{tone.label}</span>
                    <span className="ftip-date">{ago(r.created_at)}</span>
                  </div>
                  <div className="ftip-tasks">
                    <span className="ftip-task clip">{r.name}</span>
                    <span className="ftip-more">{fmtRunTime(r.created_at)}</span>
                  </div>
                </>,
              )
            }
            onMouseLeave={hide}
          />
        )
      })}
    </span>
  )
}

// Near-term due-date agenda for the Mission Control main column: overdue /
// today / this week, each a soonest-first list. Reuses the .lrow row shape from
// the backlog section so it reads as part of the same console, not a new widget.
function AgendaBuckets({ due, onOpen }: { due: DueBuckets; onOpen: (slug: string) => void }) {
  const groups: { key: string; label: string; tasks: TaskView[] }[] = [
    { key: 'overdue', label: 'Overdue', tasks: due.overdue },
    { key: 'today', label: 'Today', tasks: due.today },
    { key: 'week', label: 'This week', tasks: due.week },
  ]
  return (
    <>
      {groups.map((g) =>
        g.tasks.length === 0 ? null : (
          <div key={g.key} className="agenda-bucket">
            <div className={`agenda-bucket-head ${g.key}`}>
              {g.label} <span className="faint">· {g.tasks.length}</span>
            </div>
            <div className="rows">
              {g.tasks.map((t) => (
                <div key={t.slug} className="lrow" onClick={() => onOpen(t.slug)}>
                  <span className={`prio ${t.priority}`} />
                  <ProviderIcon provider={t.session_provider} size={14} />
                  <div className="lrow-main">
                    <div className="lrow-title clip">{t.name}</div>
                    <div className="lrow-sub clip">
                      {t.project_slug || 'no project'}
                      {t.assignee ? ` · @${t.assignee}` : ''}
                    </div>
                  </div>
                  {t.due_info && (
                    <span
                      className={`badge ${dueTone(t.due_date, t.due_info)}`}
                      style={{ whiteSpace: 'nowrap', height: 'auto', padding: '3px 8px' }}
                      title={t.due_date ? `Due ${t.due_date}` : undefined}
                    >
                      {t.due_info}
                    </span>
                  )}
                </div>
              ))}
            </div>
          </div>
        ),
      )}
    </>
  )
}

// Compact duration: sub-day spans read as hours, otherwise days (1 decimal
// under 10d). Keeps the time-to-done stat legible at a glance.
function fmtDays(d: number): string {
  if (d < 1) return `${Math.round(d * 24)}h`
  return `${d < 10 ? d.toFixed(1) : Math.round(d)}d`
}

// Mission Control trends: throughput (tasks done/week) and token cost
// (fresh work tokens/week) as 12-week sparklines, plus median/avg time-to-done.
// All derived client-side from the task list + server TOKEN_SERIES.
function TrendsCard({ doneTasks, tokenSeries }: { doneTasks: TaskView[]; tokenSeries: TokenDay[] }) {
  const throughput = useMemo(() => throughputByWeek(doneTasks, new Date()), [doneTasks])
  const tokens = useMemo(() => tokensByWeek(tokenSeries), [tokenSeries])
  const ttd = useMemo(() => timeToDone(doneTasks), [doneTasks])
  const doneTotal = throughput.reduce((s, w) => s + w.value, 0)
  const tokenTotal = tokens.reduce((s, w) => s + w.value, 0)
  return (
    <section className="card rail-card">
      <div className="bento-head">
        <span className="eyebrow"><TrendingUp size={13} /> Trends</span>
        <div className="spacer" />
        <span className="faint mono" style={{ fontSize: 10 }}>12 weeks</span>
      </div>
      <div className="trend-row">
        <div className="trend-label">
          <span className="eyebrow">Throughput</span>
          <span className="faint mono">{doneTotal} done</span>
        </div>
        <Sparkline data={throughput.map((w) => w.value)} flex />
      </div>
      <div className="hairline" style={{ margin: '12px 0' }} />
      <div className="trend-row">
        <div className="trend-label">
          <span className="eyebrow"><Coins size={11} /> Token cost</span>
          <span className="faint mono">{compactTokens(tokenTotal)} fresh</span>
        </div>
        <Sparkline data={tokens.map((w) => w.value)} flex />
      </div>
      <div className="hairline" style={{ margin: '12px 0' }} />
      <div className="row" style={{ gap: 0 }}>
        <Stat label="median to done" value={ttd.count ? fmtDays(ttd.medianDays) : '—'} />
        <Stat label="avg to done" value={ttd.count ? fmtDays(ttd.avgDays) : '—'} />
        <Stat label="closed" value={ttd.count} />
      </div>
    </section>
  )
}

// Per-project completion: done/total as a progress bar per project. Reads
// PROJECTS_MC.task_counts (already shipped) — no extra data needed.
function ProjectProgressCard({ projects, onOpen }: { projects: ProjectMC[]; onOpen: (slug: string) => void }) {
  const shown = projects.filter((p) => p.tasks.total > 0).slice(0, 6)
  return (
    <section className="card rail-card">
      <div className="bento-head">
        <span className="eyebrow"><BarChart3 size={13} /> Project progress</span>
        <div className="spacer" />
        <Link href="/projects" className="btn ghost sm">All <ArrowRight size={13} /></Link>
      </div>
      {shown.length === 0 ? (
        <div className="faint" style={{ padding: 8 }}>No projects with tasks yet.</div>
      ) : (
        <div className="col" style={{ gap: 10 }}>
          {shown.map((p) => {
            const pct = Math.round((p.tasks.done / p.tasks.total) * 100)
            return (
              <div key={p.slug} className="proj-prog" onClick={() => onOpen(p.slug)}>
                <div className="proj-prog-head">
                  <span className="clip">{p.name}</span>
                  <span className="faint mono">{p.tasks.done}/{p.tasks.total}</span>
                </div>
                <div className="prog-track">
                  <div className="prog-fill" style={{ width: `${pct}%` }} />
                </div>
              </div>
            )
          })}
        </div>
      )}
    </section>
  )
}

export function Overview() {
  useDocumentTitle('Mission Control')
  const [, navigate] = useLocation()
  const [askFlow, setAskFlow] = useState('')
  const [askProvider, setAskProvider] = useState('claude')
  const { open: openFloatingTerminal } = useFloatingTerminals()
  const { data: ui, isLoading, error } = useUiData()
  const { data: overview } = useOverview()
  const { data: inbox } = useInbox()
  const action = useAction()
  // One task fetch (incl. done) feeds both the agenda lens and the analytics
  // trends: open tasks drive the due buckets, done tasks drive throughput and
  // time-to-done. A done task with a past due date must NOT show as "overdue",
  // so the agenda is built from the open subset only.
  const { data: allTasks } = useTasks({ include_done: true })
  const doneTasks = useMemo(() => (allTasks ?? []).filter((t) => t.status === 'done'), [allTasks])
  const due = useMemo(
    () => bucketByDue((allTasks ?? []).filter((t) => t.status !== 'done')),
    [allTasks],
  )
  const { text: greeting, hourKey } = useGreeting()
  const { data: quote } = useQuote(hourKey)
  const providerOptions = useMemo(() => ui?.CAPABILITIES.providers ?? [], [ui?.CAPABILITIES.providers])
  const effectiveAskProvider = useMemo(() => {
    const available = providerOptions.filter((p) => p.available !== false)
    if (!available.length) return askProvider
    return available.some((p) => p.id === askProvider) ? askProvider : available[0].id
  }, [askProvider, providerOptions])

  const submitAskFlow = () => {
    const prompt = askFlow.trim()
    if (!prompt || action.isPending) return
    action.mutate(
      { kind: 'overview-chat', prompt, provider: effectiveAskProvider },
      {
        onSuccess: (resp) => {
          setAskFlow('')
          if (resp.floating_terminal) openFloatingTerminal(resp.floating_terminal)
        },
      },
    )
  }

  // High-priority backlog, sourced from /api/overview so the rows carry
  // due_info / assignee / stale_days (the UiData BACKLOG bucket drops them).
  // The endpoint returns them unsorted, so order by soonest-due here: overdue
  // first (earlier YYYY-MM-DD sorts first), undated last, newest as tie-break.
  const backlog = useMemo(() => {
    const rows = (overview?.high_priority_backlog ?? []).slice()
    rows.sort((a, b) => {
      if (a.due_date && b.due_date) return a.due_date < b.due_date ? -1 : a.due_date > b.due_date ? 1 : 0
      if (a.due_date) return -1
      if (b.due_date) return 1
      return Date.parse(b.updated_at) - Date.parse(a.updated_at)
    })
    return rows
  }, [overview])

  // One row per thread (task), newest message first — Slack AND GitHub.
  const inboxThreads = useMemo(() => {
    const map = new Map<string, { entry: InboxFeedEntry; unread: number }>()
    for (const e of inbox?.entries ?? []) {
      const cur = map.get(e.task_slug)
      if (!cur) map.set(e.task_slug, { entry: e, unread: e.unread ? 1 : 0 })
      else {
        if (e.unread) cur.unread += 1
        if (Date.parse(e.timestamp) > Date.parse(cur.entry.timestamp)) cur.entry = e
      }
    }
    return [...map.values()]
      .sort((a, b) => Date.parse(b.entry.timestamp) - Date.parse(a.entry.timestamp))
      .slice(0, 6)
  }, [inbox])

  if (isLoading) return <div className="page"><Loading label="loading mission control" /></div>
  if (error) return <div className="page"><ErrorNote error={error} /></div>
  if (!ui) return null

  const running = ui.AGENTS.filter((a) => a.status === 'running')
  const waiting = ui.AGENTS.filter((a) => a.status === 'waiting')
  // `dead` = a session that exited abnormally (stop_failure) — the old UI's
  // "CRASHED"; surface it prominently so it can be restarted.
  const crashed = ui.AGENTS.filter((a) => a.status === 'dead')
  // `stale` = an in-progress task whose session has gone quiet past the stale
  // threshold (FLOW_STALE_DAYS, default 3d). These don't show in any other
  // section, so a "going cold" shelf keeps them from being forgotten.
  const stale = ui.AGENTS.filter((a) => a.status === 'stale')
  const live = ui.AGENTS.filter((a) => a.status === 'idle' || a.status === 'released' || a.status === 'running')
  const sessions = [...running, ...live.filter((a) => a.status !== 'running')]
  const runsThisWeek = ui.PLAYBOOKS_MC.reduce((n, p) => n + p.runs_week, 0)
  const activePlaybooks = ui.PLAYBOOKS_MC.filter((p) => p.runs_week > 0)
  const env = [...ui.CAPABILITIES.providers, ...ui.CAPABILITIES.integrations]

  const stats = [
    { label: 'Running', value: running.length, tone: running.length ? 'var(--ok)' : '' },
    { label: 'Awaiting', value: waiting.length, tone: waiting.length ? 'var(--warn)' : '' },
    { label: 'Crashed', value: crashed.length, tone: crashed.length ? 'var(--danger)' : '' },
    { label: 'In flight', value: ui.AGENTS.length, tone: '' },
    { label: 'Backlog', value: ui.BACKLOG.length, tone: '' },
    { label: 'Runs 7d', value: runsThisWeek, tone: '' },
  ]

  return (
    <div className="page">
      <div className="page-head mc-head">
        <div>
          <h1 className="h-xl">{greeting}, {ui.USER?.name || 'there'}</h1>
          {quote?.quote ? (
            <div className="mc-quote">
              <span className="mc-quote-text">“{quote.quote}”</span>
              {(quote.character || quote.anime || quote.author) && (
                <span className="mc-quote-attr">
                  — {[quote.character, quote.anime, quote.author].filter(Boolean).join(' · ')}
                </span>
              )}
            </div>
          ) : (
            <div className="page-sub">Here's everything on your plate.</div>
          )}
        </div>
        <div className="spacer" />
        <div className="mc-env-pills">
          {env.map((c) => (
            <span key={c.id} className={`env-pill${c.available ? '' : ' off'}`} title={c.reason || c.status || ''}>
              <span className={`dot ${c.available ? 'running' : 'idle'}`} />
              {c.id === 'claude' || c.id === 'codex' ? (
                <ProviderIcon provider={c.id} size={13} />
              ) : (
                <SourceIcon source={c.id === 'gh' ? 'github' : c.id} size={12} />
              )}
              {c.label}
            </span>
          ))}
        </div>
      </div>

      <section className="ask-flow">
        <div className="ask-flow-mark">
          <Sparkles size={18} />
        </div>
        <div className="ask-flow-main">
          <div className="eyebrow">Ask Flow</div>
          <input
            className="ask-flow-input"
            aria-label="Ask Flow"
            value={askFlow}
            disabled={action.isPending}
            placeholder="Triage my day, inspect stalled sessions, or route work into tasks…"
            onChange={(e) => setAskFlow(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                submitAskFlow()
              }
            }}
          />
        </div>
        <div className="ask-flow-actions">
          <AgentPicker value={effectiveAskProvider} onChange={setAskProvider} providers={providerOptions} />
        </div>
        <button
          type="button"
          className="btn primary"
          disabled={!askFlow.trim() || action.isPending}
          onClick={submitAskFlow}
        >
          {action.isPending ? <Loader2 size={15} className="spin" /> : <SendHorizontal size={15} />}
          Open
        </button>
      </section>

      <div className="card pulse" style={{ marginBottom: 18 }}>
        {stats.map((s) => (
          <div className="pulse-cell" key={s.label}>
            <div className="pulse-val num" style={s.tone ? { color: s.tone } : undefined}>{s.value}</div>
            <div className="pulse-label eyebrow">{s.label}</div>
          </div>
        ))}
      </div>

      <div className="mc-cols">
        {/* ---- main column ---- */}
        <div className="mc-main">
          {waiting.length > 0 && (
            <section>
              <div className="section-head">
                <span className="eyebrow"><AlertTriangle size={13} /> Needs your attention</span>
                <span className="section-count">{waiting.length}</span>
              </div>
              <div className="grid cards stagger">
                {waiting.map((a) => <AgentCard key={a.slug} agent={a} />)}
              </div>
            </section>
          )}

          {crashed.length > 0 && (
            <section>
              <div className="section-head">
                <span className="eyebrow danger-text"><AlertTriangle size={13} /> Crashed sessions</span>
                <span className="section-count">{crashed.length}</span>
                <div className="spacer" />
                <span className="faint" style={{ fontSize: 12 }}>open to restart</span>
              </div>
              <div className="grid cards stagger">
                {crashed.map((a) => <AgentCard key={a.slug} agent={a} />)}
              </div>
            </section>
          )}

          {stale.length > 0 && (
            <section>
              <div className="section-head">
                <span className="eyebrow"><Snowflake size={13} /> Going cold</span>
                <span className="section-count">{stale.length}</span>
                <div className="spacer" />
                <span className="faint" style={{ fontSize: 12 }}>no activity in a while</span>
              </div>
              <div className="grid cards stagger">
                {stale.map((a) => <AgentCard key={a.slug} agent={a} />)}
              </div>
            </section>
          )}

          <section>
            <div className="section-head">
              <span className="eyebrow"><Activity size={13} /> Live sessions</span>
              <span className="section-count">{ui.AGENTS.length}</span>
              <div className="spacer" />
              <Link href="/sessions" className="btn ghost sm">All sessions <ArrowRight size={14} /></Link>
            </div>
            {sessions.length === 0 ? (
              <EmptyState icon={<TerminalSquare size={26} />} title="No active sessions" hint="Start a task to spin up a Claude or Codex session." />
            ) : (
              <div className="grid cards stagger">
                {sessions.slice(0, 6).map((a) => <AgentCard key={a.slug} agent={a} />)}
              </div>
            )}
          </section>

          {agendaCount(due) > 0 && (
            <section>
              <div className="section-head">
                <span className="eyebrow"><CalendarClock size={13} /> Agenda</span>
                <span className="section-count">{agendaCount(due)}</span>
                <div className="spacer" />
                <Link href="/tasks" className="btn ghost sm">Tasks <ArrowRight size={14} /></Link>
              </div>
              <AgendaBuckets due={due} onOpen={(slug) => navigate(`/session/${slug}`)} />
            </section>
          )}

          <section>
            <div className="section-head">
              <span className="eyebrow">High-priority backlog</span>
              <span className="section-count">{backlog.length}</span>
              <div className="spacer" />
              <Link href="/tasks" className="btn ghost sm">Tasks <ArrowRight size={14} /></Link>
            </div>
            <div className="rows">
              {backlog.slice(0, 8).map((t) => {
                const tone = dueTone(t.due_date, t.due_info)
                return (
                  <div key={t.slug} className="lrow" aria-label={`Open ${t.name}`} {...clickable(() => navigate(`/session/${t.slug}`))}>
                    <span className={`prio ${t.priority}`} />
                    <ProviderIcon provider={t.session_provider} size={14} />
                    <div className="lrow-main">
                      <div className="lrow-title clip">{t.name}</div>
                      <div className="lrow-sub clip">
                        {t.project_slug || 'no project'}
                        {t.assignee ? ` · @${t.assignee}` : ''}
                        {t.stale_days != null && t.stale_days > 0 ? ` · stale ${t.stale_days}d` : ''}
                      </div>
                    </div>
                    {t.due_info && (
                      <span
                        className={`badge ${tone}`}
                        style={{ whiteSpace: 'nowrap', height: 'auto', padding: '3px 8px' }}
                        title={t.due_date ? `Due ${t.due_date}` : undefined}
                      >
                        {t.due_info}
                      </span>
                    )}
                    {t.tags?.slice(0, 2).map((tag) => <span key={tag} className="tag">{tag}</span>)}
                  </div>
                )
              })}
              {backlog.length === 0 && <div className="lrow"><span className="faint">No high-priority backlog.</span></div>}
            </div>
          </section>
        </div>

        {/* ---- right rail ---- */}
        <div className="mc-rail">
          {/* GitHub-style activity, placed high */}
          <section className="card rail-card">
            <div className="bento-head">
              <span className="eyebrow"><Activity size={13} /> Activity</span>
              <div className="spacer" />
              <span className="faint mono" style={{ fontSize: 10 }}>tasks · 28d</span>
            </div>
            <ActivityBars days={ui.ACTIVITY_HEATMAP} />
            <div className="hairline" style={{ margin: '14px 0' }} />
            <div className="eyebrow" style={{ marginBottom: 8 }}>tokens · 12 weeks</div>
            <div style={{ display: 'flex', justifyContent: 'center' }}>
              <MiniCalendar days={ui.TOKEN_SERIES ?? []} />
            </div>
            <div className="hairline" style={{ margin: '14px 0' }} />
            <StatsPanel stats={ui.STATS} />
          </section>

          <TrendsCard doneTasks={doneTasks} tokenSeries={ui.TOKEN_SERIES ?? []} />

          <ProjectProgressCard projects={ui.PROJECTS_MC} onOpen={(slug) => navigate(`/project/${slug}`)} />

          <section className="card rail-card">
            <div className="bento-head">
              <span className="eyebrow"><InboxIcon size={13} /> Recent inbox</span>
              <div className="spacer" />
              <Link href="/inbox" className="btn ghost sm">Open <ArrowRight size={13} /></Link>
            </div>
            <div className="rail-body">
              {inboxThreads.length === 0 && <div className="faint" style={{ padding: 8 }}>No recent messages.</div>}
              {inboxThreads.map(({ entry, unread }) => (
                <div key={entry.task_slug} className="feed-row" aria-label={`Open inbox — ${entry.task_name}`} {...clickable(() => navigate('/inbox'))}>
                  {unread > 0 ? <span className="unread-dot" /> : <span className="dot idle" />}
                  <SourceIcon source={entry.source} size={13} />
                  <div className="lrow-main">
                    <div className="feed-title clip">{entry.task_name}</div>
                    <div className="feed-sub clip">
                      {entry.body_snippet || entry.body || (entry.source === 'github' ? 'GitHub activity' : 'New activity')}
                    </div>
                  </div>
                  <div className="col" style={{ alignItems: 'flex-end', gap: 3 }}>
                    <span className="faint mono" style={{ fontSize: 10 }}>{ago(entry.timestamp)}</span>
                    {unread > 1 && <span className="tag">{unread} new</span>}
                  </div>
                </div>
              ))}
            </div>
          </section>

          <section className="card rail-card">
            <div className="bento-head">
              <span className="eyebrow"><Repeat size={13} /> Active playbooks</span>
              <div className="spacer" />
              <Link href="/playbooks" className="btn ghost sm">All <ArrowRight size={13} /></Link>
            </div>
            <div className="rail-body">
              {activePlaybooks.slice(0, 5).map((p) => (
                <div key={p.slug} className="feed-row" aria-label={`Open playbook ${p.name}`} {...clickable(() => navigate(`/playbook/${p.slug}`))}>
                  <div className="lrow-main">
                    <div className="feed-title clip">{p.name}</div>
                    <div className="feed-sub clip">{p.runs_week} runs · 7d</div>
                  </div>
                  <PlaybookSpark runs={p.runs} fallback={p.spark} />
                </div>
              ))}
              {activePlaybooks.length === 0 && <div className="faint" style={{ padding: 8 }}>No runs this week.</div>}
            </div>
          </section>
        </div>
      </div>
    </div>
  )
}
