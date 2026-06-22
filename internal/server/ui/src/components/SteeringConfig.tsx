import { useMemo, useState, type ReactNode } from 'react'
import { BellOff, Clock, Filter, Gauge, Hash, Loader2, MessagesSquare, PenLine, Save, ShieldCheck } from 'lucide-react'
import { usePersona, useSavePersona, useSettings } from '../lib/query'
import { ConfigField, SettingsPanel, useConfigDraft } from './SettingsPanels'
import { ChannelPicker } from './ChannelPicker'
import { AutonomyPanel } from './AutonomyPanel'

// SteeringConfig is the attention router's single configuration surface, rendered
// as the `config` view of the Attention screen (co-located with the feed it
// governs). It relocates every steering knob that used to be split across two
// Settings sections, grouped as Triage scope / Autonomy / Performance. It's a
// pure presentation layer over the same /api/settings keys — no backend change.

// Steering keys driven by the generic ConfigField renderer (the rich pickers own
// FLOW_STEERING_WATCH_CHANNELS / _MUTED_CHANNELS and _AUTONOMY). Module-level so
// the arrays stay referentially stable across renders.
// The per-channel session model master switch + its session provider. Both are
// env-only registry keys; surfaced here so the steerer's defining setting is
// operator-toggleable instead of requiring a shell export.
const SESSION_KEYS = ['FLOW_STEERING_SESSIONS', 'FLOW_STEERER_DEFAULT_PROVIDER']
const MUTED_KEYWORD_KEYS = ['FLOW_STEERING_MUTED_KEYWORDS']
const WAITING_KEYS = ['FLOW_STEERING_AUTO_RESOLVE_WAITING']
const AUTO_PERMIT_KEYS = ['FLOW_STEERING_AUTO_PERMIT_UNATTENDED', 'FLOW_STEERING_AUTO_PERMIT_MIN_CONF']
const PERFORMANCE_KEYS = [
  'FLOW_STEERING_CLASSIFIER_BUDGET_PER_HOUR',
  'FLOW_STEERING_CLASSIFIER_FAILURE_COOLDOWN',
]

// ConfigGroupPanel renders the given setting keys as a single card with staged
// edits + a Save button — the same stage→diff→save lifecycle as Settings.tsx's
// ConfigPanels, scoped to an explicit key list and ordered by it.
function ConfigGroupPanel({ title, icon, fieldKeys }: { title: string; icon: ReactNode; fieldKeys: string[] }) {
  const { data } = useSettings()
  const cfg = useConfigDraft()
  const fields = useMemo(
    () =>
      (data?.fields ?? [])
        .filter((f) => fieldKeys.includes(f.key))
        .sort((a, b) => fieldKeys.indexOf(a.key) - fieldKeys.indexOf(b.key)),
    [data?.fields, fieldKeys],
  )

  if (fields.length === 0) return null
  const dirty = Object.keys(cfg.changesFor(fields)).length > 0

  return (
    <SettingsPanel title={title} icon={icon}>
      <div className="config-form">
        {fields.map((f) => (
          <ConfigField key={f.key} field={f} draft={cfg.draft[f.key]} onChange={(v) => cfg.setField(f.key, v)} />
        ))}
        <div className="config-actions">
          <button type="button" className="btn primary" disabled={!dirty || cfg.isPending} onClick={() => cfg.save(fields)}>
            {cfg.isPending ? <Loader2 size={14} className="spin" /> : <Save size={14} />}
            Save {title}
          </button>
        </div>
      </div>
    </SettingsPanel>
  )
}

