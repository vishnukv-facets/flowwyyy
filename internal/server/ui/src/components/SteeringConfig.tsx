import { useMemo, type ReactNode } from 'react'
import { BellOff, Clock, Filter, Gauge, Hash, Loader2, Save } from 'lucide-react'
import { useSettings } from '../lib/query'
import { ConfigField, SettingsPanel, SettingsSection, useConfigDraft } from './SettingsPanels'
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
const MUTED_KEYWORD_KEYS = ['FLOW_STEERING_MUTED_KEYWORDS']
const WAITING_KEYS = ['FLOW_STEERING_AUTO_RESOLVE_WAITING']
const PERFORMANCE_KEYS = [
  'FLOW_STEERING_SEND_MODEL',
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

export function SteeringConfig() {
  return (
    <>
      <SettingsSection title="Triage scope" hint="What the router watches, mutes, and drops before triage.">
        <div className="settings-grid">
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
        </div>
      </SettingsSection>

      <SettingsSection title="Autonomy" hint="What the steerer may do without asking. Outward replies always stay manual.">
        <div className="settings-grid">
          <AutonomyPanel />
          <ConfigGroupPanel title="Waiting follow-up" icon={<Clock size={17} />} fieldKeys={WAITING_KEYS} />
        </div>
      </SettingsSection>

      <SettingsSection title="Performance" hint="Reply send model and classifier subprocess budget.">
        <div className="settings-grid">
          <ConfigGroupPanel title="Reply & classifier" icon={<Gauge size={17} />} fieldKeys={PERFORMANCE_KEYS} />
        </div>
      </SettingsSection>
    </>
  )
}
