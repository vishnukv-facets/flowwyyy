import { useMemo, useState, type ReactNode } from 'react'
import { useLocation, useSearch } from 'wouter'
import { useAnalytics, type AnalyticsParams } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { EmptyState, ErrorNote } from '../components/ui'
import { BarList, ConversionFunnel, LineChart, PieChart, StackedAreaChart, formatValue } from '../components/charts'
import { compact, dateTime } from '../lib/format'
import type { AnalyticsFunnel, AnalyticsKpi, AnalyticsPayload, AnalyticsSegment, AnalyticsSeries } from '../lib/types'

// Range pills, in display order. 'custom' opens the from/to popover instead of
// setting ?range=.
const RANGES = ['1d', '7d', '15d', '30d', '6m'] as const
type RangeToken = (typeof RANGES)[number]

// KPIs where a rising number is unambiguously good, so the delta can be colour-
// coded. Tokens/cost are left neutral — more usage is neither good nor bad.
const MORE_IS_BETTER = new Set(['tasks_done', 'runs', 'events_observed'])

export function Analytics() {
  useDocumentTitle('Analytics')
  const search = useSearch()
  const [, navigate] = useLocation()
  const params = useMemo(() => new URLSearchParams(search), [search])

  const fromParam = params.get('from') ?? ''
  const toParam = params.get('to') ?? ''
  const isCustom = !!(fromParam && toParam)
  const rangeParam = params.get('range') ?? ''
  const range: RangeToken = (RANGES as readonly string[]).includes(rangeParam) ? (rangeParam as RangeToken) : '7d'

  const query: AnalyticsParams = isCustom ? { from: fromParam, to: toParam } : { range }
  const { data, error, isLoading, isFetching } = useAnalytics(query)

  const pickRange = (r: RangeToken) => navigate(r === '7d' ? '/analytics' : `/analytics?range=${r}`)

  return (
    <div className="page analytics">
      <div className="page-head">
        <div>
          <div className="eyebrow">analytics</div>
          <h1 className="h-xl">Analytics</h1>
        </div>
        <div className="spacer" />
        <LiveStamp data={data} fetching={isFetching} />
      </div>

      <div className="row gap wrap analytics-controls">
        <div className="range-pills" role="tablist" aria-label="Time range">
          {RANGES.map((r) => (
            <button
              key={r}
              type="button"
              role="tab"
              aria-selected={!isCustom && range === r}
              className={`pill ${!isCustom && range === r ? 'active' : ''}`}
              onClick={() => pickRange(r)}
            >
              {r}
            </button>
          ))}
          <CustomRange active={isCustom} from={fromParam} to={toParam} onApply={(f, t) => navigate(`/analytics?from=${encodeURIComponent(f)}&to=${encodeURIComponent(t)}`)} />
        </div>
      </div>

      {error ? (
        <ErrorNote error={error} />
      ) : isLoading || !data ? (
        <AnalyticsSkeleton />
      ) : (
        <AnalyticsBody data={data} />
      )}
    </div>
  )
}

function LiveStamp({ data, fetching }: { data?: AnalyticsPayload; fetching: boolean }) {
  if (!data) return null
  return (
    <div className="live-stamp" title={`window ${dateTime(data.from)} → ${dateTime(data.to)} · ${data.tz}`}>
      <span className={`live-dot ${fetching ? 'pulsing' : ''}`} />
      <span className="faint">{data.partial_bucket ? 'live' : 'updated'}</span>
      <span className="mono faint">{dateTime(data.generated_at)}</span>
    </div>
  )
}

