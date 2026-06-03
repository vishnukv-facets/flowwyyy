import { useEffect, useId, useRef } from 'react'
import gsap from 'gsap'

// The flow waveform mark — an arch (∩) over two "foot" dots on a purple tile.
// Same geometry as flow-mark.svg / Loader.tsx's WAVE, but rendered inline so
// GSAP can target the path and dots individually and play a randomly-looping
// set of small "expressions". Technique adapted from Claude.ai's SVG+GSAP
// mascot (Codrops, 2026): each expression is a gsap.timeline() of eased
// sub-tweens; the loop picks a non-repeating expression, plays it once, then
// rests for a beat before the next — so the mark is calm with occasional life.

const WAVE = 'M9 23 Q14 23 14 18 Q14 13 19 13 Q24 13 24 18 Q24 23 27 23'
// Structurally identical to WAVE (same M/Q command layout, only numbers differ)
// so GSAP's AttrPlugin can interpolate the `d` string directly — a free "morph"
// with no paid MorphSVGPlugin. The crest lifts from y=13 to y=11; the feet stay
// anchored at y=23 so the separate dot circles never drift off the path ends.
const WAVE_LIFT = 'M9 23 Q14 23 14 17 Q14 11 19 11 Q24 11 24 17 Q24 23 27 23'

interface Parts {
  art: SVGGElement
  wave: SVGPathElement
  dot1: SVGCircleElement
  dot2: SVGCircleElement
}

type Expr = (p: Parts) => gsap.core.Timeline

// flow — the signature one: the crest breathes up and down with a faint sway,
// like water moving through the mark.
const flow: Expr = ({ art, wave }) =>
  gsap
    .timeline()
    .to(wave, { attr: { d: WAVE_LIFT }, duration: 0.9, ease: 'sine.inOut' }, 0)
    .to(art, { skewX: 2.5, x: -0.6, svgOrigin: '18 18', duration: 0.9, ease: 'sine.inOut' }, 0)
    .to(wave, { attr: { d: WAVE }, duration: 0.9, ease: 'sine.inOut' }, 0.9)
    .to(art, { skewX: -2.5, x: 0.6, svgOrigin: '18 18', duration: 0.9, ease: 'sine.inOut' }, 0.9)
    .to(art, { skewX: 0, x: 0, svgOrigin: '18 18', duration: 0.6, ease: 'sine.inOut' }, 1.8)

// pulse — a two-beat heartbeat from the tile centre.
const pulse: Expr = ({ art }) =>
  gsap
    .timeline({ defaults: { svgOrigin: '18 18' } })
    .to(art, { scale: 1.13, duration: 0.14, ease: 'power2.out' })
    .to(art, { scale: 0.98, duration: 0.16, ease: 'power2.inOut' })
    .to(art, { scale: 1.07, duration: 0.12, ease: 'power2.out' })
    .to(art, { scale: 1, duration: 0.5, ease: 'elastic.out(1, 0.5)' })

// draw — erase the wave and redraw it left→right (strokeDashoffset), with the
// dots popping in at each foot as the line reaches them. Like a signal plotting.
const draw: Expr = ({ wave, dot1, dot2 }) => {
  const len = wave.getTotalLength()
  return gsap
    .timeline()
    .set(wave, { strokeDasharray: len, strokeDashoffset: len })
    .set([dot1, dot2], { scale: 0, transformOrigin: '50% 50%' })
    .to(wave, { strokeDashoffset: 0, duration: 0.85, ease: 'power2.inOut' })
    .to(dot1, { scale: 1, duration: 0.25, ease: 'back.out(2.4)' }, 0.05)
    .to(dot2, { scale: 1, duration: 0.25, ease: 'back.out(2.4)' }, 0.7)
    .set(wave, { strokeDasharray: 'none' })
}

// hop — the two feet hop up and bounce back, staggered, then the mark squashes
// on the landing. A little walk-in-place.
const hop: Expr = ({ art, dot1, dot2 }) =>
  gsap
    .timeline()
    .to(dot1, { y: -3.5, duration: 0.22, ease: 'power2.out' }, 0)
    .to(dot1, { y: 0, duration: 0.4, ease: 'bounce.out' }, 0.22)
    .to(dot2, { y: -3.5, duration: 0.22, ease: 'power2.out' }, 0.16)
    .to(dot2, { y: 0, duration: 0.4, ease: 'bounce.out' }, 0.38)
    .to(art, { scaleY: 0.92, scaleX: 1.05, svgOrigin: '18 24', duration: 0.12, ease: 'sine.inOut' }, 0.52)
    .to(art, { scaleY: 1, scaleX: 1, svgOrigin: '18 24', duration: 0.34, ease: 'elastic.out(1, 0.6)' }, 0.64)

