import type { ReactNode } from 'react'
import { pct } from '../lib/format'
import { BranchWeave } from './Loader'

// ---- provider / source glyphs (served from /static brand svgs) -----------
export function ProviderIcon({ provider, size = 15 }: { provider?: string | null; size?: number }) {
  const src = provider === 'codex' ? '/codex-color.svg' : '/claudecode-color.svg'
  return (
    <img
      src={src}
      width={size}
      height={size}
      alt={provider ?? 'claude'}
      style={{ display: 'block', flex: 'none' }}
    />
  )
}

export function SourceIcon({ source, size = 14 }: { source?: string; size?: number }) {
  if (source === 'github')
    return <img className="gh-mark" src="/github-mark.svg" width={size} height={size} alt="github" />
  if (source === 'slack') return <img src="/slack-mark.svg" width={size} height={size} alt="slack" />
  return null
}

// ---- status -------------------------------------------------------------
const STATUS_CLASS: Record<string, string> = {
  running: 'running',
  waiting: 'waiting',
  stale: 'stale',
  dead: 'stale',
  released: 'done',
  idle: 'idle',
  done: 'done',
  backlog: 'idle',
  'in-progress': 'running',
}
export function StatusDot({ status }: { status: string }) {
  return <span className={`dot ${STATUS_CLASS[status] ?? 'idle'}`} />
}

export function StatusBadge({ status }: { status: string }) {
  const tone =
    status === 'running'
      ? 'ok'
      : status === 'waiting'
      ? 'warn'
      : status === 'stale'
      ? 'danger'
      : status === 'done'
      ? 'info'
      : ''
  return (
    <span className={`badge ${tone}`}>
      <StatusDot status={status} />
      {status}
    </span>
  )
}

// ---- sparkline ----------------------------------------------------------
export function Sparkline({ data, max, flex }: { data: number[]; max?: number; flex?: boolean }) {
  const peak = max ?? Math.max(1, ...data)
  return (
    <span className={`spark${flex ? ' flex' : ''}`} aria-hidden>
      {data.map((v, i) => (
        <i
          key={i}
          className={v > 0 ? 'on' : ''}
          style={{ height: `${Math.max(2, Math.round((v / peak) * 22))}px` }}
        />
      ))}
    </span>
  )
}

// ---- token meter --------------------------------------------------------
export function TokenBar({ used, max }: { used: number; max: number }) {
  const p = pct(used, max)
  const tone = p > 90 ? 'var(--danger)' : p > 70 ? 'var(--warn)' : 'var(--accent)'
  return (
    <div className="token-bar" title={`${used.toLocaleString()} / ${max.toLocaleString()} tokens`}>
      <div className="token-bar-fill" style={{ width: `${p}%`, background: tone }} />
    </div>
  )
}

// Compact circular context gauge — a donut showing how full the model's
// context window is, sized to sit inline next to the token/cost pill. Hover
// shows the full breakdown. Tone warms as the window fills (closer to compaction).
// Claude Code reserves ~16.5% of the window for the auto-compact buffer, so the
// USABLE context is ~83.5% of the total. We report % against that usable window
// — 100% ≈ where auto-compact fires — so the ring matches the terminal statusline
// (gsd-statusline normalizes the same way) and Claude's own "headroom" meter,
// rather than % of the raw window (which reads ~11 points low on a 1M session).
const USABLE_CONTEXT_FRACTION = 0.835

export function ContextRing({ used, max }: { used: number; max: number }) {
  const usable = Math.max(1, Math.round(max * USABLE_CONTEXT_FRACTION))
  const p = Math.min(100, pct(used, usable))
  const tone =
    p >= 90
      ? 'var(--danger)'
      : p >= 75
        ? 'var(--usage-high)'
        : p >= 50
          ? 'var(--usage-warn)'
          : 'var(--accent)'
  const r = 7
  const circ = 2 * Math.PI * r
  const dash = (p / 100) * circ
  return (
    <span
      className="tag ctx-ring"
      title={`Context: ${used.toLocaleString()} / ${max.toLocaleString()} tokens · ${p}% of usable (pre auto-compact) · ${Math.max(
        0,
        usable - used,
      ).toLocaleString()} left before auto-compact`}
    >
      <svg className="ctx-ring-svg" width="16" height="16" viewBox="0 0 18 18" aria-hidden="true">
        <circle cx="9" cy="9" r={r} fill="none" stroke="var(--border-strong)" strokeWidth="2.6" />
        <circle
          cx="9"
          cy="9"
          r={r}
          fill="none"
          stroke={tone}
          strokeWidth="2.6"
          strokeLinecap="round"
          strokeDasharray={`${dash} ${circ}`}
          transform="rotate(-90 9 9)"
        />
      </svg>
      <span className="ctx-ring-pct">{p}% ctx</span>
    </span>
  )
}

// ---- empty / loading / error -------------------------------------------
export function EmptyState({
  icon,
  title,
  hint,
  action,
}: {
  icon?: ReactNode
  title: string
  hint?: string
  action?: ReactNode
}) {
  return (
    <div className="empty">
      {icon && <div className="empty-icon">{icon}</div>}
      <div className="empty-title">{title}</div>
      {hint && <div className="empty-hint">{hint}</div>}
      {action && <div style={{ marginTop: 14 }}>{action}</div>}
    </div>
  )
}

export function Loading({ label }: { rows?: number; label?: string }) {
  return <BranchWeave label={label} />
}

export function SkeletonRows({ rows = 4 }: { rows?: number }) {
  return (
    <div className="col" style={{ gap: 10, padding: '4px 0' }}>
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="skeleton" style={{ height: 58, opacity: 1 - i * 0.12 }} />
      ))}
    </div>
  )
}

export function ErrorNote({ error }: { error: unknown }) {
  const msg = error instanceof Error ? error.message : String(error)
  return (
    <div className="error-note">
      <span className="mono">error</span> {msg}
    </div>
  )
}

export function Field({
  label,
  children,
  hint,
  className,
}: {
  label: string
  children: ReactNode
  hint?: string
  className?: string
}) {
  return (
    <label className={`field${className ? ` ${className}` : ''}`}>
      <span className="field-label">{label}</span>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </label>
  )
}

export function Stat({ label, value, tone }: { label: string; value: ReactNode; tone?: string }) {
  return (
    <div className="stat">
      <div className="stat-value num" style={tone ? { color: tone } : undefined}>
        {value}
      </div>
      <div className="stat-label eyebrow">{label}</div>
    </div>
  )
}
