// Hand-built SVG charts for the analytics page. No chart library — the geometry
// lives in lib/chartGeom.ts (unit-tested), and these components turn an
// AnalyticsSeries into crisp, theme-aware SVG. Charts measure their container
// (ResizeObserver) and draw in real pixels, so strokes and dots never distort
// the way a scaled viewBox would.
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { cumulativeStacks, labelIndices, nearestIndex, niceCeil, pathFrom, pieSlices, project, ticks, type Pt } from '../lib/chartGeom'
import { compact, compactTokens, fmtUSD, fromMinutes, shortDate } from '../lib/format'
import { useFloatTip } from './FloatTip'
import { SourceIcon } from './ui'
import type { AnalyticsLine, AnalyticsSegment, AnalyticsSeries, AnalyticsSourceConversion } from '../lib/types'

const PAD = { t: 10, r: 10, b: 22, l: 42 }

// ---- measure (responsive without viewBox scaling) -----------------------
function useMeasure(): [(el: HTMLDivElement | null) => void, number] {
  const [w, setW] = useState(0)
  const obs = useRef<ResizeObserver | null>(null)
  const ref = useCallback((el: HTMLDivElement | null) => {
    obs.current?.disconnect()
    if (!el) return
    obs.current = new ResizeObserver((entries) => {
      const cr = entries[0]?.contentRect
      if (cr) setW(cr.width)
    })
    obs.current.observe(el)
    setW(el.clientWidth)
  }, [])
  useEffect(() => () => obs.current?.disconnect(), [])
  return [ref, w]
}

// ---- tones --------------------------------------------------------------
// Stable colours by well-known line key, falling back to a small palette so an
// unrecognised provider/model still gets a distinct, repeatable colour.
const KEY_TONE: Record<string, string> = {
  created: 'var(--text-3)',
  done: 'var(--accent)',
  started: 'var(--text-3)',
  completed: 'var(--accent)',
  observed: 'var(--text-3)',
  surfaced: 'var(--accent)',
  p50: 'var(--warn)',
  claude: 'var(--accent)',
  codex: 'var(--accent-2)',
  resume: 'var(--accent)',
  reference: 'var(--info)',
  cross_task: 'var(--warn)',
  kb: 'var(--accent-2)',
  unknown: 'var(--text-3)',
}
const PALETTE = ['var(--accent)', 'var(--accent-2)', 'var(--info)', 'var(--warn)', 'var(--ok)', 'var(--danger)']
function lineTone(key: string, i: number): string {
  return KEY_TONE[key] ?? PALETTE[i % PALETTE.length]
}

// ---- value / axis formatting --------------------------------------------
export function formatValue(v: number, unit: string): string {
  switch (unit) {
    case 'tokens':
      return compactTokens(v)
    case 'usd':
      return fmtUSD(v)
    case 'min':
      return v >= 1 ? fromMinutes(v) : `${Math.round(v * 60)}s`
    case 'pct':
      return `${Math.round(v)}%`
    case 'ms':
      return v >= 1000 ? `${(v / 1000).toFixed(1)}s` : `${Math.round(v)}ms`
    case 'days':
      return `${v.toFixed(v < 10 ? 1 : 0)}d`
    case 'hours':
      return `${v.toFixed(v < 10 ? 1 : 0)}h`
    default:
      return compact(v)
  }
}

// Sparse, readable x labels: a date for day/week buckets, an hour for hourly.
function bucketLabel(t: string, bucket: string): string {
  if (bucket === 'hour') {
    const d = new Date(t)
    return `${String(d.getHours()).padStart(2, '0')}:00`
  }
  return shortDate(t)
}

// ---- shared axis frame ---------------------------------------------------
function axisTicks(max: number) {
  return ticks(max, 4)
}

interface Frame {
  w: number
  h: number
  plotW: number
  plotH: number
  top: number // niceCeil'd y max
}
function frame(width: number, height: number, max: number): Frame {
  return {
    w: width,
    h: height,
    plotW: Math.max(0, width - PAD.l - PAD.r),
    plotH: Math.max(0, height - PAD.t - PAD.b),
    top: niceCeil(max),
  }
}

function YAxis({ f, unit }: { f: Frame; unit: string }) {
  return (
    <g className="ax-y">
      {axisTicks(f.top).map((tv, i) => {
        const y = PAD.t + f.plotH - (tv / f.top) * f.plotH
        return (
          <g key={i}>
            <line x1={PAD.l} y1={y} x2={f.w - PAD.r} y2={y} className="grid-line" />
            <text x={PAD.l - 6} y={y + 3} className="ax-label" textAnchor="end">
              {formatValue(tv, unit)}
            </text>
          </g>
        )
      })}
    </g>
  )
}