function AnalyticsBody({ data }: { data: AnalyticsPayload }) {
  const bucket = data.bucket
  const seriesBy = (key: string) => data.series.find((s) => s.key === key)
  const breakdownBy = (key: string) => data.breakdowns?.find((b) => b.key === key)

  const hasUsage = seriesBy('tokens') || seriesBy('cost') || breakdownBy('model_mix') || breakdownBy('project_effort')
  const hasConnectors =
    (data.conversions?.length ?? 0) > 0 || !!data.funnel || !!seriesBy('steering') || !!breakdownBy('task_source')

  const allZero =
    data.kpis.every((k) => k.value === 0) &&
    !data.funnel &&
    (data.breakdowns?.length ?? 0) === 0 &&
    (data.conversions?.length ?? 0) === 0

  if (allZero) {
    return (
      <>
        <KpiRow kpis={data.kpis} />
        <EmptyState title="No activity in this window" hint="Pick a wider range, or come back once there's work to chart." />
      </>
    )
  }

  return (
    <>
      <KpiRow kpis={data.kpis} />

      <Section title="Activity">
        {seriesBy('throughput') && <SeriesCard series={seriesBy('throughput')!} bucket={bucket} />}
      </Section>

      {hasUsage && (
        <Section title="Usage & cost">
          {seriesBy('tokens') && <SeriesCard series={seriesBy('tokens')!} bucket={bucket} />}
          {seriesBy('cost') && <SeriesCard series={seriesBy('cost')!} bucket={bucket} />}
          {breakdownBy('model_mix') && (
            <Card title="Model mix">
              <BarList segments={breakdownBy('model_mix')!.segments} unit="tokens" />
            </Card>
          )}
          {breakdownBy('project_effort') && (
            <Card title="Project effort (tokens)">
              <BarList segments={breakdownBy('project_effort')!.segments} unit="tokens" />
            </Card>
          )}
        </Section>
      )}

      {hasConnectors && (
        <Section title="Connectors & attention">
          {(data.conversions?.length ?? 0) > 0 && (
            <Card title="Connector → task conversion">
              <ConversionFunnel conversions={data.conversions!} />
            </Card>
          )}
          {breakdownBy('task_source') && (
            <Card title="Tasks by origin">
              <PieChart
                segments={breakdownBy('task_source')!.segments}
                centerLabel={<PieCenter value={breakdownBy('task_source')!.segments.reduce((a, s) => a + s.value, 0)} label="tasks" />}
              />
            </Card>
          )}
          {data.funnel && (
            <Card title="Attention funnel">
              <PieChart
                segments={funnelSegments(data.funnel)}
                colorOf={funnelTone}
                centerLabel={<PieCenter value={data.funnel.observed} label="observed" />}
              />
            </Card>
          )}
          {seriesBy('steering') && <SeriesCard series={seriesBy('steering')!} bucket={bucket} />}
          {seriesBy('steering_latency') && <SeriesCard series={seriesBy('steering_latency')!} bucket={bucket} />}
        </Section>
      )}

      {seriesBy('runs') && (
        <Section title="Autonomy">
          <SeriesCard series={seriesBy('runs')!} bucket={bucket} />
        </Section>
      )}
    </>
  )
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="analytics-section">
      <h2 className="section-title eyebrow">{title}</h2>
      <div className="chart-grid">{children}</div>
    </section>
  )
}

function Card({ title, wide, children }: { title: string; wide?: boolean; children: ReactNode }) {
  return (
    <section className={`card chart-card ${wide ? 'span-2' : ''}`}>
      <header className="card-head">
        <span className="eyebrow">{title}</span>
      </header>
      {children}
    </section>
  )
}

function SeriesCard({ series, bucket }: { series: AnalyticsSeries; bucket: string }) {
  return (
    <Card title={series.label}>
      {series.stacked ? <StackedAreaChart series={series} bucket={bucket} /> : <LineChart series={series} bucket={bucket} />}
    </Card>
  )
}

function KpiRow({ kpis }: { kpis: AnalyticsKpi[] }) {
  if (kpis.length === 0) return null
  return (
    <div className="kpi-row">
      {kpis.map((k) => (
        <KpiCard key={k.key} kpi={k} />
      ))}
    </div>
  )
}

function KpiCard({ kpi }: { kpi: AnalyticsKpi }) {
  return (
    <div className="kpi">
      <div className="kpi-label eyebrow">{kpi.label}</div>
      <div className="kpi-value num">{formatValue(kpi.value, kpi.unit)}</div>
      <Delta kpi={kpi} />
    </div>
  )
}

