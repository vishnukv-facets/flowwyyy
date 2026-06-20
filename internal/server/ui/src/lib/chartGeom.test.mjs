import { test } from 'node:test'
import assert from 'node:assert/strict'

import { niceCeil, ticks, project, pathFrom, areaFrom, cumulativeStacks, pieSlices, nearestIndex, labelIndices } from './chartGeom.ts'

test('niceCeil rounds up to a 1/2/5 × 10^n bound', () => {
  assert.equal(niceCeil(0), 1) // never 0 — avoids divide-by-zero scales
  assert.equal(niceCeil(1), 1)
  assert.equal(niceCeil(3), 5)
  assert.equal(niceCeil(7), 10)
  assert.equal(niceCeil(11), 20)
  assert.equal(niceCeil(230), 500)
  assert.equal(niceCeil(1500), 2000)
})

test('ticks spans 0..niceCeil in evenly spaced steps, ascending', () => {
  const t = ticks(7, 4) // niceCeil(7)=10
  assert.equal(t[0], 0)
  assert.equal(t[t.length - 1], 10)
  // strictly ascending
  for (let i = 1; i < t.length; i++) assert.ok(t[i] > t[i - 1])
})

test('project maps values across width and inverts y (0 at bottom)', () => {
  const pts = project([0, 5, 10], { w: 100, h: 50, max: 10 })
  assert.equal(pts.length, 3)
  // x evenly spaced 0..w
  assert.equal(pts[0].x, 0)
  assert.equal(pts[2].x, 100)
  assert.equal(pts[1].x, 50)
  // y inverted: value 0 → bottom (h), value max → top (0)
  assert.equal(pts[0].y, 50)
  assert.equal(pts[2].y, 0)
  assert.equal(pts[1].y, 25)
})

test('project clamps a single point to the left edge without NaN', () => {
  const pts = project([4], { w: 100, h: 50, max: 10 })
  assert.equal(pts.length, 1)
  assert.equal(pts[0].x, 0)
  assert.ok(Number.isFinite(pts[0].y))
})

test('pathFrom builds an M…L SVG path', () => {
  const d = pathFrom([
    { x: 0, y: 10 },
    { x: 5, y: 2 },
  ])
  assert.equal(d, 'M0 10L5 2')
})

test('areaFrom closes the path down to the baseline', () => {
  const d = areaFrom(
    [
      { x: 0, y: 10 },
      { x: 10, y: 4 },
    ],
    50,
  )
  // starts at baseline under first x, traces the line, drops to baseline under last x, closes
  assert.ok(d.startsWith('M0 50'))
  assert.ok(d.endsWith('Z'))
  assert.ok(d.includes('L10 50')) // returns to baseline at the last x
})

test('cumulativeStacks returns running tops per series', () => {
  const tops = cumulativeStacks([
    [1, 2, 3],
    [10, 20, 30],
  ])
  assert.deepEqual(tops[0], [1, 2, 3])
  assert.deepEqual(tops[1], [11, 22, 33]) // series 1 stacked on top of series 0
})

const TWO_PI = Math.PI * 2
const span = (s) => s.a1 - s.a0

test('pieSlices splits the circle proportionally and contiguously', () => {
  const s = pieSlices([1, 1, 2], { cx: 50, cy: 50, r: 40 })
  assert.equal(s.length, 3)
  // total sweep is a full circle
  assert.ok(Math.abs((s[2].a1 - s[0].a0) - TWO_PI) < 1e-9)
  // slices are contiguous (no gaps)
  assert.ok(Math.abs(s[0].a1 - s[1].a0) < 1e-9)
  assert.ok(Math.abs(s[1].a1 - s[2].a0) < 1e-9)
  // the value-2 slice sweeps twice the value-1 slice
  assert.ok(Math.abs(span(s[2]) - 2 * span(s[0])) < 1e-9)
  // every slice yields a real SVG arc path
  for (const sl of s) {
    assert.ok(sl.d.startsWith('M'))
    assert.ok(sl.d.includes('A'))
  }
})

test('pieSlices renders a single 100% value as a full ring', () => {
  const s = pieSlices([5], { cx: 60, cy: 60, r: 50, innerR: 30 })
  assert.equal(s.length, 1)
  assert.ok(Math.abs(span(s[0]) - TWO_PI) < 1e-9)
  assert.ok(s[0].d.startsWith('M'))
  assert.ok(s[0].d.includes('A'))
})

test('pieSlices on an all-zero set produces zero-width slices', () => {
  const s = pieSlices([0, 0], { cx: 10, cy: 10, r: 8 })
  assert.equal(s.length, 2)
  for (const sl of s) assert.equal(span(sl), 0)
})

test('labelIndices spaces x-axis ticks without colliding at the ends', () => {
  // few points → label them all
  assert.deepEqual(labelIndices(5), [0, 1, 2, 3, 4])
  assert.deepEqual(labelIndices(1), [0])
  assert.deepEqual(labelIndices(2), [0, 1])

  // many points → evenly spaced, always includes first + last, never adjacent
  assert.deepEqual(labelIndices(27), [0, 6, 12, 18, 26])
  assert.deepEqual(labelIndices(8), [0, 2, 4, 7]) // 6 then 7 would collide → 6 dropped

  for (const n of [7, 8, 11, 15, 20, 27, 30, 53]) {
    const ix = labelIndices(n)
    assert.equal(ix[0], 0, `n=${n} starts at 0`)
    assert.equal(ix[ix.length - 1], n - 1, `n=${n} ends at last`)
    for (let k = 1; k < ix.length; k++) {
      assert.ok(ix[k] > ix[k - 1], `n=${n} strictly ascending`)
      assert.ok(ix[k] - ix[k - 1] >= 2, `n=${n} no adjacent ticks (gap ${ix[k] - ix[k - 1]})`)
    }
  }
})

test('nearestIndex snaps a cursor x to the closest data point and clamps', () => {
  // 8 points across a 700px plot → spacing 100px.
  assert.equal(nearestIndex(0, 700, 8), 0)
  assert.equal(nearestIndex(700, 700, 8), 7)
  assert.equal(nearestIndex(349, 700, 8), 3) // 0.4986 * 7 = 3.49 → rounds to 3
  assert.equal(nearestIndex(351, 700, 8), 4) // 0.5014 * 7 = 3.51 → rounds to 4
  // out of bounds clamps
  assert.equal(nearestIndex(-50, 700, 8), 0)
  assert.equal(nearestIndex(9999, 700, 8), 7)
  // degenerate
  assert.equal(nearestIndex(123, 700, 1), 0)
  assert.equal(nearestIndex(123, 0, 8), 0)
})