function XLabels({ f, points, bucket }: { f: Frame; points: { t: string }[]; bucket: string }) {
  const n = points.length
  if (n === 0) return null
  const idxs = labelIndices(n)
  return (
    <g className="ax-x">
      {idxs.map((i) => {
        const x = PAD.l + (n <= 1 ? 0 : (i / (n - 1)) * f.plotW)
        const anchor = i === 0 ? 'start' : i === n - 1 ? 'end' : 'middle'
        return (
          <text key={i} x={x} y={f.h - 6} className="ax-label" textAnchor={anchor}>
            {bucketLabel(points[i].t, bucket)}
          </text>
        )
      })}
    </g>
  )
}

// A vertical hover guide that finds the nearest bucket and shows a FloatTip with
// every line's value at that bucket. Shared by line + stacked-area charts.
function useCrosshair(f: Frame, n: number) {
  const { showAt, hide, portal } = useFloatTip()
  const [hover, setHover] = useState<number | null>(null)
  const onMove = useCallback(
    (e: React.MouseEvent<SVGRectElement>, tip: (i: number) => ReactNode) => {
      if (n === 0) return
      // The overlay rect starts at SVG x=PAD.l, so its bounding-box left IS the
      // plot's left edge — cursor offset needs no further padding adjustment.
      const r = e.currentTarget.getBoundingClientRect()
      const i = nearestIndex(e.clientX - r.left, f.plotW, n)
      setHover(i)
      // Anchor the tip at the hovered data point's x (snapped to the bucket) and
      // the cursor's y — not the rect centre, which pinned every tip mid-chart.
      const px = r.left + (n <= 1 ? 0 : (i / (n - 1)) * f.plotW)
      showAt(px, e.clientY, tip(i))
    },
    [f.plotW, n, showAt],
  )
  const onLeave = useCallback(() => {
    setHover(null)
    hide()
  }, [hide])
  const xAt = (i: number) => PAD.l + (n <= 1 ? 0 : (i / (n - 1)) * f.plotW)
  return { hover, onMove, onLeave, portal, xAt }
}

function ChartTip({ label, rows }: { label: string; rows: { key: string; tone: string; text: string }[] }) {
  return (
    <div className="chart-tip">
      <div className="chart-tip-head">{label}</div>
      {rows.map((r) => (
        <div key={r.key} className="chart-tip-row">
          <i className="chart-tip-dot" style={{ background: r.tone }} />
          <span className="chart-tip-key">{r.key}</span>
          <span className="chart-tip-val mono">{r.text}</span>
        </div>
      ))}
    </div>
  )
}

// ---- line chart (1+ non-stacked lines) ----------------------------------
export function LineChart({ series, bucket, height = 190 }: { series: AnalyticsSeries; bucket: string; height?: number }) {
  const [ref, w] = useMeasure()
  const lines = series.lines
  const n = lines[0]?.points.length ?? 0
  const max = Math.max(1, ...lines.flatMap((l) => l.points.map((p) => p.v)))
  const f = frame(w, height, max)
  const ch = useCrosshair(f, n)

  const tip = (i: number): ReactNode => (
    <ChartTip
      label={lines[0]?.points[i] ? bucketLabel(lines[0].points[i].t, bucket) : ''}
      rows={lines.map((l, li) => ({
        key: l.label ?? l.key,
        tone: lineTone(l.key, li),
        text: formatValue(l.points[i]?.v ?? 0, series.unit),
      }))}
    />
  )

  return (
    <div className="chart" ref={ref}>
      {ch.portal}
      {w > 0 && (
        <svg width={w} height={height} role="img" aria-label={series.label}>
          <YAxis f={f} unit={series.unit} />
          <XLabels f={f} points={lines[0]?.points ?? []} bucket={bucket} />
          {ch.hover != null && (
            <line x1={ch.xAt(ch.hover)} y1={PAD.t} x2={ch.xAt(ch.hover)} y2={PAD.t + f.plotH} className="hover-guide" />
          )}
          {lines.map((l, li) => {
            const pts = project(l.points.map((p) => p.v), { w: f.plotW, h: f.plotH, max }).map((p) => offset(p))
            const tone = lineTone(l.key, li)
            return (
              <g key={l.key}>
                <path d={pathFrom(pts)} fill="none" stroke={tone} strokeWidth={2} className="line-path" />
                {ch.hover != null && pts[ch.hover] && (
                  <circle cx={pts[ch.hover].x} cy={pts[ch.hover].y} r={3.5} fill={tone} className="hover-dot" />
                )}
              </g>
            )
          })}
          <rect
            x={PAD.l}
            y={PAD.t}
            width={f.plotW}
            height={f.plotH}
            fill="transparent"
            onMouseMove={(e) => ch.onMove(e, tip)}
            onMouseLeave={ch.onLeave}
          />
        </svg>
      )}
      <Legend lines={lines} />
    </div>
  )

  function offset(p: Pt): Pt {
    return { x: p.x + PAD.l, y: p.y + PAD.t }
  }
}

