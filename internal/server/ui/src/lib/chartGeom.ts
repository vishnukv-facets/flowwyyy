// Pure SVG chart geometry for the analytics page. No React, no DOM — just the
// math that maps a series of numbers onto an SVG viewbox. Kept separate from the
// chart components (charts.tsx) so the geometry is unit-testable (chartGeom.test.mjs).

export interface Pt {
  x: number
  y: number
}

/**
 * Round `v` up to a "nice" axis bound: 1, 2, or 5 times a power of ten. Never
 * returns 0 (an empty series still gets a 1-high axis, so scales never divide
 * by zero). niceCeil(7) → 10, niceCeil(230) → 500.
 */
export function niceCeil(v: number): number {
  if (!Number.isFinite(v) || v <= 1) return 1
  const exp = Math.floor(Math.log10(v))
  const pow = Math.pow(10, exp)
  const frac = v / pow
  let nice: number
  if (frac <= 1) nice = 1
  else if (frac <= 2) nice = 2
  else if (frac <= 5) nice = 5
  else nice = 10
  return nice * pow
}

/** `count`+1 evenly spaced tick values from 0 to niceCeil(max), ascending. */
export function ticks(max: number, count = 4): number[] {
  const top = niceCeil(max)
  const step = top / count
  const out: number[] = []
  for (let i = 0; i <= count; i++) out.push(step * i)
  return out
}

/**
 * Map `values` to points in a w×h box: x evenly spaced across [0,w], y scaled
 * against `max` and inverted (value 0 → y=h bottom, value max → y=0 top). A
 * single value pins to x=0. `max` is niceCeil'd so the top of the data sits just
 * under the chart ceiling.
 */
export function project(values: number[], opts: { w: number; h: number; max: number }): Pt[] {
  const { w, h } = opts
  const top = niceCeil(opts.max)
  const n = values.length
  return values.map((v, i) => ({
    x: n <= 1 ? 0 : (i / (n - 1)) * w,
    y: h - (v / top) * h,
  }))
}

