// Dependency-free line diff for rendering Edit/Write tool calls as red/green
// diffs in the transcript chat view. We deliberately avoid adding an npm dep
// (the app ships a single go:embed bundle) — an LCS line diff is plenty for
// the small old_string→new_string snippets these tools carry.

export type DiffRowType = 'add' | 'del' | 'ctx'
export interface DiffRow {
  type: DiffRowType
  text: string
}

// Above this many lines on either side the O(n*m) LCS table gets expensive and
// the visual payoff vanishes — fall back to a plain "all removed, then all
// added" block, which is still correct, just not minimal.
const LCS_LINE_CAP = 1500

export function diffLines(oldStr: string, newStr: string): DiffRow[] {
  const a = oldStr.length ? oldStr.split('\n') : []
  const b = newStr.length ? newStr.split('\n') : []
  const n = a.length
  const m = b.length

  if (n === 0) return b.map((text) => ({ type: 'add' as const, text }))
  if (m === 0) return a.map((text) => ({ type: 'del' as const, text }))

  if (n > LCS_LINE_CAP || m > LCS_LINE_CAP) {
    return [
      ...a.map((text) => ({ type: 'del' as const, text })),
      ...b.map((text) => ({ type: 'add' as const, text })),
    ]
  }

  // lcs[i][j] = length of the longest common subsequence of a[i:] and b[j:].
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0))
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1])
    }
  }

  const rows: DiffRow[] = []
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      rows.push({ type: 'ctx', text: a[i] })
      i++
      j++
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      rows.push({ type: 'del', text: a[i] })
      i++
    } else {
      rows.push({ type: 'add', text: b[j] })
      j++
    }
  }
  while (i < n) rows.push({ type: 'del', text: a[i++] })
  while (j < m) rows.push({ type: 'add', text: b[j++] })
  return rows
}

export function countChanges(rows: DiffRow[]): { add: number; del: number } {
  let add = 0
  let del = 0
  for (const r of rows) {
    if (r.type === 'add') add++
    else if (r.type === 'del') del++
  }
  return { add, del }
}

// Collapse long runs of unchanged context to a single marker, GitHub-style, so
// a small edit inside a large snippet stays readable. `pad` context lines are
// kept on each side of a change.
export type CollapsedRow = DiffRow | { type: 'gap'; count: number }

export function collapseContext(rows: DiffRow[], pad = 3): CollapsedRow[] {
  // Mark which context rows are "near" a change (within `pad`).
  const keep = new Array(rows.length).fill(false)
  for (let i = 0; i < rows.length; i++) {
    if (rows[i].type !== 'ctx') {
      for (let k = Math.max(0, i - pad); k <= Math.min(rows.length - 1, i + pad); k++) keep[k] = true
    }
  }
  const out: CollapsedRow[] = []
  let run = 0
  for (let i = 0; i < rows.length; i++) {
    if (rows[i].type === 'ctx' && !keep[i]) {
      run++
      continue
    }
    if (run > 0) {
      out.push({ type: 'gap', count: run })
      run = 0
    }
    out.push(rows[i])
  }
  if (run > 0) out.push({ type: 'gap', count: run })
  return out
}