function Delta({ kpi }: { kpi: AnalyticsKpi }) {
  if (kpi.delta_pct == null) return <div className="kpi-delta faint">vs prior · —</div>
  const up = kpi.delta_pct >= 0
  const neutral = !MORE_IS_BETTER.has(kpi.key)
  const tone = neutral ? '' : up ? 'good' : 'bad'
  return (
    <div className={`kpi-delta ${tone}`}>
      <span className="delta-arrow">{up ? '▲' : '▼'}</span>
      {Math.abs(Math.round(kpi.delta_pct))}% <span className="faint">vs prior</span>
    </div>
  )
}

// Donut-hole label: a big total with a caption beneath.
function PieCenter({ value, label }: { value: number; label: string }) {
  return (
    <>
      <b className="num">{compact(value)}</b>
      <span>{label}</span>
    </>
  )
}

// Flatten the attention funnel into pie segments: where did observed events go
// (surfaced, errored, or dropped at each stage)?
function funnelSegments(f: AnalyticsFunnel): AnalyticsSegment[] {
  const segs: AnalyticsSegment[] = [{ key: 'surfaced', label: 'Surfaced', value: f.surfaced }]
  for (const d of f.dropped ?? []) segs.push({ key: `drop:${d.key}`, label: `Dropped · ${d.label ?? d.key}`, value: d.value })
  if (f.errors > 0) segs.push({ key: 'errors', label: 'Errors', value: f.errors })
  return segs
}

// Semantic colours for the funnel pie: surfaced is the win (accent), errors are
// danger, dropped stages cycle through a muted ramp.
const DROP_RAMP = ['var(--text-3)', 'var(--border-strong)', 'var(--accent-lo)', 'var(--info)', 'var(--warn)']
function funnelTone(s: AnalyticsSegment, i: number): string {
  if (s.key === 'surfaced') return 'var(--accent)'
  if (s.key === 'errors') return 'var(--danger)'
  return DROP_RAMP[i % DROP_RAMP.length]
}

function CustomRange({ active, from, to, onApply }: { active: boolean; from: string; to: string; onApply: (from: string, to: string) => void }) {
  const [open, setOpen] = useState(false)
  const [f, setF] = useState(() => from.slice(0, 10))
  const [t, setT] = useState(() => to.slice(0, 10))

  const apply = () => {
    if (!f || !t) return
    const [fy, fm, fd] = f.split('-').map(Number)
    const [ty, tm, td] = t.split('-').map(Number)
    // Local-day boundaries → RFC3339 (toISOString emits the Z form the Go
    // RFC3339 parser requires); the server clamps `to` to now.
    const fromIso = new Date(fy, fm - 1, fd, 0, 0, 0).toISOString()
    const toIso = new Date(ty, tm - 1, td, 23, 59, 59).toISOString()
    setOpen(false)
    onApply(fromIso, toIso)
  }

  return (
    <div className="custom-range">
      <button type="button" className={`pill ${active ? 'active' : ''}`} aria-expanded={open} onClick={() => setOpen((v) => !v)}>
        {active ? `${from.slice(0, 10)} → ${to.slice(0, 10)}` : 'custom'}
      </button>
      {open && (
        <div className="range-pop card">
          <label className="field">
            <span className="field-label">From</span>
            <input type="date" value={f} max={t || undefined} onChange={(e) => setF(e.target.value)} />
          </label>
          <label className="field">
            <span className="field-label">To</span>
            <input type="date" value={t} min={f || undefined} onChange={(e) => setT(e.target.value)} />
          </label>
          <button type="button" className="btn sm primary" disabled={!f || !t} onClick={apply}>
            Apply
          </button>
        </div>
      )}
    </div>
  )
}

function AnalyticsSkeleton() {
  return (
    <>
      <div className="kpi-row">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="kpi skeleton" style={{ height: 84 }} />
        ))}
      </div>
      <div className="chart-grid">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className={`card chart-card skeleton ${i === 0 ? 'span-2' : ''}`} style={{ height: 240 }} />
        ))}
      </div>
    </>
  )
}
