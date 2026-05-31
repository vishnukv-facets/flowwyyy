// Recently-opened items from the command palette, persisted to localStorage so
// "where was I?" survives reloads. Keyed by route (`to`) — re-opening an item
// moves it to the top rather than duplicating.
export interface RecentItem {
  label: string
  sub?: string
  to: string
  type?: string
  at: number
}

const KEY = 'flow.recents'
const MAX = 8

export function getRecents(): RecentItem[] {
  try {
    const raw = JSON.parse(localStorage.getItem(KEY) || '[]')
    return Array.isArray(raw) ? raw : []
  } catch {
    return []
  }
}

export function pushRecent(it: { label: string; sub?: string; to: string; type?: string }) {
  if (!it.to || it.to === '/') return
  const list = getRecents().filter((r) => r.to !== it.to)
  list.unshift({ ...it, at: Date.now() })
  try {
    localStorage.setItem(KEY, JSON.stringify(list.slice(0, MAX)))
  } catch {
    // storage full / disabled — recents are best-effort
  }
}