/** An SVG "M…L…" polyline path through the points. */
export function pathFrom(points: Pt[]): string {
  if (points.length === 0) return ''
  return points
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${round(p.x)} ${round(p.y)}`)
    .join('')
}

/**
 * A closed area path: baseline under the first x, up the line, down to the
 * baseline under the last x, and closed. `baseline` is the y of the chart floor
 * (usually h).
 */
export function areaFrom(points: Pt[], baseline: number): string {
  if (points.length === 0) return ''
  const first = points[0]
  const last = points[points.length - 1]
  const line = points.map((p) => `L${round(p.x)} ${round(p.y)}`).join('')
  return `M${round(first.x)} ${round(baseline)}${line}L${round(last.x)} ${round(baseline)}Z`
}

/**
 * Running cumulative tops for a stacked chart: tops[k][i] is the sum of
 * series[0..k][i]. Each band k is drawn between tops[k-1] (lower) and tops[k]
 * (upper). All series must share the same length.
 */
export function cumulativeStacks(series: number[][]): number[][] {
  const tops: number[][] = []
  let running: number[] | null = null
  for (const s of series) {
    running = running ? running.map((v, i) => v + (s[i] ?? 0)) : s.slice()
    tops.push(running.slice())
  }
  return tops
}

// SVG paths don't need sub-pixel precision; trim to 2 decimals to keep the DOM small.
function round(n: number): number {
  return Math.round(n * 100) / 100
}

/**
 * Pick up to `max` evenly-spaced x-axis tick indices that always include the
 * first (0) and last (n-1) point. When the last stepped tick would land within
 * one step of the end it's dropped, so the final two labels never render on top
 * of each other. Few points (n ≤ max) are all labelled.
 */
export function labelIndices(n: number, max = 6): number[] {
  if (n <= 0) return []
  if (n <= max) return Array.from({ length: n }, (_, i) => i)
  const step = Math.ceil((n - 1) / (max - 1))
  const out: number[] = []
  for (let i = 0; i < n - 1; i += step) out.push(i)
  if (out.length && n - 1 - out[out.length - 1] < step) out.pop()
  out.push(n - 1)
  return out
}

/**
 * The data-point index nearest a cursor x-offset within the plot. `relX` is the
 * cursor position measured from the plot's left edge (NOT the SVG origin), so it
 * needs no axis-padding adjustment. Clamps to [0, n-1]; returns 0 for a single
 * point or zero-width plot.
 */
export function nearestIndex(relX: number, plotW: number, n: number): number {
  if (n <= 1 || plotW <= 0) return 0
  const frac = relX / plotW
  return Math.max(0, Math.min(n - 1, Math.round(frac * (n - 1))))
}

// ---- pie / donut geometry -----------------------------------------------

export interface PieSlice {
  index: number
  value: number
  a0: number // start angle (radians, 0 = 3 o'clock; slices begin at -π/2 = top)
  a1: number // end angle
  d: string // SVG path for the slice (a ring sector when innerR > 0)
}

const TWO_PI = Math.PI * 2

/**
 * Split a circle into proportional slices, sweeping clockwise from the top
 * (−π/2). With innerR > 0 each slice is a donut ring sector. A single non-zero
 * value renders as a full ring (drawn as two arcs so the path isn't degenerate);
 * zero values produce zero-width slices the caller can skip.
 */
export function pieSlices(values: number[], opts: { cx: number; cy: number; r: number; innerR?: number }): PieSlice[] {
  const { cx, cy, r } = opts
  const innerR = opts.innerR ?? 0
  const total = values.reduce((s, v) => s + Math.max(0, v), 0)
  let a = -Math.PI / 2
  const out: PieSlice[] = []
  for (let i = 0; i < values.length; i++) {
    const v = Math.max(0, values[i])
    const frac = total > 0 ? v / total : 0
    const a0 = a
    const a1 = a + frac * TWO_PI
    a = a1
    out.push({ index: i, value: values[i], a0, a1, d: ringSector(cx, cy, r, innerR, a0, a1) })
  }
  return out
}

function polar(cx: number, cy: number, rad: number, ang: number): string {
  return `${round(cx + rad * Math.cos(ang))} ${round(cy + rad * Math.sin(ang))}`
}

// SVG path for a sector between angles a0..a1. A ring sector (donut) when ir>0,
// a wedge from the centre otherwise. The full-circle case is split into two
// semicircle arcs because a single 360° arc collapses to nothing.
function ringSector(cx: number, cy: number, r: number, ir: number, a0: number, a1: number): string {
  if (a1 - a0 >= TWO_PI - 1e-9) {
    const mid = a0 + Math.PI
    const outer = `M${polar(cx, cy, r, a0)}A${r} ${r} 0 1 1 ${polar(cx, cy, r, mid)}A${r} ${r} 0 1 1 ${polar(cx, cy, r, a0)}`
    if (ir > 0) {
      // evenodd fill + a reverse-wound inner circle carves the hole.
      return `${outer}M${polar(cx, cy, ir, a0)}A${ir} ${ir} 0 1 0 ${polar(cx, cy, ir, mid)}A${ir} ${ir} 0 1 0 ${polar(cx, cy, ir, a0)}Z`
    }
    return `${outer}Z`
  }
  const large = a1 - a0 > Math.PI ? 1 : 0
  if (ir > 0) {
    return `M${polar(cx, cy, r, a0)}A${r} ${r} 0 ${large} 1 ${polar(cx, cy, r, a1)}L${polar(cx, cy, ir, a1)}A${ir} ${ir} 0 ${large} 0 ${polar(cx, cy, ir, a0)}Z`
  }
  return `M${round(cx)} ${round(cy)}L${polar(cx, cy, r, a0)}A${r} ${r} 0 ${large} 1 ${polar(cx, cy, r, a1)}Z`
}
