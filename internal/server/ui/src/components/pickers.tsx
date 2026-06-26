import { Lock, Zap, Ban } from 'lucide-react'
import { ProviderIcon } from './ui'

const PERMS = [
  { v: 'default', label: 'default', Icon: Lock, title: 'Permissions: default · prompt to run' },
  { v: 'auto', label: 'auto', Icon: Zap, title: 'Permissions: auto · accept edits' },
  { v: 'bypass', label: 'bypass', Icon: Ban, title: 'Permissions: bypass · skip all prompts' },
]

// Universal permission selector — lock / zap / ban, used in both the new-task
// form and the session toolbar so the control reads the same everywhere.
export function PermissionPicker({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="segmented perm-seg">
      {PERMS.map(({ v, label, Icon, title }) => (
        <button key={v} type="button" className={value === v ? 'active' : ''} title={title} onClick={() => onChange(v)}>
          <Icon size={14} /> {label}
        </button>
      ))}
    </div>
  )
}

// Concrete model ids per provider, passed straight to `claude --model` /
// `codex --model` — mirrors flowdb.ModelForTier. "Auto" (empty value) leaves the
// task with no explicit pin, so flow resolves a tier at launch (with
// auto-downshift). Largest → smallest. Backlog-locked like the agent picker: a
// started session never switches models mid-life.
const MODELS: Record<string, { v: string; label: string; title: string }[]> = {
  claude: [
    { v: '', label: 'Auto', title: 'Resolve at launch — tier default with auto-downshift on descriptive briefs' },
    { v: 'opus', label: 'Opus', title: 'claude --model opus · largest' },
    { v: 'sonnet', label: 'Sonnet', title: 'claude --model sonnet · medium' },
    { v: 'haiku', label: 'Haiku', title: 'claude --model haiku · smallest' },
  ],
  codex: [
    { v: '', label: 'Auto', title: 'Resolve at launch — tier default with auto-downshift on descriptive briefs' },
    { v: 'gpt-5.5', label: 'gpt-5.5', title: 'codex --model gpt-5.5 · largest' },
    { v: 'gpt-5.4', label: 'gpt-5.4', title: 'codex --model gpt-5.4 · medium' },
    { v: 'gpt-5.4-mini', label: 'gpt-5.4-mini', title: 'codex --model gpt-5.4-mini · smallest' },
  ],
}

// Session-model selector, scoped to the chosen provider. A model the operator
// pinned via the CLI that isn't in the menu is appended so it still shows.
export function ModelPicker({
  provider,
  value,
  onChange,
}: {
  provider: string
  value: string
  onChange: (v: string) => void
}) {
  const base = MODELS[provider] ?? MODELS.claude
  const opts = value && !base.some((o) => o.v === value)
    ? [...base, { v: value, label: value, title: `--model ${value}` }]
    : base
  return (
    <select
      className="input model-select"
      value={value || ''}
      onChange={(e) => onChange(e.target.value)}
      title="Session model — set before the session starts"
      aria-label="Session model"
    >
      {opts.map((o) => (
        <option key={o.v || 'auto'} value={o.v} title={o.title}>
          {o.v ? o.label : 'Model: Auto'}
        </option>
      ))}
    </select>
  )
}

const EFFORTS: Record<string, { v: string; label: string; title: string }[]> = {
  claude: [
    { v: '', label: 'Auto', title: 'Provider default; xhigh when model resolves to Opus' },
    { v: 'low', label: 'low', title: 'claude --effort low' },
    { v: 'medium', label: 'medium', title: 'claude --effort medium' },
    { v: 'high', label: 'high', title: 'claude --effort high' },
    { v: 'xhigh', label: 'xhigh', title: 'claude --effort xhigh' },
    { v: 'max', label: 'max', title: 'claude --effort max' },
  ],
  codex: [
    { v: '', label: 'Auto', title: 'Provider default; xhigh when model resolves to gpt-5.5' },
    { v: 'minimal', label: 'minimal', title: 'model_reasoning_effort=minimal' },
    { v: 'low', label: 'low', title: 'model_reasoning_effort=low' },
    { v: 'medium', label: 'medium', title: 'model_reasoning_effort=medium' },
    { v: 'high', label: 'high', title: 'model_reasoning_effort=high' },
    { v: 'xhigh', label: 'xhigh', title: 'model_reasoning_effort=xhigh' },
  ],
}

export function EffortPicker({
  provider,
  value,
  onChange,
}: {
  provider: string
  value: string
  onChange: (v: string) => void
}) {
  const opts = EFFORTS[provider] ?? EFFORTS.claude
  return (
    <select
      className="input model-select effort-select"
      value={value || ''}
      onChange={(e) => onChange(e.target.value)}
      title="Reasoning effort — set before the session starts"
      aria-label="Reasoning effort"
    >
      {opts.map((o) => (
        <option key={o.v || 'auto'} value={o.v} title={o.title}>
          {o.v ? o.label : 'Effort: Auto'}
        </option>
      ))}
    </select>
  )
}

const PRIOS = [
  { v: 'low', label: 'low' },
  { v: 'medium', label: 'medium' },
  { v: 'high', label: 'high' },
]

// Priority selector — low / medium / high as text segments.
export function PriorityPicker({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="segmented prio-seg">
      {PRIOS.map(({ v, label }) => (
        <button key={v} type="button" className={value === v ? 'active' : ''} onClick={() => onChange(v)}>
          <span className={`prio ${v}`} /> {label}
        </button>
      ))}
    </div>
  )
}

// Agent selector as a segmented switch (Claude / Codex) — one connected control
// that reads as a toggle, matching the permission segmented control beside it.
// The active provider's segment is accent-filled. Unavailable providers (binary
// not on PATH) render greyed-out and unclickable with the reason as a tooltip.
export function AgentPicker({
  value,
  onChange,
  providers,
}: {
  value: string
  onChange: (v: string) => void
  providers: { id: string; label: string; available?: boolean; reason?: string }[]
}) {
  const list = providers.length ? providers : [{ id: 'claude', label: 'Claude Code', available: true }]
  return (
    <div className="segmented agent-seg" role="radiogroup" aria-label="Agent">
      {list.map((p) => {
        const disabled = p.available === false
        const active = value === p.id
        return (
          <button
            key={p.id}
            type="button"
            role="radio"
            aria-checked={active}
            aria-label={p.label}
            disabled={disabled}
            className={active ? 'active' : ''}
            title={disabled ? p.reason || `${p.label} is not installed` : p.label}
            onClick={() => !disabled && onChange(p.id)}
          >
            <ProviderIcon provider={p.id} size={18} />
          </button>
        )
      })}
    </div>
  )
}