// tilt — a curious head-tilt that settles with an elastic wobble.
const tilt: Expr = ({ art }) =>
  gsap
    .timeline({ defaults: { svgOrigin: '18 19' } })
    .to(art, { rotation: 9, duration: 0.35, ease: 'back.out(2)' })
    .to(art, { rotation: -7, duration: 0.4, ease: 'sine.inOut' })
    .to(art, { rotation: 0, duration: 0.5, ease: 'elastic.out(1, 0.5)' })

// breathe — the quiet idle: a barely-there swell. Used as connective tissue.
const breathe: Expr = ({ art }) =>
  gsap
    .timeline({ defaults: { svgOrigin: '18 18' } })
    .to(art, { scale: 1.04, duration: 0.7, ease: 'sine.inOut' })
    .to(art, { scale: 1, duration: 0.9, ease: 'sine.inOut' })

// flow is weighted x2 — it's the signature motion and reads as the brand.
const POOL: Expr[] = [flow, flow, pulse, draw, hop, tilt, breathe]

export function FlowMark({
  size = 23,
  className,
  animated = true,
}: {
  size?: number
  className?: string
  animated?: boolean
}) {
  const svgRef = useRef<SVGSVGElement>(null)
  const artRef = useRef<SVGGElement>(null)
  const waveRef = useRef<SVGPathElement>(null)
  const dot1Ref = useRef<SVGCircleElement>(null)
  const dot2Ref = useRef<SVGCircleElement>(null)
  // Namespace the gradient id per instance — multiple marks on one page would
  // otherwise share `id="flow-mark-gradient"` (invalid, can drop the fill).
  const gid = 'fm-grad-' + useId().replace(/:/g, '')

  useEffect(() => {
    if (!animated) return
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return
    const art = artRef.current
    const wave = waveRef.current
    const dot1 = dot1Ref.current
    const dot2 = dot2Ref.current
    if (!art || !wave || !dot1 || !dot2) return
    const parts: Parts = { art, wave, dot1, dot2 }

    let tl: gsap.core.Timeline | null = null
    let wait: gsap.core.Tween | null = null
    let last: Expr | null = null
    let dead = false
    const rand = (a: number, b: number) => a + Math.random() * (b - a)

    const playNext = () => {
      if (dead) return
      let next = POOL[Math.floor(Math.random() * POOL.length)]
      for (let i = 0; next === last && i < 5; i++) next = POOL[Math.floor(Math.random() * POOL.length)]
      last = next
      tl = next(parts)
      tl.eventCallback('onComplete', () => {
        if (!dead) wait = gsap.delayedCall(rand(1.1, 2.8), playNext)
      })
    }

    // Let the app paint before the first expression fires.
    wait = gsap.delayedCall(rand(0.6, 1.2), playNext)

    return () => {
      dead = true
      wait?.kill()
      tl?.kill()
      gsap.set([art, wave, dot1, dot2], { clearProps: 'all' })
      gsap.set(wave, { attr: { d: WAVE } })
    }
  }, [animated])

  return (
    <svg
      ref={svgRef}
      width={size}
      height={size}
      viewBox="0 0 36 36"
      className={className}
      aria-hidden="true"
      focusable="false"
    >
      <defs>
        <linearGradient id={gid} x1="0" y1="0" x2="36" y2="36" gradientUnits="userSpaceOnUse">
          <stop offset="0" stopColor="#645df6" />
          <stop offset="1" stopColor="#8b87f8" />
        </linearGradient>
      </defs>
      <rect width="36" height="36" rx="8" fill={`url(#${gid})`} />
      <g ref={artRef}>
        <path ref={waveRef} d={WAVE} fill="none" stroke="#fff" strokeWidth="2.6" strokeLinecap="round" />
        <circle ref={dot1Ref} cx="9" cy="23" r="2" fill="#fff" />
        <circle ref={dot2Ref} cx="27" cy="23" r="2" fill="#fff" />
      </g>
    </svg>
  )
}
