// Client-side analytics derivations for the Mission Control trends.
//
// Throughput and time-to-done are computed from the task list (done tasks use
// updated_at as the completion proxy — there is no done_at column). Token cost
// is bucketed from the server's TOKEN_SERIES (a Sunday-aligned 84-day daily
// grid; see buildTokenSeries in ui_data.go). All weekly grids are Sunday-
// aligned to match the server's heatmap window so the dashboards line up.
import type { TaskView, TokenDay } from './types'

export interface WeekPoint {
  weekStart: string // YYYY-MM-DD, the Sunday that starts the week
  value: number
  cost?: number // estimated USD; only set by tokensByWeek
}

/** Local YYYY-MM-DD for a Date. */
function fmt(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

/**
 * The `weeks` most-recent Sunday week-start dates, oldest first, ending with
 * the Sunday of the week containing `now`. Mirrors the server's grid alignment.
 */
export function sundayWeekStarts(now: Date, weeks = 12): string[] {
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const thisSunday = new Date(today)
  thisSunday.setDate(today.getDate() - today.getDay()) // getDay(): Sun=0
  const out: string[] = []
  for (let i = weeks - 1; i >= 0; i--) {
    const d = new Date(thisSunday)
    d.setDate(thisSunday.getDate() - i * 7)
    out.push(fmt(d))
  }
  return out
}

/** Index of the week bucket a YYYY-MM-DD date falls into, or -1 if before the grid. */
function weekIndex(grid: string[], date: string): number {
  let idx = -1
  for (let i = 0; i < grid.length; i++) {
    if (date >= grid[i]) idx = i
    else break
  }
  return idx
}

/**
 * Tasks completed per week over the last `weeks` weeks. Completion is proxied
 * by updated_at (no done_at column exists); tasks updated after a done-flip are
 * a minor source of drift. Only status==='done' tasks count.
 */
export function throughputByWeek(tasks: TaskView[], now: Date, weeks = 12): WeekPoint[] {
  const grid = sundayWeekStarts(now, weeks)
  const out = grid.map((weekStart) => ({ weekStart, value: 0 }))
  for (const t of tasks) {
    if (t.status !== 'done' || !t.updated_at) continue
    const idx = weekIndex(grid, t.updated_at.slice(0, 10))
    if (idx >= 0) out[idx].value++
  }
  return out
}

export interface TimeToDone {
  medianDays: number
  avgDays: number
  count: number
}

/**
 * Median and average days from created_at → done for completed tasks (updated_at
 * as the done timestamp). Tasks with bad/negative spans are skipped.
 */
export function timeToDone(tasks: TaskView[]): TimeToDone {
  const days: number[] = []
  for (const t of tasks) {
    if (t.status !== 'done') continue
    const c = Date.parse(t.created_at)
    const u = Date.parse(t.updated_at)
    if (!Number.isFinite(c) || !Number.isFinite(u) || u < c) continue
    days.push((u - c) / 86_400_000)
  }
  if (days.length === 0) return { medianDays: 0, avgDays: 0, count: 0 }
  days.sort((a, b) => a - b)
  const mid = Math.floor(days.length / 2)
  const median = days.length % 2 ? days[mid] : (days[mid - 1] + days[mid]) / 2
  const avg = days.reduce((s, d) => s + d, 0) / days.length
  return { medianDays: median, avgDays: avg, count: days.length }
}

/**
 * Bucket the server's daily TOKEN_SERIES into weekly sums. The series is
 * Sunday-aligned and a multiple of 7 long, so each 7-day chunk is one week.
 * `cost` carries the matching estimated-USD sum for the bar's tooltip.
 */
export function tokensByWeek(series: TokenDay[]): WeekPoint[] {
  const out: WeekPoint[] = []
  for (let w = 0; w * 7 < series.length; w++) {
    const chunk = series.slice(w * 7, w * 7 + 7)
    if (chunk.length === 0) break
    out.push({
      weekStart: chunk[0].date,
      value: chunk.reduce((s, d) => s + d.tokens, 0),
      cost: chunk.reduce((s, d) => s + (d.cost_usd ?? 0), 0),
    })
  }
  return out
}