// ---- stacked area chart (provider-split tokens/cost) --------------------
export function StackedAreaChart({ series, bucket, height = 190 }: { series: AnalyticsSeries; bucket: string; height?: number }) {
  const [ref, w] = useMeasure()
  const lines = series.lines
  const n = lines[0]?.points.length ?? 0
  const values = lines.map((l) => l.points.map((p) => p.v))
  const tops = cumulativeStacks(values)
  const max = Math.max(1, ...(tops[tops.length - 1] ?? [0]))
  const f = frame(w, height, max)
  const ch = useCrosshair(f, n)

  const tip = (i: number): ReactNode => (
    <ChartTip
      label={lines[0]?.points[i] ? bucketLabel(lines[0].points[i].t, bucket) : ''}
      rows={lines.map((l, li) => ({
        key: l.label ?? l.key,
        tone: lineTone(l.key, li),
        text: formatValue(l.points[i]?.v ?? 0, series.unit),
      }))}
    />
  )

  return (
    <div className="chart" ref={ref}>
      {ch.portal}
      {w > 0 && (
        <svg width={w} height={height} role="img" aria-label={series.label}>
          <YAxis f={f} unit={series.unit} />
          <XLabels f={f} points={lines[0]?.points ?? []} bucket={bucket} />
          {ch.hover != null && (
            <line x1={ch.xAt(ch.hover)} y1={PAD.t} x2={ch.xAt(ch.hover)} y2={PAD.t + f.plotH} className="hover-guide" />
          )}
          {lines.map((l, li) => {
            const upper = project(tops[li], { w: f.plotW, h: f.plotH, max }).map(off)
            const lowerVals = li === 0 ? values[0].map(() => 0) : tops[li - 1]
            const lower = project(lowerVals, { w: f.plotW, h: f.plotH, max }).map(off)
            const tone = lineTone(l.key, li)
            return <path key={l.key} d={bandPath(upper, lower)} fill={tone} fillOpacity={0.5} stroke={tone} strokeWidth={1.25} />
          })}
          <rect
            x={PAD.l}
            y={PAD.t}
            width={f.plotW}
            height={f.plotH}
            fill="transparent"
            onMouseMove={(e) => ch.onMove(e, tip)}
            onMouseLeave={ch.onLeave}
          />
        </svg>
      )}
      <Legend lines={lines} />
    </div>
  )

  function off(p: Pt): Pt {
    return { x: p.x + PAD.l, y: p.y + PAD.t }
  }
}

// Polygon between an upper and lower boundary (lower traced in reverse).
function bandPath(upper: Pt[], lower: Pt[]): string {
  if (upper.length === 0) return ''
  const up = pathFrom(upper)
  const down = [...lower].reverse().map((p) => `L${r2(p.x)} ${r2(p.y)}`).join('')
  return `${up}${down}Z`
}
function r2(n: number): number {
  return Math.round(n * 100) / 100
}

function Legend({ lines }: { lines: AnalyticsLine[] }) {
  if (lines.length <= 1 && (lines[0]?.label ?? lines[0]?.key) === undefined) return null
  return (
    <div className="chart-legend">
      {lines.map((l, i) => (
        <span key={l.key} className="chart-legend-item">
          <i className="chart-legend-dot" style={{ background: lineTone(l.key, i) }} />
          {l.label ?? l.key}
        </span>
      ))}
    </div>
  )
}

