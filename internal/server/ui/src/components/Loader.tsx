// Brand loaders ported from flow_branding.html — the flow waveform "draws"
// itself (l-flow-dash) cycling indigo→indigo-hi, and a pulsing think-dots row.
// Both read --accent so they follow the active theme.

const WAVE = 'M9 23 Q14 23 14 18 Q14 13 19 13 Q24 13 24 18 Q24 23 27 23'

export function FlowLoader({ size = 38, label = 'loading' }: { size?: number; label?: string }) {
  return (
    <div className="flow-loader" role="status" aria-label={label}>
      <svg width={size} height={size} viewBox="0 0 36 36" fill="none">
        <path d={WAVE} stroke="var(--border-strong)" strokeWidth="2.6" strokeLinecap="round" />
        <path className="fl-dash" d={WAVE} stroke="var(--accent)" strokeWidth="2.6" strokeLinecap="round" />
        <circle cx="9" cy="23" r="1.8" fill="var(--accent)" />
        <circle cx="27" cy="23" r="1.8" fill="var(--accent-hi)" />
      </svg>
      {label && <span className="flow-loader-label">{label}</span>}
    </div>
  )
}

// Branch weave — the brand's git/branch-operation loader (flow_branding.html
// §5): two paths weave past each other. Used as the data-retrieval spinner.
export function BranchWeave({ size = 50, label = 'retrieving data' }: { size?: number; label?: string }) {
  return (
    <div className="flow-loader" role="status" aria-label={label}>
      <div className="l-weave" style={{ width: size, height: size }}>
        <svg viewBox="0 0 54 54" fill="none" aria-hidden>
          <path className="path p1" d="M5 18 Q14 18 18 27 Q22 36 31 36 Q40 36 49 36" />
          <path className="path p2" d="M5 36 Q14 36 18 27 Q22 18 31 18 Q40 18 49 18" />
        </svg>
      </div>
      {label && <span className="flow-loader-label">{label}</span>}
    </div>
  )
}

export function ThinkDots() {
  return (
    <span className="think" role="status" aria-label="working">
      <i />
      <i />
      <i />
    </span>
  )
}
