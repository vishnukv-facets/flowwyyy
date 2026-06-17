import { useMemo, useState } from 'react'
import { Loader2, Save, Zap } from 'lucide-react'
import { useAction, useSettings } from '../lib/query'

// The setting the steerer reads (FLOW_STEERING_AUTONOMY): a JSON object mapping
// action name → {enabled, threshold}. The generic ConfigPanels form filters this
// key out, so this panel is its sole control. Saved via the same update-settings
// action the rest of Settings uses.
const AUTONOMY_KEY = 'FLOW_STEERING_AUTONOMY'

type ActionPolicy = { enabled: boolean; threshold: number }
type Policy = Record<string, ActionPolicy>

// Auto-actable actions. make_task/forward are medium-risk. reply is CRITICAL —
// it posts to a colleague's thread/DM with no per-message click — so it stays OFF
// by default at a high (0.95) threshold; turning it on is an explicit opt-in.
// afk_reply is still operator-confirmed-only (no send path) and not shown.
const ACTIONS: {
  key: string
  label: string
  defaultThreshold: number
  risk: string
  audit: string
}[] = [
  {
    key: 'make_task',
    label: 'Make task',
    defaultThreshold: 0.8,
    risk: 'medium',
    audit: 'trace + feedback + task link',
  },
  {
    key: 'forward',
    label: 'Forward to matched task',
    defaultThreshold: 0.85,
    risk: 'medium',
    audit: 'trace + feedback + inbox link',
  },
  {
    key: 'reply',
    label: 'Auto-send reply',
    defaultThreshold: 0.95,
    risk: 'critical',
    audit: 'posts via the channel chat / gh agent — no per-message click',
  },
]

// Parse the saved JSON defensively: empty/invalid → {}. Only well-formed
// per-action entries (enabled bool, threshold number in [0,1]) survive.
function parsePolicy(raw: string): Policy {
  const out: Policy = {}
  const trimmed = raw.trim()
  if (!trimmed) return out
  let parsed: unknown
  try {
    parsed = JSON.parse(trimmed)
  } catch {
    return out
  }
  if (!parsed || typeof parsed !== 'object') return out
  for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
    if (!v || typeof v !== 'object') continue
    const entry = v as Record<string, unknown>
    const enabled = entry.enabled === true
    let threshold = typeof entry.threshold === 'number' ? entry.threshold : 0
    if (threshold < 0) threshold = 0
    if (threshold > 1) threshold = 1
    out[k] = { enabled, threshold }
  }
  return out
}

function policyFor(p: Policy, key: string, defaultThreshold: number): ActionPolicy {
  return p[key] ?? { enabled: false, threshold: defaultThreshold }
}

function samePolicy(a: Policy, b: Policy): boolean {
  for (const { key, defaultThreshold } of ACTIONS) {
    const pa = policyFor(a, key, defaultThreshold)
    const pb = policyFor(b, key, defaultThreshold)
    if (pa.enabled !== pb.enabled) return false
    // Threshold only matters when enabled — an off action's slider value is moot.
    if (pa.enabled && pa.threshold !== pb.threshold) return false
  }
  return true
}

