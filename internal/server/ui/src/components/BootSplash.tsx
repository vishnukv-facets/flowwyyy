import { useEffect, useState, type ReactNode } from 'react'
import { useUiData } from '../lib/query'

// Full-screen boot splash shown on hard refresh while the WS connects and
// the first ui-data payload loads. Children mount underneath immediately
// (so data starts loading); the splash overlays until ready, then fades.
export function BootGate({ children }: { children: ReactNode }) {
  const { isSuccess, isError } = useUiData()
  const [minElapsed, setMinElapsed] = useState(false)
  const [gone, setGone] = useState(false)

  useEffect(() => {
    const t = setTimeout(() => setMinElapsed(true), 650)
    return () => clearTimeout(t)
  }, [])

  const ready = (isSuccess || isError) && minElapsed

  useEffect(() => {
    if (!ready) return
    const t = setTimeout(() => setGone(true), 480)
    return () => clearTimeout(t)
  }, [ready])

  return (
    <>
      {children}
      {!gone && <BootSplash leaving={ready} />}
    </>
  )
}

function BootSplash({ leaving }: { leaving: boolean }) {
  // Determinate-feeling progress: trickle toward ~92% (fast at first, slowing
  // asymptotically so it never stalls), then snap to 100% once data is ready
  // (leaving). We can't know the true % — the wait is the server building the
  // first ui-data snapshot — so this communicates "working + nearly there"
  // honestly without faking a measured percentage.
  const [progress, setProgress] = useState(8)
  useEffect(() => {
    if (leaving) {
      setProgress(100)
      return
    }
    const id = setInterval(() => {
      setProgress((p) => (p >= 92 ? p : p + Math.max(0.4, (92 - p) * 0.06)))
    }, 180)
    return () => clearInterval(id)
  }, [leaving])

  return (
    <div className={`boot${leaving ? ' boot-leaving' : ''}`}>
      <div className="boot-glow" />
      <div className="boot-inner">
        <div className="boot-brand">
          <img src="/flow-mark.svg" width={34} height={34} alt="flowwyyy" className="boot-mark" />
          <span className="boot-word">
            flowwyyy<span className="accent">.</span>
          </span>
        </div>
        <div className="boot-status">
          <span className="dot running" />
          <span className="mono">booting mission control · reading ~/.flow/flow.db</span>
        </div>
        <div
          className="boot-bar"
          role="progressbar"
          aria-label="Loading Mission Control"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(progress)}
        >
          <i style={{ width: `${progress}%` }} />
        </div>
      </div>
    </div>
  )
}