// ---- pie / donut chart ---------------------------------------------------
// A composition donut + legend, for small comparable sets (tasks by origin,
// attention disposition). colorOf overrides the default palette per segment so
// callers can pin semantic tones (e.g. surfaced→accent, errors→danger). Zero
// segments are dropped. centerLabel renders in the donut hole.
export function PieChart({
  segments,
  unit = 'count',
  colorOf,
  centerLabel,
}: {
  segments: AnalyticsSegment[]
  unit?: string
  colorOf?: (s: AnalyticsSegment, i: number) => string
  centerLabel?: ReactNode
}) {
  const { show, hide, portal } = useFloatTip()
  const shown = segments.filter((s) => s.value > 0)
  const total = shown.reduce((a, s) => a + s.value, 0)
  if (shown.length === 0 || total === 0) return <div className="faint" style={{ padding: '8px 0' }}>no data</div>
  const tone = (s: AnalyticsSegment, i: number) => (colorOf ? colorOf(s, i) : PALETTE[i % PALETTE.length])
  const SIZE = 132
  const slices = pieSlices(shown.map((s) => s.value), { cx: SIZE / 2, cy: SIZE / 2, r: 60, innerR: 38 })
  return (
    <div className="pie">
      {portal}
      <div className="pie-figure">
        <svg width={SIZE} height={SIZE} viewBox={`0 0 ${SIZE} ${SIZE}`} role="img" aria-label="composition">
          {slices.map((sl, i) => {
            const s = shown[i]
            return (
              <path
                key={s.key}
                d={sl.d}
                fill={tone(s, i)}
                fillRule="evenodd"
                className="pie-slice"
                onMouseEnter={(e) =>
                  show(
                    e.currentTarget as unknown as HTMLElement,
                    <div className="chart-tip">
                      <div className="chart-tip-head">{s.label ?? s.key}</div>
                      <div className="chart-tip-row">
                        <span className="chart-tip-key">{formatValue(s.value, unit)}</span>
                        <span className="chart-tip-val mono">{Math.round((s.value / total) * 100)}%</span>
                      </div>
                    </div>,
                  )
                }
                onMouseLeave={hide}
              />
            )
          })}
        </svg>
        {centerLabel && <div className="pie-center">{centerLabel}</div>}
      </div>
      <div className="pie-legend">
        {shown.map((s, i) => (
          <span key={s.key} className="pie-legend-item">
            <i className="pie-legend-dot" style={{ background: tone(s, i) }} />
            <span className="pie-legend-label" title={s.label ?? s.key}>
              {s.label ?? s.key}
            </span>
            <b className="mono">{formatValue(s.value, unit)}</b>
            <span className="faint pie-legend-pct">{Math.round((s.value / total) * 100)}%</span>
          </span>
        ))}
      </div>
    </div>
  )
}

// ---- ranked horizontal bar list -----------------------------------------
// For non-time compositions where ordering matters (project effort, etc.): a
// labelled, value-sorted set of proportional bars.
export function BarList({ segments, unit, limit = 8 }: { segments: AnalyticsSegment[]; unit: string; limit?: number }) {
  const shown = segments.filter((s) => s.value > 0).slice(0, limit)
  const max = Math.max(1, ...shown.map((s) => s.value))
  if (shown.length === 0) return <div className="faint" style={{ padding: '8px 0' }}>no data</div>
  return (
    <div className="barlist">
      {shown.map((s, i) => (
        <div key={s.key} className="barlist-row">
          <span className="barlist-label" title={s.label ?? s.key}>
            {s.label ?? s.key}
          </span>
          <span className="barlist-track">
            <span className="barlist-fill" style={{ width: `${(s.value / max) * 100}%`, background: PALETTE[i % PALETTE.length] }} />
          </span>
          <span className="barlist-val mono">{formatValue(s.value, unit)}</span>
        </div>
      ))}
    </div>
  )
}

// ---- connector → task conversion ----------------------------------------
// Per source (Slack/GitHub): a readable observed → surfaced → tasks stat flow.
// Numbers sit OUTSIDE the bars (so they never truncate), with a thin proportional
// track underneath showing how steeply each stage drops off.
export function ConversionFunnel({ conversions }: { conversions: AnalyticsSourceConversion[] }) {
  if (conversions.length === 0) return <div className="faint" style={{ padding: '8px 0' }}>no connector activity</div>
  return (
    <div className="conversions">
      {conversions.map((c) => {
        const obs = Math.max(1, c.observed)
        const rate = (c.tasks / obs) * 100
        const rateLabel = c.tasks > 0 && rate < 1 ? '<1%' : `${Math.round(rate)}%`
        const stages = [
          { key: 'observed', label: 'observed', value: c.observed, tone: 'var(--text-3)' },
          { key: 'surfaced', label: 'surfaced', value: c.surfaced, tone: 'var(--accent)' },
          { key: 'tasks', label: 'tasks', value: c.tasks, tone: 'var(--ok)' },
        ]
        return (
          <div key={c.source} className="conv-row">
            <div className="conv-head">
              <SourceIcon source={c.source} size={14} />
              <span className="conv-source">{c.source}</span>
              <span className="spacer" />
              <span className="conv-rate mono" title="share of observed events that became tasks">
                {rateLabel} → task
              </span>
            </div>
            <div className="conv-flow">
              {stages.map((st, i) => (
                <div key={st.key} className="conv-stat">
                  {i > 0 && <span className="conv-arrow">→</span>}
                  <div className="conv-stat-cell">
                    <b className="num" style={{ color: st.tone }}>
                      {compact(st.value)}
                    </b>
                    <span className="conv-stat-label">{st.label}</span>
                    <span className="conv-track">
                      <span className="conv-track-fill" style={{ width: `${(st.value / obs) * 100}%`, background: st.tone }} />
                    </span>
                  </div>
                </div>
              ))}
            </div>
          </div>
        )
      })}
    </div>
  )
}
