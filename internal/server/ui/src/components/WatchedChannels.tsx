import { useMemo, useState } from 'react'
import { Hash, Loader2, Lock, Save, Search } from 'lucide-react'
import { useAction, useSettings, useSlackChannels } from '../lib/query'

// The setting the attention router reads (FLOW_STEERING_WATCH_CHANNELS). The
// generic settings form is told to skip this key (see Settings.tsx) so this
// checkbox picker is the single control for it.
const WATCH_KEY = 'FLOW_STEERING_WATCH_CHANNELS'

function parseIds(csv: string): string[] {
  return csv
    .split(/[,\s]+/)
    .map((s) => s.trim())
    .filter(Boolean)
}

// WatchedChannels is a checkbox picker for the channels the attention router
// watches (in addition to DMs + @mentions, which are always triaged). It reads
// the live channel list from /api/slack/channels and the current selection from
// the persisted FLOW_STEERING_WATCH_CHANNELS setting, and saves via the same
// update-settings action the rest of Settings uses.
export function WatchedChannels() {
  const { data: settings } = useSettings()
  const { data: channels, isLoading, error } = useSlackChannels()
  const action = useAction()

  const savedValue = useMemo(
    () => settings?.fields?.find((f) => f.key === WATCH_KEY)?.value ?? '',
    [settings],
  )
  const savedSet = useMemo(() => new Set(parseIds(savedValue)), [savedValue])

  // Local edits. null = "follow the saved value" (so a refresh after save
  // re-syncs the checkboxes from the server).
  const [selected, setSelected] = useState<Set<string> | null>(null)
  const [filter, setFilter] = useState('')

  const current = selected ?? savedSet
  const dirty = useMemo(() => {
    if (selected === null) return false
    if (selected.size !== savedSet.size) return true
    for (const id of selected) if (!savedSet.has(id)) return true
    return false
  }, [selected, savedSet])

  const toggle = (id: string) => {
    const next = new Set(current)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    setSelected(next)
  }

  const save = () => {
    action.mutate(
      { kind: 'update-settings', settings: { [WATCH_KEY]: Array.from(current).join(',') } },
      { onSuccess: () => setSelected(null) },
    )
  }

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase()
    const list = (channels ?? []).filter(
      (c) => !q || c.name.toLowerCase().includes(q) || c.id.toLowerCase().includes(q),
    )
    // Watched first, then alphabetical — so the current selection stays visible.
    return [...list].sort((a, b) => {
      const aw = current.has(a.id) ? 0 : 1
      const bw = current.has(b.id) ? 0 : 1
      if (aw !== bw) return aw - bw
      return a.name.localeCompare(b.name)
    })
  }, [channels, filter, current])

  return (
    <section className="settings-card">
      <div className="settings-card-head">
        <span>
          <Hash size={17} />
        </span>
        <h2>Watched channels</h2>
        <span className="spacer" />
        <span className="env-pill" title="channels the attention router watches">
          <span className="dot idle" />
          {current.size} watched
        </span>
      </div>
      <div className="settings-card-body">
        <p className="config-help">
          DMs and @mentions are always triaged. Tick channels to also watch them.
        </p>

        {isLoading ? (
          <div className="row gap dim">
            <Loader2 size={14} className="spin" /> loading channels…
          </div>
        ) : error ? (
          <div className="slack-error">
            Couldn&apos;t list channels — connect Slack (a bot token with <code>channels:read</code>) first.
          </div>
        ) : (channels ?? []).length === 0 ? (
          <div className="dim">No channels available yet. Connect Slack to populate this list.</div>
        ) : (
          <>
            <label className="wc-search row gap">
              <Search size={13} className="faint" />
              <input
                className="input"
                placeholder="filter channels…"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
              />
            </label>
            <div className="wc-list">
              {shown.map((c) => (
                <label key={c.id} className={`wc-row${current.has(c.id) ? ' on' : ''}`}>
                  <input type="checkbox" checked={current.has(c.id)} onChange={() => toggle(c.id)} />
                  {c.is_private ? <Lock size={12} className="faint" /> : <Hash size={12} className="faint" />}
                  <span className="wc-name clip">{c.name}</span>
                  <span className="spacer" />
                  <span className="wc-id mono faint">{c.id}</span>
                </label>
              ))}
            </div>
            <div className="config-actions">
              <button
                type="button"
                className="btn primary sm"
                disabled={!dirty || action.isPending}
                onClick={save}
              >
                {action.isPending ? <Loader2 size={14} className="spin" /> : <Save size={14} />}
                Save watched channels
              </button>
            </div>
          </>
        )}
      </div>
    </section>
  )
}
