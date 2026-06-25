// A custom, theme-aware floating tooltip — replaces the browser's native
// `title` bubble (which renders as an un-themed white OS popup). Portaled to
// <body> so it never clips inside a card's overflow, positioned against the
// hovered element's bounding box: it flips below when too close to the top,
// and is clamped horizontally to the viewport (with the caret kept pointing at
// the anchor) so edge cells don't push it off-screen.
import { useCallback, useEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

type TipState = { x: number; y: number; caret: number; below: boolean; content: ReactNode } | null

const HALF = 150 // ~ half the tooltip max-width, used for viewport clamping
const MARGIN = 10

export function useFloatTip() {
  const [tip, setTip] = useState<TipState>(null)
  const tipRef = useRef<HTMLDivElement | null>(null)

  const show = useCallback((el: HTMLElement, content: ReactNode) => {
    const r = el.getBoundingClientRect()
    const below = r.top < 150 // not enough room above → drop the tip beneath
    const anchor = r.left + r.width / 2
    const x = Math.max(HALF + MARGIN, Math.min(anchor, window.innerWidth - HALF - MARGIN))
    // Keep the caret over the real anchor even when the box was clamped inward,
    // but don't let it slide past the box's rounded corners.
    const caret = Math.max(-(HALF - 16), Math.min(anchor - x, HALF - 16))
    setTip({ x, y: below ? r.bottom : r.top, caret, below, content })
  }, [])

  // Anchor at an explicit viewport point (px, py) rather than an element's
  // centre — used by the line/area charts to track the hovered data point.
  const showAt = useCallback((px: number, py: number, content: ReactNode) => {
    const below = py < 150
    const x = Math.max(HALF + MARGIN, Math.min(px, window.innerWidth - HALF - MARGIN))
    const caret = Math.max(-(HALF - 16), Math.min(px - x, HALF - 16))
    setTip({ x, y: py, caret, below, content })
  }, [])

  const hide = useCallback(() => setTip(null), [])

  useEffect(() => {
    if (!tip) return
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target
      if (target instanceof Node && tipRef.current?.contains(target)) return
      setTip(null)
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setTip(null)
    }
    document.addEventListener('pointerdown', onPointerDown, true)
    document.addEventListener('keydown', onKeyDown, true)
    return () => {
      document.removeEventListener('pointerdown', onPointerDown, true)
      document.removeEventListener('keydown', onKeyDown, true)
    }
  }, [tip])

  const portal = tip
    ? createPortal(
        <div
          ref={tipRef}
          className={`float-tip${tip.below ? ' below' : ''}`}
          style={{ left: tip.x, top: tip.y, '--caret-dx': `${tip.caret}px` } as CSSProperties}
          role="tooltip"
        >
          {tip.content}
        </div>,
        document.body,
      )
    : null

  return { show, showAt, hide, portal }
}