// VoicePanel edits the operator's persona/voice (persona.md), injected into the
// steerer's drafting + send prompts so replies read like the operator, not a
// bot. Plain markdown over /api/persona — staged locally, saved on click.
function VoicePanel() {
  const { data, isLoading } = usePersona()
  const save = useSavePersona()
  const [draft, setDraft] = useState<string | null>(null)
  const value = draft ?? data ?? ''
  const dirty = draft !== null && draft !== (data ?? '')
  return (
    <SettingsPanel title="Voice" icon={<PenLine size={17} />}>
      <div className="config-form">
        <p className="voice-note">
          flow writes Slack/GitHub replies in this voice — injected into the draft and
          send prompts so they read like you, not a bot. Edit it below: tone, phrasing,
          greetings, sign-offs, and do/don'ts.
        </p>
        <textarea
          className="voice-editor"
          value={value}
          disabled={isLoading || save.isPending}
          placeholder="Describe your voice — tone, phrasing, greetings, sign-offs, and do/don'ts. Lines inside <!-- --> are notes and aren't sent to the model."
          onChange={(e) => setDraft(e.target.value)}
          rows={5}
          spellCheck
        />
        <div className="config-actions">
          <button
            type="button"
            className="btn primary"
            disabled={!dirty || save.isPending}
            onClick={() => save.mutate(value, { onSuccess: () => setDraft(null) })}
          >
            {save.isPending ? <Loader2 size={14} className="spin" /> : <Save size={14} />}
            Save voice
          </button>
        </div>
      </div>
    </SettingsPanel>
  )
}

// ConfigHead is a full-width row label inside the single steering-config grid —
// it replaces the per-section card wrapper so the small knob-boxes below it can
// pack horizontally instead of each section stacking a lone full-width card.
function ConfigHead({ title, hint }: { title: string; hint: string }) {
  return (
    <div className="steering-config-head settings-section-head">
      <span className="eyebrow">{title}</span>
      <span className="settings-section-hint">{hint}</span>
    </div>
  )
}

export function SteeringConfig() {
  return (
    <div className="steering-config">
      <VoicePanel />
      <ConfigGroupPanel title="Per-channel sessions" icon={<MessagesSquare size={17} />} fieldKeys={SESSION_KEYS} />

      <ConfigHead title="Triage scope" hint="What the router watches, mutes, and drops before triage." />
      <ChannelPicker
        settingKey="FLOW_STEERING_WATCH_CHANNELS"
        title="Watched channels"
        icon={<Hash size={17} />}
        help="DMs and @mentions are always triaged. Tick channels to also watch them."
        pillNoun="watched"
        saveLabel="Save watched channels"
        savedActiveHint="Your saved watch list is still active."
      />
      <ChannelPicker
        settingKey="FLOW_STEERING_MUTED_CHANNELS"
        title="Muted channels"
        icon={<BellOff size={17} />}
        help="Channels you never want surfaced — messages from them are dropped before triage."
        pillNoun="muted"
        saveLabel="Save muted channels"
        savedActiveHint="Your saved mute list is still active."
      />
      <ConfigGroupPanel title="Muted keywords" icon={<Filter size={17} />} fieldKeys={MUTED_KEYWORD_KEYS} />
      <ChannelPicker
        settingKey="FLOW_STEERING_TRUSTED_CHANNELS"
        title="Trusted sources"
        icon={<ShieldCheck size={17} />}
        help="DMs, group DMs, and channels whose forwarded content may be auto-permitted into an unattended (bypass/auto) session. Your own messages are always trusted; tick others you trust. Pairs with Auto-permit below."
        pillNoun="trusted"
        saveLabel="Save trusted sources"
        savedActiveHint="Your saved trusted list is still active."
        kinds={['channel', 'im', 'mpim']}
      />

      <ConfigHead title="Autonomy" hint="What the steerer may do without asking. Outward replies always stay manual." />
      <AutonomyPanel />
      <ConfigGroupPanel title="Waiting follow-up" icon={<Clock size={17} />} fieldKeys={WAITING_KEYS} />
      <ConfigGroupPanel title="Auto-permit (unattended)" icon={<ShieldCheck size={17} />} fieldKeys={AUTO_PERMIT_KEYS} />

      <ConfigHead title="Performance" hint="Classifier subprocess budget." />
      <ConfigGroupPanel title="Classifier" icon={<Gauge size={17} />} fieldKeys={PERFORMANCE_KEYS} />
    </div>
  )
}
