import { useMemo, useState, type ReactNode } from 'react'
import { Hash, Loader2, Lock, MessageCircle, Save, Search, Users } from 'lucide-react'
import { useAction, useSettings, useSlackChannels } from '../lib/query'
import type { SlackChannel } from '../lib/types'

// ChannelPicker is a checkbox picker over the live Slack channel list bound to a
// single comma-separated channel-ID setting. It's the shared control behind both
// the attention router's "watched" channels (FLOW_STEERING_WATCH_CHANNELS) and
// its "muted" channels (FLOW_STEERING_MUTED_CHANNELS) — the same decision, so the
// same crafted control. The generic settings form is told to skip these keys (see
// Settings.tsx / SteeringConfig.tsx) so this picker is their sole editor.
//
// Saving writes Array.from(current), where `current` starts from the saved set —
// so channel IDs not present in the live list (e.g. a channel you've left) are
// preserved across a save rather than silently dropped.

export interface ChannelPickerProps {
  /** The setting key this picker reads/writes (a comma-separated channel-ID CSV). */
  settingKey: string
  title: string
  icon: ReactNode
  /** One-line description shown above the list. */
  help: string
  /** Noun for the count pill, e.g. "watched" / "muted". */
  pillNoun: string
  /** Label for the save button, e.g. "Save watched channels". */
  saveLabel: string
  /** Shown in the Slack-error fallback when a saved selection still exists. */
  savedActiveHint: string
  /**
   * Conversation kinds this picker offers. Defaults to channels only so the
   * watched/muted pickers are unchanged; the trusted-sources picker passes
   * ['channel','im','mpim'] to also offer DMs and group DMs.
   */
  kinds?: string[]
}

// kindOf normalizes a channel's kind (absent → "channel").
function kindOf(c: SlackChannel): string {
  return c.kind || 'channel'
}

// kindIcon picks the row icon: DM, group DM, or public/private channel.
function kindIcon(c: SlackChannel): ReactNode {
  switch (kindOf(c)) {
    case 'im':
      return <MessageCircle size={12} className="faint" />
    case 'mpim':
      return <Users size={12} className="faint" />
    default:
      return c.is_private ? <Lock size={12} className="faint" /> : <Hash size={12} className="faint" />
  }
}

function parseIds(csv: string): string[] {
  return csv
    .split(/[,\s]+/)
    .flatMap((s) => {
      const id = s.trim()
      return id ? [id] : []
    })
}

export function ChannelPicker({ settingKey, title, icon, help, pillNoun, saveLabel, savedActiveHint, kinds }: ChannelPickerProps) {
  const { data: settings } = useSettings()
  const { data: channels, isLoading, error } = useSlackChannels()
  const action = useAction()

  const savedValue = useMemo(
    () => settings?.fields?.find((f) => f.key === settingKey)?.value ?? '',
    [settings, settingKey],
  )
  const savedSet = useMemo(() => new Set(parseIds(savedValue)), [savedValue])

  // Local edits. null = "follow the saved value" (so a refresh after save
  // re-syncs the checkboxes from the server).
  const [selected, setSelected] = useState<Set<string> | null>(null)
  const [filter, setFilter] = useState('')

  const current = selected ?? savedSet
  const savedIDs = useMemo(() => Array.from(current).sort(), [current])
  const errorMessage = error instanceof Error ? error.message : error ? String(error) : ''
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
      { kind: 'update-settings', settings: { [settingKey]: Array.from(current).join(',') } },
      { onSuccess: () => setSelected(null) },
    )
  }

  const kindsKey = (kinds ?? ['channel']).join(',')
  const shown = useMemo(() => {
    const allow = new Set(kindsKey.split(','))
    const q = filter.trim().toLowerCase()
    const list = (channels ?? []).filter(
      (c) => allow.has(kindOf(c)) && (!q || c.name.toLowerCase().includes(q) || c.id.toLowerCase().includes(q)),
    )
    // Selected first, then alphabetical — so the current selection stays visible.
    return list.sort((a, b) => {
      const aw = current.has(a.id) ? 0 : 1
      const bw = current.has(b.id) ? 0 : 1
      if (aw !== bw) return aw - bw
      return a.name.localeCompare(b.name)
    })
  }, [channels, filter, current, kindsKey])

  return (
    <section className="settings-card">
      <div className="settings-card-head">
        <span>{icon}</span>
        <h2>{title}</h2>
        <span className="spacer" />
        <span className="env-pill" title={`channels currently ${pillNoun}`}>
          <span className="dot idle" />
          {current.size} {pillNoun}
        </span>
      </div>
      <div className="settings-card-body">
        <p className="config-help">{help}</p>

        {isLoading ? (
          <div className="row gap dim">
            <Loader2 size={14} className="spin" /> loading channels…
          </div>
        ) : error ? (
          <div className="wc-fallback">
            <div className="slack-error">
              Couldn&apos;t list Slack channels: {errorMessage || 'unknown error'}.
              {savedIDs.length > 0 ? ` ${savedActiveHint}` : ' Connect Slack with channel-list access to pick channels here.'}
            </div>
            {savedIDs.length > 0 ? (
              <div className="wc-list saved-only">
                {savedIDs.map((id) => (
                  <div key={id} className="wc-row on">
                    <Hash size={12} className="faint" />
                    <span className="wc-name clip">saved channel</span>
                    <span className="spacer" />
                    <span className="wc-id mono faint">{id}</span>
                  </div>
                ))}
              </div>
            ) : null}
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
                  {kindIcon(c)}
                  <span className="wc-name clip">{c.name || c.id}</span>
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
                {saveLabel}
              </button>
            </div>
          </>
        )}
      </div>
    </section>
  )
}