// AutonomyPanel lets the operator opt into the steerer acting WITHOUT asking for
// an action, above a per-action confidence threshold. Off (surface-only) is the
// default. It reads the current policy from the persisted FLOW_STEERING_AUTONOMY
// setting and saves the assembled JSON via the shared update-settings action.
export function AutonomyPanel() {
  const { data: settings } = useSettings()
  const action = useAction()

  const savedValue = useMemo(
    () => settings?.fields?.find((f) => f.key === AUTONOMY_KEY)?.value ?? '',
    [settings],
  )
  const savedPolicy = useMemo(() => parsePolicy(savedValue), [savedValue])

  // Local edits. null = "follow the saved value" (so a refresh after save
  // re-syncs the toggles/sliders from the server).
  const [draft, setDraft] = useState<Policy | null>(null)
  const current = draft ?? savedPolicy

  const dirty = useMemo(
    () => draft !== null && !samePolicy(draft, savedPolicy),
    [draft, savedPolicy],
  )

  const enabledCount = ACTIONS.reduce(
    (n, a) => n + (policyFor(current, a.key, a.defaultThreshold).enabled ? 1 : 0),
    0,
  )

  const setAction = (key: string, next: ActionPolicy) => {
    const base = draft ?? savedPolicy
    setDraft({ ...base, [key]: next })
  }

  const toggle = (key: string, defaultThreshold: number) => {
    const cur = policyFor(current, key, defaultThreshold)
    // Re-enabling seeds the default threshold if the stored one was never set.
    const threshold = cur.threshold > 0 ? cur.threshold : defaultThreshold
    setAction(key, { enabled: !cur.enabled, threshold })
  }

  const setThreshold = (key: string, defaultThreshold: number, value: number) => {
    const cur = policyFor(current, key, defaultThreshold)
    const clamped = Math.min(1, Math.max(0, value))
    setAction(key, { ...cur, threshold: clamped })
  }

  const save = () => {
    // Persist only the two managed actions, so unrelated keys can't leak in.
    const policy: Policy = {}
    for (const { key, defaultThreshold } of ACTIONS) {
      policy[key] = policyFor(current, key, defaultThreshold)
    }
    action.mutate(
      { kind: 'update-settings', settings: { [AUTONOMY_KEY]: JSON.stringify(policy) } },
      { onSuccess: () => setDraft(null) },
    )
  }

  return (
    <section className="settings-card">
      <div className="settings-card-head">
        <span>
          <Zap size={17} />
        </span>
        <h2>Autonomy</h2>
        <span className="spacer" />
        <span className="env-pill" title="actions the steerer may perform without asking">
          <span className={`dot ${enabledCount > 0 ? 'running' : 'idle'}`} />
          {enabledCount > 0 ? `${enabledCount} auto` : 'surface-only'}
        </span>
      </div>
      <div className="settings-card-body">
        <p className="config-help">
          When ON, the steerer acts automatically at or above the threshold — no click needed.
          Off = surface-only. Outward replies always stay manual.
        </p>

        <div className="autonomy-rows">
          {ACTIONS.map(({ key, label, defaultThreshold, risk, audit }) => {
            const pol = policyFor(current, key, defaultThreshold)
            const pct = Math.round(pol.threshold * 100)
            return (
              <div key={key} className={`autonomy-row${pol.enabled ? ' on' : ''}`}>
                <label className="autonomy-toggle" htmlFor={`autonomy-${key}`}>
                  <input
                    id={`autonomy-${key}`}
                    type="checkbox"
                    checked={pol.enabled}
                    onChange={() => toggle(key, defaultThreshold)}
                  />
                  <span className="autonomy-name">{label}</span>
                  <span className="autonomy-meta">
                    {risk} · {audit}
                  </span>
                </label>
                <input
                  className="autonomy-slider"
                  type="range"
                  min={0}
                  max={1}
                  step={0.05}
                  value={pol.threshold}
                  disabled={!pol.enabled}
                  onChange={(e) => setThreshold(key, defaultThreshold, Number(e.target.value))}
                  aria-label={`${label} confidence threshold`}
                />
                <span className={`autonomy-pct mono${pol.enabled ? '' : ' faint'}`}>
                  {pol.enabled ? `${pct}%` : 'off'}
                </span>
              </div>
            )
          })}
        </div>

        <div className="config-actions">
          <button
            type="button"
            className="btn primary sm"
            disabled={!dirty || action.isPending}
            onClick={save}
          >
            {action.isPending ? <Loader2 size={14} className="spin" /> : <Save size={14} />}
            Save autonomy
          </button>
        </div>
      </div>
    </section>
  )
}
