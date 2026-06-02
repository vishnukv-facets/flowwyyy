import { useEffect, useMemo, useState } from 'react'
import { X } from 'lucide-react'
import { ProviderIcon } from './ui'
import { useFloatingTerminals } from '../lib/floatingTerminals'
import { useUiData } from '../lib/query'

// AttentionWidget is the in-app replacement for desktop/bell notifications:
// whenever ANY session is waiting on you it surfaces a persistent popup card
// ("… needs your input") with Open / Dismiss. Click Open and the session pops
// up as the small hovering terminal (popped out to the tray if it wasn't
// already) — submit, then minimize it back to the tray. Nothing renders while
// everything's handled. A dismissed card re-appears if that session goes back
// to waiting. The session you're already viewing (its page is open) is never
// nagged about — you can see it.
interface AttnItem {
  id: string
  kind: 'floating' | 'task'
  provider: string
  title: string
  why?: string
}

export function AttentionWidget() {
  const { windows, restore, popOut, activeSessionSlug } = useFloatingTerminals()
  const { data: ui } = useUiData()
  const [dismissed, setDismissed] = useState<Record<string, boolean>>({})

  const items = useMemo<AttnItem[]>(() => {
    const byId = new Map<string, AttnItem>()
    // Adhoc Ask Flow sessions carry their own waiting flag (they have no page).
    for (const w of windows) {
      if (w.kind === 'floating' && w.waiting) {
        byId.set(w.id, { id: w.id, kind: 'floating', provider: w.provider, title: w.title || 'Ask Flow', why: w.waitingWhy })
      }
    }
    // Every task session awaiting input — popped out or not. Skip the one whose
    // page you're already on.
    for (const a of ui?.AGENTS ?? []) {
      if (a.status === 'waiting' && a.slug !== activeSessionSlug && !byId.has(a.slug)) {
        byId.set(a.slug, { id: a.slug, kind: 'task', provider: a.provider, title: a.name, why: a.waiting_for?.why })
      }
    }
    return [...byId.values()]
  }, [windows, ui?.AGENTS, activeSessionSlug])

  // Forget dismissals once a session stops waiting, so the next time it needs
  // input it alerts again rather than staying silent.
  useEffect(() => {
    const ids = new Set(items.map((i) => i.id))
    setDismissed((d) => {
      let changed = false
      const next: Record<string, boolean> = {}
      for (const id of Object.keys(d)) {
        if (ids.has(id)) next[id] = d[id]
        else changed = true
      }
      return changed ? next : d
    })
  }, [items])

  const needsYou = items.filter((i) => !dismissed[i.id])
  if (needsYou.length === 0) return null

  // Open → the small hovering terminal. Tasks get popped out to the tray (which
  // also opens the window); adhoc sessions just restore.
  const open = (it: AttnItem) => {
    if (it.kind === 'task') popOut({ slug: it.id, provider: it.provider, title: it.title })
    else restore(it.id)
  }

  return (
    <div className="attn-stack" role="region" aria-label="Sessions needing your input">
      {needsYou.map((it) => (
        <div key={it.id} className="attn-pop" role="alert">
          <span className="attn-pop-dot" />
          <span className="attn-pop-icon">
            <ProviderIcon provider={it.provider} size={15} />
          </span>
          <div className="attn-pop-main">
            <div className="attn-pop-title clip">{it.title || 'Ask Flow'} needs your input</div>
            <div className="attn-pop-sub clip">{it.why || 'The session is waiting for your response.'}</div>
          </div>
          <div className="attn-pop-actions">
            <button type="button" className="btn primary sm" onClick={() => open(it)}>
              Open
            </button>
            <button
              type="button"
              className="btn icon sm"
              title="Dismiss"
              onClick={() => setDismissed((d) => ({ ...d, [it.id]: true }))}
            >
              <X size={13} />
            </button>
          </div>
        </div>
      ))}
    </div>
  )
}
