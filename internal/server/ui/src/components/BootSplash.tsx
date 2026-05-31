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
  return (
    <div className={`boot${leaving ? ' boot-leaving' : ''}`}>
      <div className="boot-glow" />
      <div className="boot-inner">
        <div className="boot-brand">
          <img src="/flow-mark.svg" width={34} height={34} alt="flow" className="boot-mark" />
          <span className="boot-word">
            flow<span className="accent">.</span>
          </span>
        </div>
        <div className="boot-status">
          <span className="dot running" />
          <span className="mono">booting mission control · reading ~/.flow/flow.db</span>
        </div>
        <div className="boot-bar">
          <i />
        </div>
      </div>
    </div>
  )
}
