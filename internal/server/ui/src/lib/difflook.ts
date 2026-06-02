// "Since I last looked" tracking for the diff tab.
//
// Persists a per-file signature snapshot to localStorage, keyed by task slug, so
// returning to a session's diff can highlight which files changed since the last
// visit. Best-effort: storage may be disabled, so reads/writes never throw and
// degrade to "nothing changed". Mirrors the localStorage convention in
// recents.ts.
import type { DiffFile } from './types'

/** Cheap per-file change proxy: adds / removes / hunk count. */
export function fileSignature(f: DiffFile): string {
  return `${f.add}:${f.rem}:${(f.hunks ?? []).length}`
}

function storageKey(slug: string): string {
  return `flow.difflook.${slug}`
}

function readBaseline(slug: string): Record<string, string> | null {
  try {
    const raw = localStorage.getItem(storageKey(slug))
    if (!raw) return null
    const obj = JSON.parse(raw)
    return obj && typeof obj === 'object' ? (obj as Record<string, string>) : null
  } catch {
    return null
  }
}

/**
 * Filenames that are new or changed since the last visit. Returns an empty set
 * on the first-ever visit (no baseline) — with no prior reference, nothing is
 * meaningfully "new". Does not write; pair with commitLook to advance the
 * baseline.
 */
export function changedSinceLook(slug: string, files: DiffFile[]): Set<string> {
  const prev = readBaseline(slug)
  const changed = new Set<string>()
  if (!prev) return changed
  for (const f of files) {
    if (prev[f.name] !== fileSignature(f)) changed.add(f.name)
  }
  return changed
}

/** Persist the current files as the new "last looked" baseline (best-effort). */
export function commitLook(slug: string, files: DiffFile[]): void {
  const cur: Record<string, string> = {}
  for (const f of files) cur[f.name] = fileSignature(f)
  try {
    localStorage.setItem(storageKey(slug), JSON.stringify(cur))
  } catch {
    // storage full / disabled — look-tracking is best-effort
  }
}
