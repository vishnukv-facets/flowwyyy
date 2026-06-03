import { useEffect, useId, useRef, type RefObject } from 'react'
import gsap from 'gsap'
import { napMs } from '../lib/mascot'

// The Claude Code mascot — the front-facing pixel-art critter from the Codrops
// "Reverse-engineering Claude AI's mascot animations" article, recreated from
// sharp <rect>s and animated purely via transforms.
//   ClaudeMascot     — standalone (hops/waves)
//   ClaudeFlowScene  — meets the flow wave logo (stomp / flag / gym)
//   ClaudeRunner     — sidebar critter: scurries a line, naps, reacts, pokeable

const WAVE = 'M9 23 Q14 23 14 18 Q14 13 19 13 Q24 13 24 18 Q24 23 27 23'
const WAVE_LIFT = 'M9 23 Q14 23 14 17 Q14 11 19 11 Q24 11 24 17 Q24 23 27 23'

const CORAL = '#bf6a4e'
const EYE = '#1c140f'
const IRON = '#3a2a24' // dumbbells + flag pole
const HEART = '#e0607e'

// Confetti, in figure-local coords above the head; 0-3 burst left, 4-7 right.
const CONF = [
  { x: 14, y: 6, dx: -15, dy: -16, rot: 140, fill: CORAL },
  { x: 20, y: 2, dx: -9, dy: -22, rot: -120, fill: '#645df6' },
  { x: 26, y: 5, dx: -4, dy: -19, rot: 200, fill: '#b9b5fb' },
  { x: 30, y: 1, dx: -1, dy: -24, rot: 90, fill: '#ffffff' },
  { x: 34, y: 1, dx: 1, dy: -24, rot: -60, fill: CORAL },
  { x: 38, y: 5, dx: 4, dy: -19, rot: 160, fill: '#645df6' },
  { x: 44, y: 2, dx: 9, dy: -22, rot: -150, fill: '#b9b5fb' },
  { x: 50, y: 6, dx: 15, dy: -16, rot: 120, fill: '#ffffff' },
]

interface ClaudeRefs {
  group: RefObject<SVGGElement>
  breath: RefObject<SVGGElement>
  eyes: RefObject<SVGGElement>
  legs: RefObject<SVGGElement>
  nubL: RefObject<SVGGElement>
  nubR: RefObject<SVGGElement>
  flag: RefObject<SVGGElement>
  dumbbellL: RefObject<SVGGElement>
  dumbbellR: RefObject<SVGGElement>
  conf: RefObject<SVGGElement>
  mug: RefObject<SVGGElement>
  book: RefObject<SVGGElement>
  heart: RefObject<SVGGElement>
  board: RefObject<SVGGElement>
  surf: RefObject<SVGGElement>
  dream: RefObject<SVGGElement>
  laptop: RefObject<SVGGElement>
}

function useClaudeRefs(): ClaudeRefs {
  return {
    group: useRef<SVGGElement>(null),
    breath: useRef<SVGGElement>(null),
    eyes: useRef<SVGGElement>(null),
    legs: useRef<SVGGElement>(null),
    nubL: useRef<SVGGElement>(null),
    nubR: useRef<SVGGElement>(null),
    flag: useRef<SVGGElement>(null),
    dumbbellL: useRef<SVGGElement>(null),
    dumbbellR: useRef<SVGGElement>(null),
    conf: useRef<SVGGElement>(null),
    mug: useRef<SVGGElement>(null),
    book: useRef<SVGGElement>(null),
    heart: useRef<SVGGElement>(null),
    board: useRef<SVGGElement>(null),
    surf: useRef<SVGGElement>(null),
    dream: useRef<SVGGElement>(null),
    laptop: useRef<SVGGElement>(null),
  }
}

// Front-facing, symmetric about x=32, flat top. Each arm is a group so it can
// hold a prop; props are display:none when idle (kept out of the arm's bbox so
// rotation pivots stay stable).
function ClaudeFigure({ r, withProps = false }: { r: ClaudeRefs; withProps?: boolean }) {
  return (
    <g ref={r.group}>
      <g ref={r.breath}>
        <g ref={r.legs}>
          <rect x="14" y="36" width="6" height="8" fill={CORAL} />
          <rect x="24" y="36" width="6" height="8" fill={CORAL} />
          <rect x="34" y="36" width="6" height="8" fill={CORAL} />
          <rect x="44" y="36" width="6" height="8" fill={CORAL} />
        </g>
        {/* body — one flat-topped block */}
        <rect x="11" y="8" width="42" height="28" fill={CORAL} />
        {/* left arm group */}
        <g ref={r.nubL}>
          <rect x="6" y="16" width="5" height="7" fill={CORAL} />
          {withProps && (
            <g ref={r.dumbbellL} style={{ display: 'none' }}>
              <rect x="5" y="18" width="7" height="2" fill={IRON} />
              <rect x="4" y="16" width="2.5" height="6" fill={IRON} />
              <rect x="10.5" y="16" width="2.5" height="6" fill={IRON} />
            </g>
          )}
        </g>
        {/* right arm group (holds flag / dumbbell / mug) */}
        <g ref={r.nubR}>
          <rect x="53" y="16" width="5" height="7" fill={CORAL} />
          {withProps && (
            <>
              <g ref={r.flag} style={{ display: 'none' }}>
                <rect x="56" y="4" width="2" height="14" fill={IRON} />
                <rect x="58" y="4" width="11" height="7" fill="#fff" />
                <rect x="58" y="4" width="3.6" height="3.5" fill={EYE} />
                <rect x="65.2" y="4" width="3.6" height="3.5" fill={EYE} />
                <rect x="61.6" y="7.5" width="3.6" height="3.5" fill={EYE} />
              </g>
              <g ref={r.dumbbellR} style={{ display: 'none' }}>
                <rect x="52" y="18" width="7" height="2" fill={IRON} />
                <rect x="51" y="16" width="2.5" height="6" fill={IRON} />
                <rect x="57.5" y="16" width="2.5" height="6" fill={IRON} />
              </g>
              <g ref={r.mug} style={{ display: 'none' }}>
                <rect x="54" y="15" width="7" height="7" rx="1" fill="#e8e6df" />
                <rect x="54" y="15" width="7" height="2" fill="#6b4a2f" />
                <rect x="61" y="16.5" width="2.4" height="3.4" rx="1" fill="none" stroke="#e8e6df" strokeWidth="1.3" />
              </g>
            </>
          )}
        </g>
        {/* eyes — wide, near the walls */}
        <g ref={r.eyes}>
          <rect x="15" y="18" width="6" height="6" fill={EYE} />
          <rect x="43" y="18" width="6" height="6" fill={EYE} />
        </g>
        {withProps && (
          <>
            {/* confetti */}
            <g ref={r.conf}>
              {CONF.map((p, i) => (
                <rect key={i} x={p.x} y={p.y} width="3.2" height="3.2" fill={p.fill} opacity="0" />
              ))}
            </g>
            {/* book (reading) — held in front */}
            <g ref={r.book} style={{ display: 'none' }}>
              <rect x="22" y="25" width="9" height="7" fill="#f0eee6" />
              <rect x="33" y="25" width="9" height="7" fill="#f0eee6" />
              <rect x="31" y="24" width="2" height="9" fill="#9a5a3f" />
              <rect x="24" y="27" width="5" height="1" fill="#b9b3a6" />
              <rect x="35" y="27" width="5" height="1" fill="#b9b3a6" />
              <rect x="24" y="29.5" width="5" height="1" fill="#b9b3a6" />
              <rect x="35" y="29.5" width="5" height="1" fill="#b9b3a6" />
            </g>
            {/* heart (pet) — pops above the head */}
            <g ref={r.heart} style={{ display: 'none' }}>
              <rect x="28" y="-5" width="3" height="3" fill={HEART} />
              <rect x="33" y="-5" width="3" height="3" fill={HEART} />
              <rect x="27" y="-2" width="10" height="3" fill={HEART} />
              <rect x="29" y="1" width="6" height="2" fill={HEART} />
              <rect x="31" y="3" width="2" height="2" fill={HEART} />
            </g>
            {/* skateboard (ride) — under the feet */}
            <g ref={r.board} style={{ display: 'none' }}>
              <rect x="11" y="44" width="42" height="2.6" rx="1.3" fill="#c08348" />
              <rect x="17" y="46.4" width="5" height="3.4" rx="1.5" fill="#8a5836" />
              <rect x="42" y="46.4" width="5" height="3.4" rx="1.5" fill="#8a5836" />
            </g>
            {/* surfboard (surf) — under the feet while riding the wave. The wave
                itself is a separate rail-layer element; this is just the board the
                rider stands on. Its upturned nose flips with travel direction via
                the group's scaleX. */}
            <g ref={r.surf} style={{ display: 'none' }}>
              <rect x="12" y="42" width="40" height="3.4" rx="1.7" fill="#e8e6df" />
              <rect x="12" y="43.4" width="40" height="1" rx="0.5" fill={CORAL} />
              <rect x="48" y="40.4" width="6" height="3" rx="1.5" fill="#e8e6df" transform="rotate(-20 51 41.9)" />
            </g>
            {/* dream bubble (asleep) */}
            <g ref={r.dream} style={{ display: 'none' }}>
              <rect x="38" y="-11" width="15" height="10" rx="3.5" fill="#ffffff" opacity="0.92" />
              <circle cx="36.5" cy="-1.5" r="1.5" fill="#ffffff" opacity="0.92" />
              {/* dreaming of a chocolate-chip cookie */}
              <circle cx="45.5" cy="-6" r="4" fill="#cf9d5b" />
              <circle cx="44" cy="-7.2" r="0.8" fill="#5b3a1e" />
              <circle cx="47.1" cy="-5.4" r="0.8" fill="#5b3a1e" />
              <circle cx="44.5" cy="-4.6" r="0.7" fill="#5b3a1e" />
              <circle cx="46.7" cy="-7.5" r="0.6" fill="#5b3a1e" />
            </g>
            {/* laptop (working) — a clamshell seen from the side: a foreshortened
                keyboard slab with the screen hinged up and tilted back, glowing
                code toward us. The angle is what reads as "laptop". Drawn last →
                frontmost. The lid's tilt lives on its own group's rotate attribute
                so the GSAP-driven hands/lines never clobber it. */}
            <g ref={r.laptop} style={{ display: 'none' }}>
              {/* keyboard base — a slab foreshortened toward the viewer */}
              <polygon points="26,36.4 47,38.6 44.6,42 23.6,39.6" fill="#aeb3bd" />
              <polygon points="26,36.4 47,38.6 46.4,39.4 25.6,37.2" fill="#d6dae1" />
              <polygon points="28.6,38 41,39.5 40.6,40.2 28.2,38.7" fill="#8f95a0" />
              {/* screen lid — hinged at the back, leaning up and away */}
              <g transform="rotate(-20 26 37)">
                <rect x="26" y="20.5" width="18" height="16.5" rx="1.6" fill="#3a3f49" />
                <rect x="27.6" y="22" width="14.8" height="12.6" rx="0.8" fill="#0d2840" />
                <rect data-code="1" x="29.2" y="23.6" width="9" height="1.5" rx="0.4" fill="#7aa2f7" />
                <rect data-code="1" x="29.2" y="26.1" width="11.4" height="1.5" rx="0.4" fill="#56d364" />
                <rect data-code="1" x="29.2" y="28.6" width="6.4" height="1.5" rx="0.4" fill="#e3a45e" />
                <rect data-code="1" x="29.2" y="31.1" width="9.8" height="1.5" rx="0.4" fill="#9aa5b1" />
              </g>
              {/* hands on the keyboard front (axis-aligned so GSAP can tap them) */}
              <rect data-hand="1" x="29" y="36.4" width="6" height="3" rx="1.4" fill={CORAL} />
              <rect data-hand="1" x="37.5" y="37.6" width="6" height="3" rx="1.4" fill={CORAL} />
            </g>
          </>
        )}
      </g>
    </g>
  )
}

const prefersReduced = () =>
  typeof window !== 'undefined' && window.matchMedia('(prefers-reduced-motion: reduce)').matches

// One hop: legs crouch then push, body arcs up and lands; xTo = absolute group x.
function hop(tl: gsap.core.Timeline, t: number, xTo: number, legs: Element[], cg: Element) {
  tl.to(legs, { scaleY: 0.62, transformOrigin: '50% 100%', duration: 0.12, ease: 'power2.in' }, t)
    .to(legs, { scaleY: 1.06, duration: 0.16, ease: 'power2.out' }, t + 0.12)
    .to(cg, { y: -13, duration: 0.2, ease: 'sine.out' }, t + 0.14)
    .to(cg, { x: xTo, duration: 0.38, ease: 'sine.inOut' }, t + 0.14)
    .to(cg, { y: 0, duration: 0.22, ease: 'power2.in' }, t + 0.34)
    .to(legs, { scaleY: 0.8, duration: 0.1, ease: 'power2.in' }, t + 0.54)
    .to(legs, { scaleY: 1, duration: 0.18, ease: 'elastic.out(1, 0.5)' }, t + 0.64)
}

// ---------------------------------------------------------------------------
// Standalone mascot — hops in place, wiggles its arms, breathes, blinks.
// ---------------------------------------------------------------------------
export function ClaudeMascot({ size = 88, className }: { size?: number; className?: string }) {
  const r = useClaudeRefs()

  useEffect(() => {
    if (prefersReduced()) return
    const group = r.group.current, breath = r.breath.current, eyes = r.eyes.current
    const legsG = r.legs.current, nubL = r.nubL.current, nubR = r.nubR.current
    if (!group || !breath || !eyes || !legsG || !nubL || !nubR) return
    const legs = Array.from(legsG.children)

    const breathe = gsap.to(breath, { scaleY: 1.03, transformOrigin: '50% 100%', duration: 1.3, ease: 'sine.inOut', yoyo: true, repeat: -1 })
    const blink = gsap.timeline({ repeat: -1, repeatDelay: 2.7 })
    blink.to(eyes, { scaleY: 0.1, transformOrigin: '50% 50%', duration: 0.07 }).to(eyes, { scaleY: 1, duration: 0.07 })

    const tl = gsap.timeline({ repeat: -1, repeatDelay: 0.7 })
    hop(tl, 0, 0, legs, group)
    hop(tl, 0.95, 0, legs, group)
    tl.to(nubL, { rotation: -28, transformOrigin: '100% 50%', duration: 0.18, ease: 'sine.inOut', yoyo: true, repeat: 3 }, 1.95)
      .to(nubR, { rotation: 28, transformOrigin: '0% 50%', duration: 0.18, ease: 'sine.inOut', yoyo: true, repeat: 3 }, 1.95)
      .to(eyes, { scaleY: 0.2, transformOrigin: '50% 50%', duration: 0.1, yoyo: true, repeat: 1 }, 2.3)

    return () => {
      ;[breathe, blink, tl].forEach((t) => t.kill())
      gsap.set([group, breath, eyes, nubL, nubR, ...legs], { clearProps: 'all' })
    }
  }, [])

  return (
    <svg width={size} height={Math.round((size * 50) / 64)} viewBox="0 0 64 50" className={className} aria-hidden="true" focusable="false">
      <ClaudeFigure r={r} />
    </svg>
  )
}

type VariantKey = 'stomp' | 'flag' | 'gym'

// ---------------------------------------------------------------------------
// The interaction — Claude walks over to the flow logo and (per loop, at
// random) does the confetti stomp, raises a victory flag, or curls dumbbells,
// while the wave reacts. `only` forces one variant (validation seam).
// ---------------------------------------------------------------------------
export function ClaudeFlowScene({ width = 248, className, only }: { width?: number; className?: string; only?: VariantKey }) {
  const c = useClaudeRefs()
  const tileRef = useRef<SVGGElement>(null)
  const artRef = useRef<SVGGElement>(null)
  const waveRef = useRef<SVGPathElement>(null)
  const dot1Ref = useRef<SVGCircleElement>(null)
  const dot2Ref = useRef<SVGCircleElement>(null)
  const gid = 'cfs-grad-' + useId().replace(/:/g, '')

  useEffect(() => {
    if (prefersReduced()) return
    const cg = c.group.current, breath = c.breath.current, eyes = c.eyes.current
    const legsG = c.legs.current, nubL = c.nubL.current, nubR = c.nubR.current
    const flag = c.flag.current, dbL = c.dumbbellL.current, dbR = c.dumbbellR.current, confG = c.conf.current
    const tile = tileRef.current, art = artRef.current, wave = waveRef.current
    const dot1 = dot1Ref.current, dot2 = dot2Ref.current
    if (!cg || !breath || !eyes || !legsG || !nubL || !nubR || !flag || !dbL || !dbR || !confG || !tile || !art || !wave || !dot1 || !dot2) return
    const legs = Array.from(legsG.children)
    const conf = Array.from(confG.children)
    const rand = (a: number, b: number) => a + Math.random() * (b - a)

    const breathe = gsap.to(breath, { scaleY: 1.03, transformOrigin: '50% 100%', duration: 1.3, ease: 'sine.inOut', yoyo: true, repeat: -1 })
    const blink = gsap.timeline({ repeat: -1, repeatDelay: 3.0 })
    blink.to(eyes, { scaleY: 0.1, transformOrigin: '50% 50%', duration: 0.07 }).to(eyes, { scaleY: 1, duration: 0.07 })

    const burst = (tl: gsap.core.Timeline, at: number, idxs: number[]) =>
      tl.fromTo(idxs.map((i) => conf[i]),
        { opacity: 1, x: 0, y: 0, scale: 0.4, rotation: 0 },
        { x: (j: number) => CONF[idxs[j]].dx, y: (j: number) => CONF[idxs[j]].dy, scale: 1, rotation: (j: number) => CONF[idxs[j]].rot, opacity: 0, transformOrigin: '50% 50%', duration: 0.7, ease: 'power2.out', stagger: 0.03 },
        at)

    const wavePulse = (tl: gsap.core.Timeline, at: number) =>
      tl.to(wave, { attr: { d: WAVE_LIFT }, duration: 0.12, ease: 'power2.out' }, at)
        .to(wave, { attr: { d: WAVE }, duration: 0.34, ease: 'elastic.out(1, 0.5)' }, at + 0.12)
        .to(dot1, { y: -2.5, duration: 0.12 }, at).to(dot1, { y: 0, duration: 0.34, ease: 'bounce.out' }, at + 0.12)

    const waveCheer = (tl: gsap.core.Timeline, at: number) =>
      tl.to(wave, { attr: { d: WAVE_LIFT }, duration: 0.4, ease: 'sine.inOut' }, at)
        .to(wave, { attr: { d: WAVE }, duration: 0.4, ease: 'sine.inOut' }, at + 0.4)
        .to(dot1, { y: -2.5, duration: 0.18, yoyo: true, repeat: 1, ease: 'sine.inOut' }, at + 0.1)
        .to(dot2, { y: -2.5, duration: 0.18, yoyo: true, repeat: 1, ease: 'sine.inOut' }, at + 0.22)

    const walkIn = (tl: gsap.core.Timeline) => {
      tl.to(eyes, { x: -2.2, transformOrigin: '50% 50%', duration: 0.4, ease: 'power2.out' }, 0)
        .to(cg, { rotation: 4, transformOrigin: '50% 100%', duration: 0.32, ease: 'sine.inOut' }, 0)
        .to(cg, { rotation: -4, transformOrigin: '50% 100%', duration: 0.36, ease: 'sine.inOut' }, 0.32)
        .to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.22, ease: 'sine.inOut' }, 0.68)
        .to(art, { scale: 1.05, transformOrigin: '50% 55%', duration: 0.6, ease: 'sine.out' }, 0.2)
      hop(tl, 0.95, -26, legs, cg)
    }
    const walkBack = (tl: gsap.core.Timeline, t: number) => {
      tl.to(eyes, { x: 0, transformOrigin: '50% 50%', duration: 0.4, ease: 'power2.inOut' }, t)
        .to(art, { scale: 1, rotation: 0, transformOrigin: '50% 55%', duration: 0.5, ease: 'sine.inOut' }, t)
      hop(tl, t + 0.1, 0, legs, cg)
    }

    const vStomp = (tl: gsap.core.Timeline, t: number) => {
      const beat = (bt: number, left: boolean) => {
        tl.to(cg, { rotation: left ? 5 : -5, transformOrigin: '50% 100%', duration: 0.16, ease: 'sine.inOut' }, bt)
          .to(legs, { scaleY: 0.84, transformOrigin: '50% 100%', duration: 0.1, ease: 'power2.in' }, bt)
          .to(legs, { scaleY: 1, duration: 0.16, ease: 'power2.out' }, bt + 0.1)
        const arm = left ? nubL : nubR
        tl.to(arm, { y: -5, rotation: left ? -42 : 42, transformOrigin: left ? '100% 100%' : '0% 100%', duration: 0.16, ease: 'power2.out' }, bt)
          .to(arm, { y: 0, rotation: 0, transformOrigin: left ? '100% 100%' : '0% 100%', duration: 0.2, ease: 'power2.in' }, bt + 0.26)
        burst(tl, bt + 0.06, left ? [0, 1, 2, 3] : [4, 5, 6, 7])
        wavePulse(tl, bt + 0.08)
      }
      beat(t, true); beat(t + 0.42, false); beat(t + 0.84, true); beat(t + 1.26, false)
      tl.to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.22, ease: 'sine.out' }, t + 1.62)
      return t + 1.86
    }
    const vFlag = (tl: gsap.core.Timeline, t: number) => {
      tl.set(flag, { display: 'inline' }, t)
        .to(nubR, { y: -4, transformOrigin: '0% 100%', duration: 0.3, ease: 'back.out(1.8)' }, t)
        .fromTo(flag, { scaleY: 0 }, { scaleY: 1, transformOrigin: '50% 100%', duration: 0.4, ease: 'back.out(2)' }, t + 0.08)
        .to(cg, { rotation: -4, transformOrigin: '50% 100%', duration: 0.34, ease: 'sine.inOut', yoyo: true, repeat: 3 }, t + 0.5)
        .to(flag, { rotation: 10, transformOrigin: '50% 100%', duration: 0.34, ease: 'sine.inOut', yoyo: true, repeat: 3 }, t + 0.5)
        .to(eyes, { scaleY: 0.3, transformOrigin: '50% 50%', duration: 0.12, yoyo: true, repeat: 1 }, t + 0.6)
      waveCheer(tl, t + 0.5)
      tl.to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.2, ease: 'sine.out' }, t + 1.55)
        .to(flag, { scaleY: 0, transformOrigin: '50% 100%', duration: 0.3, ease: 'power2.in' }, t + 1.6)
        .to(nubR, { y: 0, transformOrigin: '0% 100%', duration: 0.3, ease: 'power2.inOut' }, t + 1.65)
        .set(flag, { display: 'none', rotation: 0 }, t + 1.98)
      return t + 1.98
    }
    const vGym = (tl: gsap.core.Timeline, t: number) => {
      tl.set([dbL, dbR], { display: 'inline' }, t)
      const curl = (arm: Element, bt: number) => {
        tl.to(arm, { y: -8, duration: 0.22, ease: 'power2.out' }, bt).to(arm, { y: 0, duration: 0.3, ease: 'power2.in' }, bt + 0.22)
        wavePulse(tl, bt + 0.05)
      }
      curl(nubL, t); curl(nubR, t + 0.26); curl(nubL, t + 0.52); curl(nubR, t + 0.78); curl(nubL, t + 1.04); curl(nubR, t + 1.3)
      const end = t + 1.62
      tl.set([dbL, dbR], { display: 'none' }, end + 0.05).set([nubL, nubR], { y: 0 }, end + 0.05)
      return end + 0.1
    }

    const variants: Record<VariantKey, (tl: gsap.core.Timeline, t: number) => number> = { stomp: vStomp, flag: vFlag, gym: vGym }
    const order: VariantKey[] = ['stomp', 'flag', 'gym']
    let last = -1
    let current: gsap.core.Timeline | null = null
    let wait: gsap.core.Tween | null = null
    let dead = false

    const playNext = () => {
      if (dead) return
      const tl = gsap.timeline()
      walkIn(tl)
      let key: VariantKey
      if (only && variants[only]) key = only
      else { let idx = Math.floor(Math.random() * order.length); if (idx === last) idx = (idx + 1) % order.length; last = idx; key = order[idx] }
      const end = variants[key](tl, 1.85)
      walkBack(tl, Math.max(end + 0.1, 3.6))
      tl.eventCallback('onComplete', () => { if (!dead) wait = gsap.delayedCall(rand(0.5, 1.2), playNext) })
      current = tl
    }
    wait = gsap.delayedCall(rand(0.5, 1.0), playNext)

    return () => {
      dead = true
      breathe.kill(); blink.kill(); wait?.kill(); current?.kill()
      gsap.set([cg, breath, eyes, nubL, nubR, flag, dbL, dbR, art, wave, dot1, dot2, tile, ...legs, ...conf], { clearProps: 'all' })
      gsap.set(wave, { attr: { d: WAVE } })
      gsap.set([flag, dbL, dbR], { display: 'none' })
      gsap.set(conf, { opacity: 0 })
    }
  }, [only])

  return (
    <svg width={width} height={Math.round((width * 96) / 170)} viewBox="0 0 170 96" className={className} aria-hidden="true" focusable="false">
      <defs>
        <linearGradient id={gid} x1="28" y1="32" x2="64" y2="68" gradientUnits="userSpaceOnUse">
          <stop offset="0" stopColor="#645df6" />
          <stop offset="1" stopColor="#8b87f8" />
        </linearGradient>
      </defs>
      <g ref={tileRef} transform="translate(28 32)">
        <rect width="36" height="36" rx="8" fill={`url(#${gid})`} />
        <g ref={artRef}>
          <path ref={waveRef} d={WAVE} fill="none" stroke="#fff" strokeWidth="2.6" strokeLinecap="round" />
          <circle ref={dot1Ref} cx="9" cy="23" r="2" fill="#fff" />
          <circle ref={dot2Ref} cx="27" cy="23" r="2" fill="#fff" />
        </g>
      </g>
      <g transform="translate(88 22)">
        <ClaudeFigure r={c} withProps />
      </g>
    </svg>
  )
}

// ---------------------------------------------------------------------------
// Sidebar runner — a tiny Claude that mirrors your presence. While you're around
// it scurries the line and (mostly) sits at its laptop and works; when you go
// idle it nods off, and it wakes the moment you run a session. At night it's
// asleep by default — nudge it (poke ×4) and it'll grumble awake to work for a
// bit before dozing off again. Poke while awake = pet/tickle; drag it along.
// ---------------------------------------------------------------------------
const RUNNER_W = 42
const WAVE_W = 58 // swell graphic width (px) — wider than the mascot so it can ride it
const WAVE_H = 15 // swell crest height (px) — a touch under the mascot's ~24px body
const WAKE_NUDGES = 4 // pokes needed to anger it awake
const NUDGE_WINDOW_MS = 2500 // stop poking this long → it settles back to deep sleep
const NIGHT_AWAKE_MS = 24000 // once woken at night, works this long (sans new sessions) then sleeps

export function ClaudeRunner({ conn, running, monitored, inbox }: { conn?: string; running?: number; monitored?: number; inbox?: number }) {
  const r = useClaudeRefs()
  const wrapRef = useRef<HTMLDivElement>(null)
  const svgRef = useRef<SVGSVGElement>(null)
  const waveRef = useRef<SVGSVGElement>(null)
  const browsRef = useRef<SVGGElement>(null)
  const zzzRef = useRef<SVGGElement>(null)
  const downRef = useRef<(e: { clientX: number }) => void>(() => {})
  const reactRef = useRef<{
    session?: () => void
    inbox?: () => void
    disconnect?: () => void
    reconnect?: () => void
  }>({})
  const prevConn = useRef(conn)
  const prevRun = useRef(running)
  const prevMon = useRef(monitored)
  const prevInbox = useRef(inbox)

  useEffect(() => {
    const wrap = wrapRef.current, svg = svgRef.current, waveEl = waveRef.current, brows = browsRef.current, zzzG = zzzRef.current
    const cg = r.group.current, breath = r.breath.current, eyes = r.eyes.current
    const legsG = r.legs.current, nubL = r.nubL.current, nubR = r.nubR.current
    const flag = r.flag.current, dbL = r.dumbbellL.current, dbR = r.dumbbellR.current, confG = r.conf.current
    const mug = r.mug.current, book = r.book.current, heart = r.heart.current, board = r.board.current, dream = r.dream.current
    const laptop = r.laptop.current, surfRig = r.surf.current
    if (!wrap || !svg || !waveEl || !brows || !zzzG || !cg || !breath || !eyes || !legsG || !nubL || !nubR || !flag || !dbL || !dbR || !confG || !mug || !book || !heart || !board || !dream || !laptop || !surfRig) return
    const legs = Array.from(legsG.children)
    const conf = Array.from(confG.children)
    const zzz = Array.from(zzzG.children)
    const spray = Array.from(waveEl.querySelectorAll('[data-spray]'))
    if (prefersReduced()) { gsap.set(svg, { x: Math.max(0, (wrap.offsetWidth - RUNNER_W) / 2) }); return }
    const rand = (a: number, b: number) => a + Math.random() * (b - a)
    const maxX = () => Math.max(16, wrap.offsetWidth - RUNNER_W)
    const centerX = () => Math.max(0, (wrap.offsetWidth - RUNNER_W) / 2)
    const curX = () => Number(gsap.getProperty(svg, 'x')) || 0
    const farEnd = () => (curX() > maxX() / 2 ? 0 : maxX()) // the end we're not near
    const isNight = () => { const h = new Date().getHours(); return h >= 21 || h < 6 }

    const breathe = gsap.to(breath, { scaleY: 1.03, transformOrigin: '50% 100%', duration: 1.2, ease: 'sine.inOut', yoyo: true, repeat: -1 })
    const blink = gsap.timeline({ repeat: -1, repeatDelay: 2.6 })
    blink.to(eyes, { scaleY: 0.1, transformOrigin: '50% 50%', duration: 0.07 }).to(eyes, { scaleY: 1, duration: 0.07 })

    const burst = (tl: gsap.core.Timeline, at: number, idxs: number[]) =>
      tl.fromTo(idxs.map((i) => conf[i]),
        { opacity: 1, x: 0, y: 0, scale: 0.4, rotation: 0 },
        { x: (j: number) => CONF[idxs[j]].dx, y: (j: number) => CONF[idxs[j]].dy, scale: 1, rotation: (j: number) => CONF[idxs[j]].rot, opacity: 0, transformOrigin: '50% 50%', duration: 0.7, ease: 'power2.out', stagger: 0.03 }, at)

    const scurry = (tl: gsap.core.Timeline, t: number, toX: number) => {
      tl.to(svg, { x: toX, duration: 1.5, ease: 'power1.inOut' }, t)
        .to(cg, { y: -2.4, duration: 0.15, ease: 'sine.inOut', yoyo: true, repeat: 9 }, t)
        .to(legs, { rotation: (i: number) => (i % 2 ? 18 : -18), transformOrigin: '50% 0%', duration: 0.13, ease: 'sine.inOut', yoyo: true, repeat: 11 }, t)
        .set(legs, { rotation: 0 }, t + 1.5).set(cg, { y: 0 }, t + 1.5)
      return t + 1.5
    }

    // skateboard ride — alt locomotion: board out, lean into the roll, legs still
    const ride = (tl: gsap.core.Timeline, t: number, toX: number) => {
      const cur = Number(gsap.getProperty(svg, 'x')) || 0
      const lean = toX > cur ? 7 : -7
      tl.set(board, { display: 'inline' }, t)
        .to(cg, { y: -6, transformOrigin: '50% 100%', duration: 0.18, ease: 'power2.out' }, t)
        .to(cg, { rotation: lean, transformOrigin: '50% 100%', duration: 0.25, ease: 'sine.out' }, t)
        .to(svg, { x: toX, duration: 1.1, ease: 'power1.inOut' }, t + 0.1)
        .to(cg, { rotation: lean * 0.45, transformOrigin: '50% 100%', duration: 0.5, ease: 'sine.inOut', yoyo: true, repeat: 1 }, t + 0.3)
        .to(cg, { rotation: 0, y: 0, transformOrigin: '50% 100%', duration: 0.25, ease: 'power2.inOut' }, t + 1.25)
        .set(board, { display: 'none' }, t + 1.55)
      return t + 1.55
    }

    // surf set-piece — every so often a swell rolls in from a corner, Claude is
    // startled, then hops onto a surfboard and rides ABOVE the crest to the far
    // corner, where the wave breaks and recedes off-screen. The wave is its own
    // full-rail element animated in the same px space as the figure's svg.x, so
    // the two stay locked: the rider sits on the crest the whole way. `dir` (the
    // travel direction) flips the breaking curl, so it reads the same whether the
    // wave comes from the left corner or the right.
    const SURF_OFFSET = (WAVE_W - RUNNER_W) / 2 // centres the rider on the crest peak
    const SURF_LIFT = WAVE_H - 3                 // how far up the figure rides to sit on the crest
    const surfWave = (tl: gsap.core.Timeline) => {
      const railW = wrap.offsetWidth
      const cur = curX()
      const fromLeft = cur < maxX() / 2          // the swell enters from the nearer corner…
      const dir = fromLeft ? 1 : -1              // …and carries Claude to the far one
      const entryWaveX = fromLeft ? -WAVE_W : railW
      const exitWaveX = fromLeft ? railW : -WAVE_W
      const farX = fromLeft ? maxX() : 0
      const mountWaveX = cur - SURF_OFFSET       // wave peak meets Claude where he stands
      const ENTER = 0.8, RIDE = 1.7

      // --- swell rolls in from the corner ---
      tl.set(waveEl, { x: entryWaveX, scaleX: dir, opacity: 0, transformOrigin: '50% 100%' }, 0)
        .to(waveEl, { opacity: 1, duration: 0.25 }, 0)
        .to(waveEl, { x: mountWaveX, duration: ENTER, ease: 'power1.out' }, 0)
        // Claude clocks it coming — glance toward the incoming side
        .to(eyes, { x: dir * 2.4, transformOrigin: '50% 50%', duration: 0.3, ease: 'sine.out' }, 0.15)
        .to(cg, { rotation: dir * 4, transformOrigin: '50% 100%', duration: 0.3, ease: 'sine.out' }, 0.2)
        // SURPRISE as it arrives — eyes pop, arms fly up, a startled recoil hop
        .to(eyes, { scaleY: 1.5, scaleX: 1.15, x: 0, transformOrigin: '50% 50%', duration: 0.12, ease: 'power2.out' }, ENTER - 0.2)
        .to(nubL, { rotation: -60, y: -6, transformOrigin: '100% 100%', duration: 0.16, ease: 'back.out(2)' }, ENTER - 0.2)
        .to(nubR, { rotation: 60, y: -6, transformOrigin: '0% 100%', duration: 0.16, ease: 'back.out(2)' }, ENTER - 0.2)
        .to(cg, { y: -7, rotation: -dir * 6, transformOrigin: '50% 100%', duration: 0.16, ease: 'power2.out' }, ENTER - 0.2)
        .to(legs, { scaleY: 0.82, transformOrigin: '50% 100%', duration: 0.12 }, ENTER - 0.2)

      // --- mount: board out, climb on, ride lifts him up onto the crest ---
      const mt = ENTER
      tl.set(surfRig, { display: 'inline', scaleX: dir, opacity: 1, transformOrigin: '50% 50%' }, mt)
        .to(legs, { scaleY: 1, rotation: 0, transformOrigin: '50% 100%', duration: 0.18 }, mt)
        .to(svg, { y: -SURF_LIFT, duration: 0.3, ease: 'power2.out' }, mt)        // up onto the crest
        .to(cg, { y: 0, rotation: dir * 8, transformOrigin: '50% 100%', duration: 0.3, ease: 'power2.out' }, mt)
        .to(nubL, { rotation: -22, y: -2, transformOrigin: '100% 100%', duration: 0.26, ease: 'sine.out' }, mt)
        .to(nubR, { rotation: 22, y: -2, transformOrigin: '0% 100%', duration: 0.26, ease: 'sine.out' }, mt)
        .to(eyes, { scaleY: 1, scaleX: 1, transformOrigin: '50% 50%', duration: 0.25 }, mt + 0.1)

      // --- ride: wave + rider sweep to the far corner together, locked in step ---
      const rd = mt + 0.2
      tl.to(waveEl, { x: farX - SURF_OFFSET, duration: RIDE, ease: 'sine.inOut' }, rd)
        .to(svg, { x: farX, duration: RIDE, ease: 'sine.inOut' }, rd)
        // bob along the crest + carve the lean
        .to(svg, { y: -SURF_LIFT - 3, duration: 0.42, ease: 'sine.inOut', yoyo: true, repeat: 2 }, rd + 0.1)
        .to(cg, { rotation: dir * 4, transformOrigin: '50% 100%', duration: 0.5, ease: 'sine.inOut', yoyo: true, repeat: 1 }, rd + 0.2)
        .to(eyes, { scaleY: 0.6, transformOrigin: '50% 50%', duration: 0.18, yoyo: true, repeat: 1 }, rd + 0.7)
      // spray flicks off the crest (local coords; the wave's scaleX mirrors the side)
      spray.forEach((s, i) =>
        tl.fromTo(s,
          { opacity: 0, x: 0, y: 0, scale: 0.5 },
          { opacity: 0.9, x: -(4 + i * 3), y: -(4 + i * 2), scale: 1, transformOrigin: '50% 50%', duration: 0.4, ease: 'power2.out', yoyo: true, repeat: 4 },
          rd + 0.2 + i * 0.06))

      // --- dismount at the corner: hop down, board away, wave breaks & recedes ---
      const ds = rd + RIDE
      tl.to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.2 }, ds)
        .to([nubL, nubR], { rotation: 0, y: 0, duration: 0.22, ease: 'power2.inOut' }, ds)
        .to(svg, { y: 0, duration: 0.32, ease: 'bounce.out' }, ds)                // hop back down to the line
        .to(spray, { opacity: 0, duration: 0.15 }, ds)
        .to(surfRig, { opacity: 0, duration: 0.18, ease: 'power2.in' }, ds + 0.1)
        .set(surfRig, { display: 'none', scaleX: 1, clearProps: 'transform,opacity' }, ds + 0.34)
        .set(spray, { clearProps: 'transform,opacity' }, ds + 0.34)
        .to(waveEl, { x: exitWaveX, duration: 0.6, ease: 'power1.in' }, ds)        // wave rolls on past the corner
        .to(waveEl, { opacity: 0, duration: 0.4 }, ds + 0.3)
        .set(waveEl, { display: 'inline', scaleX: 1, opacity: 0, x: 0, clearProps: 'transform' }, ds + 0.75)
        .to(eyes, { scaleY: 1, transformOrigin: '50% 50%', duration: 0.2 }, ds + 0.3)
      return ds + 0.9
    }

    // ---- activities (figure-only) ----
    const aWave = (tl: gsap.core.Timeline, t: number) => {
      tl.to(nubR, { rotation: 30, transformOrigin: '0% 50%', duration: 0.2, ease: 'sine.inOut', yoyo: true, repeat: 5 }, t)
        .to(cg, { rotation: -2, transformOrigin: '50% 100%', duration: 0.25, ease: 'sine.out' }, t)
        .to(eyes, { scaleY: 0.2, transformOrigin: '50% 50%', duration: 0.1, yoyo: true, repeat: 1 }, t + 0.5)
        .to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.3, ease: 'sine.inOut' }, t + 1.05)
      return t + 1.4
    }
    const aGym = (tl: gsap.core.Timeline, t: number) => {
      tl.set([dbL, dbR], { display: 'inline' }, t)
      const curl = (arm: Element, bt: number) => { tl.to(arm, { y: -8, duration: 0.22, ease: 'power2.out' }, bt).to(arm, { y: 0, duration: 0.3, ease: 'power2.in' }, bt + 0.22) }
      curl(nubL, t); curl(nubR, t + 0.26); curl(nubL, t + 0.52); curl(nubR, t + 0.78)
      tl.set([dbL, dbR], { display: 'none' }, t + 1.2).set([nubL, nubR], { y: 0 }, t + 1.2)
      return t + 1.3
    }
    const aFlag = (tl: gsap.core.Timeline, t: number) => {
      tl.set(flag, { display: 'inline' }, t)
        .to(nubR, { y: -4, transformOrigin: '0% 100%', duration: 0.3, ease: 'back.out(1.8)' }, t)
        .fromTo(flag, { scaleY: 0 }, { scaleY: 1, transformOrigin: '50% 100%', duration: 0.4, ease: 'back.out(2)' }, t + 0.08)
        .to(cg, { rotation: -4, transformOrigin: '50% 100%', duration: 0.34, ease: 'sine.inOut', yoyo: true, repeat: 3 }, t + 0.5)
        .to(flag, { rotation: 10, transformOrigin: '50% 100%', duration: 0.34, ease: 'sine.inOut', yoyo: true, repeat: 3 }, t + 0.5)
        .to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.2 }, t + 1.5)
        .to(flag, { scaleY: 0, transformOrigin: '50% 100%', duration: 0.3, ease: 'power2.in' }, t + 1.55)
        .to(nubR, { y: 0, transformOrigin: '0% 100%', duration: 0.3 }, t + 1.6)
        .set(flag, { display: 'none', rotation: 0 }, t + 1.95)
      return t + 1.95
    }
    const aStomp = (tl: gsap.core.Timeline, t: number) => {
      const beat = (bt: number, left: boolean) => {
        tl.to(cg, { rotation: left ? 5 : -5, transformOrigin: '50% 100%', duration: 0.16, ease: 'sine.inOut' }, bt)
          .to(legs, { scaleY: 0.84, transformOrigin: '50% 100%', duration: 0.1 }, bt).to(legs, { scaleY: 1, duration: 0.16 }, bt + 0.1)
        const arm = left ? nubL : nubR
        tl.to(arm, { y: -5, rotation: left ? -42 : 42, transformOrigin: left ? '100% 100%' : '0% 100%', duration: 0.16, ease: 'power2.out' }, bt)
          .to(arm, { y: 0, rotation: 0, transformOrigin: left ? '100% 100%' : '0% 100%', duration: 0.2 }, bt + 0.26)
        burst(tl, bt + 0.06, left ? [0, 1, 2, 3] : [4, 5, 6, 7])
      }
      beat(t, true); beat(t + 0.42, false); beat(t + 0.84, true); beat(t + 1.26, false)
      tl.to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.22 }, t + 1.62)
      return t + 1.86
    }
    const aLook = (tl: gsap.core.Timeline, t: number) => {
      tl.to(eyes, { x: -2.6, transformOrigin: '50% 50%', duration: 0.25, ease: 'sine.inOut' }, t)
        .to(cg, { rotation: -5, transformOrigin: '50% 100%', duration: 0.25, ease: 'sine.inOut' }, t)
        .to(eyes, { x: 2.6, duration: 0.35, ease: 'sine.inOut' }, t + 0.5)
        .to(cg, { rotation: 5, transformOrigin: '50% 100%', duration: 0.35, ease: 'sine.inOut' }, t + 0.5)
        .to(eyes, { x: 0, duration: 0.25, ease: 'sine.inOut' }, t + 1.0)
        .to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.25, ease: 'sine.inOut' }, t + 1.0)
      return t + 1.35
    }
    // sit on the ledge — scoot the whole figure DOWN so the body's base rests on
    // the divider line and the legs fall BELOW it (rendered via the rail's open
    // bottom clip), then idly kick the dangling feet. The hips now sit right on
    // the line, so each leg swings from the ledge edge — a little 3D perch.
    const aSit = (tl: gsap.core.Timeline, t: number) => {
      const kickA = [legs[0], legs[2]], kickB = [legs[1], legs[3]] // alternate pairs out of phase
      tl.to(cg, { y: 8, transformOrigin: '50% 100%', duration: 0.34, ease: 'power2.out' }, t) // drop down onto the ledge
        .to(eyes, { y: 1.2, scaleY: 0.85, transformOrigin: '50% 50%', duration: 0.3 }, t)       // peer down over the edge
        .to(kickA, { rotation: 11, transformOrigin: '50% 0%', duration: 0.55, ease: 'sine.inOut', yoyo: true, repeat: 3 }, t + 0.36)
        .to(kickB, { rotation: -11, transformOrigin: '50% 0%', duration: 0.55, ease: 'sine.inOut', yoyo: true, repeat: 3 }, t + 0.64)
        // climb back up onto the rail and stand
        .to(legs, { rotation: 0, transformOrigin: '50% 0%', duration: 0.3, ease: 'power2.inOut' }, t + 2.2)
        .to(eyes, { y: 0, scaleY: 1, transformOrigin: '50% 50%', duration: 0.25 }, t + 2.2)
        .to(cg, { y: 0, transformOrigin: '50% 100%', duration: 0.32, ease: 'power2.inOut' }, t + 2.25)
      return t + 2.7
    }
    const aYawn = (tl: gsap.core.Timeline, t: number) => {
      tl.to(nubL, { rotation: -55, y: -4, transformOrigin: '100% 100%', duration: 0.4, ease: 'power2.out' }, t)
        .to(nubR, { rotation: 55, y: -4, transformOrigin: '0% 100%', duration: 0.4, ease: 'power2.out' }, t)
        .to(cg, { scaleY: 1.12, transformOrigin: '50% 100%', duration: 0.4, ease: 'power2.out' }, t)
        .to(eyes, { scaleY: 0.15, transformOrigin: '50% 50%', duration: 0.3, ease: 'sine.in' }, t + 0.2)
        .to(eyes, { scaleY: 1.2, transformOrigin: '50% 50%', duration: 0.25, ease: 'power2.out' }, t + 0.7)
        .to([nubL, nubR], { rotation: 0, y: 0, duration: 0.4, ease: 'power2.inOut' }, t + 1.0)
        .to(cg, { scaleY: 1, transformOrigin: '50% 100%', duration: 0.4, ease: 'power2.inOut' }, t + 1.0)
        .to(eyes, { scaleY: 1, transformOrigin: '50% 50%', duration: 0.3 }, t + 1.0)
      return t + 1.5
    }
    const aCoffee = (tl: gsap.core.Timeline, t: number) => {
      tl.set(mug, { display: 'inline' }, t)
        // raise the mug up and across to the mouth (it lives out at the right hand)
        .to(nubR, { x: -21, y: 2, rotation: -10, transformOrigin: '0% 100%', duration: 0.4, ease: 'power2.out' }, t)
        .to(mug, { rotation: -16, transformOrigin: '50% 100%', duration: 0.4, ease: 'power2.out' }, t)
        // the sip — head tips back, the cup tilts right up, eyes lift toward the
        // rim and narrow happily (looking up, not drooping down)
        .to(cg, { rotation: 6, transformOrigin: '50% 100%', duration: 0.32, ease: 'sine.inOut' }, t + 0.45)
        .to(mug, { rotation: -46, transformOrigin: '50% 100%', duration: 0.32, ease: 'sine.inOut' }, t + 0.45)
        .to(eyes, { y: -1.6, scaleY: 0.62, transformOrigin: '50% 50%', duration: 0.28, ease: 'sine.inOut' }, t + 0.5)
        // back down
        .to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.36, ease: 'sine.inOut' }, t + 1.15)
        .to(mug, { rotation: -12, transformOrigin: '50% 100%', duration: 0.36, ease: 'sine.inOut' }, t + 1.15)
        .to(eyes, { y: 0, scaleY: 1, transformOrigin: '50% 50%', duration: 0.3, ease: 'sine.inOut' }, t + 1.15)
        // satisfied "ahh" bob
        .to(cg, { y: -2, transformOrigin: '50% 100%', duration: 0.16, yoyo: true, repeat: 1, ease: 'sine.inOut' }, t + 1.55)
        // lower the mug and tuck it away
        .to(nubR, { x: 0, y: 0, rotation: 0, transformOrigin: '0% 100%', duration: 0.36, ease: 'power2.inOut' }, t + 1.8)
        .to(mug, { rotation: 0, transformOrigin: '50% 100%', duration: 0.36, ease: 'power2.inOut' }, t + 1.8)
        .set(mug, { display: 'none' }, t + 2.25)
      return t + 2.35
    }
    const aRead = (tl: gsap.core.Timeline, t: number) => {
      tl.set(book, { display: 'inline' }, t)
        .to(eyes, { y: 1.6, scaleY: 0.7, transformOrigin: '50% 50%', duration: 0.3 }, t)
        .to(cg, { rotation: 2, transformOrigin: '50% 100%', duration: 0.4, ease: 'sine.inOut', yoyo: true, repeat: 3 }, t + 0.4)
        .to(eyes, { y: 0, scaleY: 1, transformOrigin: '50% 50%', duration: 0.3 }, t + 2.0)
        .set(book, { display: 'none' }, t + 2.3)
      return t + 2.4
    }
    // a real 3-ball cascade: balls start held at the hands, then get thrown in a
    // staggered rhythm — each arcs up to a peak at centre and lands in the other
    // hand (x sweeps hand-to-hand, y parabolas with an apex each crossing). The
    // confetti rects double as the balls (rounded via rx), repositioned onto the
    // hands so it plays in front of the body, not off to the side of the head.
    const aJuggle = (tl: gsap.core.Timeline, t: number) => {
      const balls = [conf[0], conf[1], conf[2]]
      // arc band sits above the head (Hy low-catch, Ay apex) so the balls never
      // rest on the face; arms raise to meet them.
      const Lx = 17, Rx = 47, Hy = 3, Ay = -15, SEG = 0.4, loops = 4, gap = 0.26
      tl.set(balls, { opacity: 1, scale: 1.5, rotation: 0, attr: { rx: 1.6, ry: 1.6 } }, t)
      balls.forEach((b, i) => {
        const bx = CONF[i].x, by = CONF[i].y
        const leftTX = Lx - bx, rightTX = Rx - bx
        const handTY = Hy - by, apexTY = Ay - by
        const startRight = i % 2 === 0
        const startTX = startRight ? rightTX : leftTX
        const otherTX = startRight ? leftTX : rightTX
        const t0 = t + i * gap
        tl.set(b, { x: startTX, y: handTY }, t)                                   // held in hand until thrown
          .to(b, { x: otherTX, duration: SEG, ease: 'none', yoyo: true, repeat: loops - 1 }, t0)
          .fromTo(b, { y: handTY }, { y: apexTY, duration: SEG / 2, ease: 'power2.out', yoyo: true, repeat: loops * 2 - 1 }, t0)
      })
      // arms reach up and bob in the throw/catch rhythm
      tl.fromTo(nubL, { rotation: -22, y: -3 }, { rotation: -38, y: -6, transformOrigin: '100% 100%', duration: SEG, ease: 'sine.inOut', yoyo: true, repeat: loops + 1 }, t)
        .fromTo(nubR, { rotation: 22, y: -3 }, { rotation: 38, y: -6, transformOrigin: '0% 100%', duration: SEG, ease: 'sine.inOut', yoyo: true, repeat: loops + 1 }, t)
      const end = t + 2 * gap + SEG * loops
      tl.to(balls, { opacity: 0, duration: 0.22 }, end)
        .set(balls, { clearProps: 'x,y,scale,rotation', attr: { rx: 0, ry: 0 } }, end + 0.22)
        .set([nubL, nubR], { rotation: 0, y: 0 }, end + 0.22)
      return end + 0.45
    }
    const aTrip = (tl: gsap.core.Timeline, t: number) => {
      tl.to(legs, { rotation: (i: number) => (i < 2 ? 38 : -10), transformOrigin: '50% 0%', duration: 0.12, ease: 'power2.in' }, t)
        .to(cg, { rotation: -22, y: 6, transformOrigin: '50% 100%', duration: 0.14, ease: 'power2.in' }, t)
        .to(eyes, { scaleY: 1.4, transformOrigin: '50% 50%', duration: 0.1 }, t)
        .to(cg, { rotation: 5, y: 0, transformOrigin: '50% 100%', duration: 0.3, ease: 'back.out(2.5)' }, t + 0.2)
        .to(legs, { rotation: 0, transformOrigin: '50% 0%', duration: 0.25, ease: 'power2.out' }, t + 0.2)
        .to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.25, ease: 'sine.inOut' }, t + 0.5)
        .to(eyes, { scaleY: 1, transformOrigin: '50% 50%', duration: 0.25 }, t + 0.5)
      return t + 0.9
    }
    // the flagship: sit down, open the laptop, and actually work — hands tap the
    // keys, the screen streams code, eyes track it. The longest, most legible act.
    const codeLines = Array.from(laptop.querySelectorAll('[data-code]'))
    const hands = Array.from(laptop.querySelectorAll('[data-hand]'))
    const aWork = (tl: gsap.core.Timeline, t: number) => {
      // settle in — perch on the ledge (legs dangle over the edge), lean over,
      // bring the laptop up, hands to the keys
      tl.set(laptop, { display: 'inline', opacity: 0, scale: 0.9, transformOrigin: '50% 100%' }, t)
        .to(cg, { y: 6, transformOrigin: '50% 100%', duration: 0.28, ease: 'power2.out' }, t)
        .to([legs[1], legs[2]], { rotation: 7, transformOrigin: '50% 0%', duration: 0.7, ease: 'sine.inOut', yoyo: true, repeat: 2 }, t + 0.4) // idle foot kick while typing
        .to(nubL, { rotation: 24, transformOrigin: '100% 100%', duration: 0.3, ease: 'power2.inOut' }, t + 0.08)
        .to(nubR, { rotation: -24, transformOrigin: '0% 100%', duration: 0.3, ease: 'power2.inOut' }, t + 0.08)
        .to(laptop, { opacity: 1, scale: 1, duration: 0.3, ease: 'back.out(1.5)' }, t + 0.2)
        .to(eyes, { y: 1.4, scaleY: 0.78, transformOrigin: '50% 50%', duration: 0.25 }, t + 0.3)
      // type — hands alternate quick taps for the bulk of the activity
      const typeStart = t + 0.7, typeEnd = t + 3.6
      for (let i = 0, bt = typeStart; bt < typeEnd; bt += 0.15, i++) {
        const h = hands[i % 2]
        tl.to(h, { y: 1.6, duration: 0.075, ease: 'power2.in' }, bt).to(h, { y: 0, duration: 0.075, ease: 'power2.out' }, bt + 0.075)
      }
      // the screen streams: code lines flicker as output scrolls past
      codeLines.forEach((ln, j) => tl.to(ln, { opacity: 0.28, duration: 0.18, yoyo: true, repeat: 7, ease: 'none' }, typeStart + j * 0.1))
      // focused micro-bob + a glance up to think
      tl.to(cg, { rotation: 1.6, transformOrigin: '50% 100%', duration: 0.55, yoyo: true, repeat: 4, ease: 'sine.inOut' }, typeStart + 0.2)
        .to(eyes, { x: 1.4, transformOrigin: '50% 50%', duration: 0.3, yoyo: true, repeat: 1, ease: 'sine.inOut' }, typeStart + 1.7)
      // wrap up — close the lid, sit back up
      const end = typeEnd + 0.1
      tl.to(eyes, { y: 0, x: 0, scaleY: 1, transformOrigin: '50% 50%', duration: 0.25 }, end)
        .to([nubL, nubR], { rotation: 0, duration: 0.28, ease: 'power2.inOut' }, end)
        .to(laptop, { opacity: 0, scale: 0.9, duration: 0.25, ease: 'power2.in' }, end)
        .to(legs, { rotation: 0, transformOrigin: '50% 0%', duration: 0.28, ease: 'power2.inOut' }, end + 0.12)
        .to(cg, { y: 0, transformOrigin: '50% 100%', duration: 0.28, ease: 'power2.inOut' }, end + 0.12)
        .set(laptop, { display: 'none' }, end + 0.5)
        .set(hands, { y: 0 }, end + 0.5)
      return end + 0.6
    }

    const acts = [aWave, aGym, aFlag, aStomp, aLook, aSit, aYawn, aCoffee, aRead, aJuggle, aTrip]

    // ---- run / sleep / wake state machine ----
    // Two clocks drive sleep: `lastActivity` (any user input — daytime idle nap)
    // and `lastStrong` (a real session running — the only thing that keeps it up
    // at night). `wokeForWork` makes the first act after any wake be coffee → work.
    let mode: 'run' | 'sleep' = 'run'
    let lastAct = -1, nudges = 0, dead = false, waking = false, wokeForWork = false
    let pointerActive = false, dragging = false, downX = 0, awakeClicks = 0
    let lastActivity = Date.now(), lastStrong = Date.now()
    let current: gsap.core.Timeline | null = null
    let wait: gsap.core.Tween | null = null
    let zzzTl: gsap.core.Timeline | null = null
    let snoreTl: gsap.core.Tween | null = null
    let dreamTl: gsap.core.Timeline | null = null
    let dreamWait: gsap.core.Tween | null = null
    let nudgeTimer: ReturnType<typeof setTimeout> | null = null
    let clickTimer: ReturnType<typeof setTimeout> | null = null

    // Drop every prop and snap the figure back to a clean standing pose. Activities
    // show their prop up-front but only hide it on their final frame, so any kill
    // (pet, drag, reaction) mid-activity would otherwise leave the prop stuck on —
    // and the next activity would stack its prop on top. Call this on every interrupt.
    const neutralize = () => {
      gsap.set([flag, dbL, dbR, mug, book, board, surfRig, laptop, heart], { display: 'none', clearProps: 'transform,opacity' })
      gsap.set(conf, { opacity: 0, clearProps: 'transform' }) // drop any stuck confetti / juggle balls off the head
      gsap.set(spray, { opacity: 0, clearProps: 'transform' }) // and any in-flight surf spray
      gsap.set(waveEl, { opacity: 0, x: 0, scaleX: 1 })        // send the swell away if a ride was cut short
      gsap.set(codeLines, { clearProps: 'opacity' })
      gsap.set(hands, { clearProps: 'transform' })
      gsap.set(legs, { rotation: 0, scaleY: 1, transformOrigin: '50% 0%' })
      gsap.set([nubL, nubR], { x: 0, y: 0, rotation: 0 })
      gsap.set(eyes, { x: 0, y: 0, scaleY: 1 })
      gsap.set(cg, { x: 0, y: 0, rotation: 0, scaleY: 1 })
      gsap.set(svg, { y: 0 }) // drop back to the rail line (x preserved — stays where it was)
    }

    const shouldSleep = () =>
      isNight() ? Date.now() - lastStrong > NIGHT_AWAKE_MS : Date.now() - lastActivity > napMs()

    const schedule = (delay = rand(0.3, 0.8)) => { if (!dead && mode === 'run') wait = gsap.delayedCall(delay, tick) }

    const wander = (tl: gsap.core.Timeline) => {
      const e1 = (Math.random() < 0.3 ? ride : scurry)(tl, 0, farEnd())
      let i = Math.floor(Math.random() * acts.length)
      if (i === lastAct) i = (i + 1) % acts.length
      lastAct = i
      return acts[i](tl, e1 + 0.2)
    }
    // walk to centre-stage, optionally grab coffee, then settle in and work —
    // front & centre, the behaviour we most want seen.
    const workBlock = (tl: gsap.core.Timeline, withCoffee: boolean) => {
      let t = scurry(tl, 0, centerX())
      if (withCoffee) t = aCoffee(tl, t + 0.2)
      return aWork(tl, t + 0.2)
    }

    function tick() {
      if (dead || mode !== 'run') return
      if (shouldSleep()) { goSleep(); return }
      const tl = gsap.timeline()
      if (wokeForWork) { wokeForWork = false; workBlock(tl, true) }       // just woke → coffee, then work
      else if (maxX() > 108 && Math.random() < 0.16) surfWave(tl)         // ~1 in 6: a rogue wave rolls in (needs room)
      else if (Math.random() < 0.55) workBlock(tl, Math.random() < 0.3)   // mostly: sit at the laptop
      else wander(tl)                                                     // otherwise: roam + a random act
      tl.eventCallback('onComplete', () => schedule())
      current = tl
    }

    const startZzz = () => {
      gsap.set(zzzG, { opacity: 1 })
      zzzTl = gsap.timeline({ repeat: -1 })
      zzz.forEach((z, i) => {
        zzzTl!.fromTo(z, { y: 8, opacity: 0 }, { y: -8, opacity: 0.85, duration: 1.1, ease: 'sine.out' }, i * 0.55).to(z, { opacity: 0, duration: 0.4 }, i * 0.55 + 0.8)
      })
    }
    const stopZzz = () => { zzzTl?.kill(); zzzTl = null; gsap.set(zzzG, { opacity: 0 }); gsap.set(zzz, { clearProps: 'y,opacity' }) }
    // snore — the Zzz cluster swells on a slow loop while asleep
    const startSnore = () => { snoreTl = gsap.to(zzzG, { scale: 1.18, transformOrigin: '40% 100%', duration: 1.3, ease: 'sine.inOut', yoyo: true, repeat: -1 }) }
    const stopSnore = () => { snoreTl?.kill(); snoreTl = null; gsap.set(zzzG, { clearProps: 'scale' }) }
    // dream — a cookie thought-bubble drifts up now and then while asleep. Each
    // dream schedules the NEXT one after a randomized gap (not a fixed metronome),
    // so it shows up far less often and on an irregular rhythm.
    const playDream = () => {
      dreamTl = gsap.timeline({
        onComplete: () => { dreamWait = gsap.delayedCall(rand(7, 18), playDream) },
      })
      dreamTl.set(dream, { display: 'inline', opacity: 0, scale: 0.6, transformOrigin: '40% 100%' })
        .to(dream, { opacity: 1, scale: 1, duration: 0.5, ease: 'back.out(1.6)' })
        .to(dream, { opacity: 1, duration: rand(1.4, 2.6) })
        .to(dream, { opacity: 0, scale: 0.7, duration: 0.4, ease: 'power2.in' })
        .set(dream, { display: 'none' })
    }
    const startDream = () => { dreamWait = gsap.delayedCall(rand(3, 9), playDream) }
    const stopDream = () => { dreamWait?.kill(); dreamWait = null; dreamTl?.kill(); dreamTl = null; gsap.set(dream, { display: 'none' }); gsap.set(dream, { clearProps: 'opacity,scale' }) }

    const goSleep = () => {
      mode = 'sleep'; nudges = 0
      current?.kill(); wait?.kill(); current = null; wait = null
      neutralize() // drop any prop/wave a just-finished activity left mid-frame so nothing shows while asleep
      gsap.set(brows, { display: 'none' })
      blink.pause(); breathe.timeScale(0.55)
      const tl = gsap.timeline()
      // yawn lead-in, then slump
      tl.to(nubL, { rotation: -50, y: -3, transformOrigin: '100% 100%', duration: 0.35, ease: 'power2.out' }, 0)
        .to(nubR, { rotation: 50, y: -3, transformOrigin: '0% 100%', duration: 0.35, ease: 'power2.out' }, 0)
        .to(eyes, { scaleY: 0.15, transformOrigin: '50% 50%', duration: 0.3 }, 0.1)
        .to([nubL, nubR], { rotation: 0, y: 0, duration: 0.4, ease: 'power2.inOut' }, 0.5)
        .to(legs, { rotation: 0, scaleY: 1, duration: 0.2 }, 0.6)
        .to(cg, { rotation: 0, y: 3, transformOrigin: '50% 100%', duration: 0.45, ease: 'power2.inOut' }, 0.6)
        .to(eyes, { scaleY: 0.12, transformOrigin: '50% 50%', duration: 0.4, ease: 'sine.inOut' }, 0.6)
        .call(() => { startZzz(); startSnore(); startDream() }, undefined, 1.1)
      current = tl
    }

    const nudgeStir = (n: number) => {
      const tl = gsap.timeline()
      tl.to(eyes, { scaleY: Math.min(0.3 + n * 0.12, 0.7), transformOrigin: '50% 50%', duration: 0.12 }, 0)
        .to(cg, { rotation: n % 2 ? 7 : -7, transformOrigin: '50% 100%', duration: 0.08, yoyo: true, repeat: 3 + n }, 0)
        .to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.18 }, 0.5)
        .to(eyes, { scaleY: 0.12, transformOrigin: '50% 50%', duration: 0.25 }, 0.55)
      current = tl
    }

    // mark the moment of waking — reset the clocks so it gets a full awake window,
    // and tee up the coffee → work routine.
    const markWoke = () => { const n = Date.now(); lastActivity = n; lastStrong = n; wokeForWork = true }

    const wakeAngry = () => {
      mode = 'run'; nudges = 0; waking = true
      current?.kill()
      if (nudgeTimer) { clearTimeout(nudgeTimer); nudgeTimer = null }
      stopZzz(); stopSnore(); stopDream(); breathe.timeScale(1)
      gsap.set(brows, { display: 'inline' })
      const tl = gsap.timeline({ onComplete: () => { waking = false; gsap.set(brows, { display: 'none' }); markWoke(); blink.restart(); if (!dead) tick() } })
      tl.to(eyes, { scaleY: 1.35, transformOrigin: '50% 50%', duration: 0.1 }, 0)
        .to(cg, { y: 0, transformOrigin: '50% 100%', duration: 0.1 }, 0)
        .fromTo(cg, { x: -2.5 }, { x: 2.5, duration: 0.05, yoyo: true, repeat: 8, ease: 'none' }, 0.12)
        .set(cg, { x: 0 }, 0.62)
        .to(cg, { y: -9, duration: 0.18, ease: 'power2.out' }, 0.64)
        .to(cg, { y: 0, duration: 0.34, ease: 'bounce.out' }, 0.82)
        .to(eyes, { scaleY: 1, transformOrigin: '50% 50%', duration: 0.25 }, 1.0)
      current = tl
    }

    // gentle wake — a stretch and a stand, used when the workspace stirs (you ran
    // a session) or you come back to the keyboard. Flows straight into work.
    const wakeToWork = () => {
      if (dead || mode !== 'sleep' || waking) return
      mode = 'run'; nudges = 0; waking = true
      current?.kill()
      if (nudgeTimer) { clearTimeout(nudgeTimer); nudgeTimer = null }
      stopZzz(); stopSnore(); stopDream(); breathe.timeScale(1)
      gsap.set(brows, { display: 'none' })
      const tl = gsap.timeline({ onComplete: () => { waking = false; markWoke(); blink.restart(); if (!dead) tick() } })
      tl.to(eyes, { scaleY: 1, transformOrigin: '50% 50%', duration: 0.22 }, 0)
        .to(nubL, { rotation: -46, y: -4, transformOrigin: '100% 100%', duration: 0.36, ease: 'power2.out' }, 0)
        .to(nubR, { rotation: 46, y: -4, transformOrigin: '0% 100%', duration: 0.36, ease: 'power2.out' }, 0)
        .to(cg, { scaleY: 1.08, transformOrigin: '50% 100%', duration: 0.36, ease: 'power2.out' }, 0)
        .to(eyes, { scaleY: 1.15, transformOrigin: '50% 50%', duration: 0.2, yoyo: true, repeat: 1 }, 0.2)
        .to([nubL, nubR], { rotation: 0, y: 0, duration: 0.4, ease: 'power2.inOut' }, 0.5)
        .to(cg, { scaleY: 1, y: -6, transformOrigin: '50% 100%', duration: 0.2, ease: 'power2.out' }, 0.5)
        .to(cg, { y: 0, duration: 0.42, ease: 'bounce.out' }, 0.7)
      current = tl
    }

    // record user/workspace activity; wakes from sleep per the day/night rules.
    const noteActivity = (strong: boolean) => {
      const now = Date.now()
      lastActivity = now
      if (strong) lastStrong = now
      if (mode === 'sleep' && !waking) {
        if (strong || !isNight()) wakeToWork() // sessions wake any time; mere input only by day
      }
    }

    // resume the run loop after a one-off reaction/pet/tickle
    const resumeSoon = (delay = 0.2) => { if (!dead && mode === 'run' && !dragging) wait = gsap.delayedCall(delay, tick) }

    // ---- pet / tickle / drag (awake) ----
    const buildPet = (tl: gsap.core.Timeline) => {
      tl.set(heart, { display: 'inline', opacity: 1, y: 0, scale: 0.5 }, 0)
        .to(heart, { y: -10, scale: 1.1, opacity: 0, transformOrigin: '50% 100%', duration: 0.9, ease: 'power2.out' }, 0)
        .to(eyes, { scaleY: 0.45, transformOrigin: '50% 50%', duration: 0.15, yoyo: true, repeat: 1 }, 0)
        .to(cg, { rotation: 6, transformOrigin: '50% 100%', duration: 0.12, yoyo: true, repeat: 3 }, 0)
        .to(cg, { rotation: 0, transformOrigin: '50% 100%', duration: 0.15 }, 0.5)
        .set(heart, { display: 'none' }, 1.0)
    }
    const buildTickle = (tl: gsap.core.Timeline) => {
      tl.to(cg, { rotation: 10, transformOrigin: '50% 100%', duration: 0.08, yoyo: true, repeat: 7, ease: 'sine.inOut' }, 0)
        .to(cg, { y: -4, duration: 0.1, yoyo: true, repeat: 5, ease: 'sine.inOut' }, 0)
        .to(eyes, { scaleY: 0.3, transformOrigin: '50% 50%', duration: 0.1, yoyo: true, repeat: 3 }, 0)
        .to(cg, { rotation: 0, y: 0, transformOrigin: '50% 100%', duration: 0.2 }, 0.7)
    }

    const onPointerDown = (e: { clientX: number }) => {
      if (dead || waking) return
      if (mode === 'sleep') {
        nudges += 1
        if (nudgeTimer) clearTimeout(nudgeTimer)
        nudgeTimer = setTimeout(() => { nudges = 0; nudgeTimer = null }, NUDGE_WINDOW_MS)
        if (nudges >= WAKE_NUDGES) wakeAngry()
        else { current?.kill(); nudgeStir(nudges) }
        return
      }
      // awake: start a tap-or-drag gesture
      pointerActive = true; dragging = false; downX = e.clientX
      current?.kill(); wait?.kill()
      neutralize() // drop any prop the interrupted activity was holding
    }
    const onPointerMove = (e: PointerEvent) => {
      noteActivity(false) // any mouse movement = you're around (wakes it by day)
      if (!pointerActive) return
      if (!dragging && Math.abs(e.clientX - downX) > 4) {
        dragging = true
        gsap.to([nubL, nubR], { rotation: 0, y: -3, duration: 0.15 })
        gsap.to(legs, { rotation: 0, scaleY: 1, duration: 0.1 })
        gsap.to(eyes, { scaleY: 1.2, transformOrigin: '50% 50%', duration: 0.12 })
        gsap.to(cg, { y: 2, transformOrigin: '50% 100%', duration: 0.12 })
      }
      if (dragging) {
        const left = wrap.getBoundingClientRect().left
        gsap.set(svg, { x: Math.max(0, Math.min(maxX(), e.clientX - left - RUNNER_W / 2)) })
      }
    }
    const onPointerUp = () => {
      if (!pointerActive) return
      pointerActive = false
      if (dragging) {
        dragging = false
        const tl = gsap.timeline({ onComplete: () => { lastActivity = Date.now(); lastStrong = Date.now(); resumeSoon() } })
        tl.to(eyes, { scaleY: 1, transformOrigin: '50% 50%', duration: 0.2 }, 0)
          .to([nubL, nubR], { rotation: 0, y: 0, duration: 0.2 }, 0)
          .to(cg, { y: -5, duration: 0.14, ease: 'power2.out' }, 0)
          .to(cg, { y: 0, duration: 0.4, ease: 'bounce.out' }, 0.14)
        current = tl
      } else {
        awakeClicks += 1
        if (clickTimer) clearTimeout(clickTimer)
        clickTimer = setTimeout(() => { awakeClicks = 0; clickTimer = null }, 1200)
        const tl = gsap.timeline({ onComplete: () => resumeSoon() })
        if (awakeClicks >= 3) buildTickle(tl); else buildPet(tl)
        current = tl
      }
    }
    downRef.current = onPointerDown
    window.addEventListener('pointermove', onPointerMove)
    window.addEventListener('pointerup', onPointerUp)

    // ---- reactive triggers (called from the prop-watching effects) ----
    // A one-off reaction only plays when it's already awake and free; waking and
    // activity-tracking are handled separately so the dispatcher stays declarative.
    const playReaction = (build: (tl: gsap.core.Timeline) => void) => {
      if (dead || mode !== 'run' || waking || pointerActive) return
      current?.kill(); wait?.kill()
      neutralize() // an interrupted activity must drop its prop (mug/laptop/board…)
      const tl = gsap.timeline({ onComplete: () => resumeSoon() })
      build(tl)
      current = tl
    }
    const reactWorried = () => playReaction((tl) => {
      tl.to(eyes, { scaleY: 1.3, transformOrigin: '50% 50%', duration: 0.12 }, 0)
        .to(nubL, { rotation: -40, y: -3, transformOrigin: '100% 100%', duration: 0.18 }, 0)
        .to(nubR, { rotation: 40, y: -3, transformOrigin: '0% 100%', duration: 0.18 }, 0)
        .fromTo(cg, { x: -1.5 }, { x: 1.5, duration: 0.06, yoyo: true, repeat: 14, ease: 'none' }, 0.1)
        .set(cg, { x: 0 }, 1.1)
        .to([nubL, nubR], { rotation: 0, y: 0, duration: 0.3 }, 1.2)
        .to(eyes, { scaleY: 1, transformOrigin: '50% 50%', duration: 0.3 }, 1.2)
    })
    const reactRelieved = () => playReaction((tl) => {
      tl.to(cg, { y: -7, duration: 0.18, ease: 'power2.out' }, 0).to(cg, { y: 0, duration: 0.4, ease: 'bounce.out' }, 0.18)
        .to(eyes, { scaleY: 0.5, transformOrigin: '50% 50%', duration: 0.15, yoyo: true, repeat: 1 }, 0)
    })
    const reactCelebrate = () => playReaction((tl) => {
      tl.to(legs, { scaleY: 0.6, transformOrigin: '50% 100%', duration: 0.14, ease: 'power2.in' }, 0)
        .to(cg, { y: -16, duration: 0.3, ease: 'sine.out' }, 0.14)
        .to(legs, { scaleY: 1.05, duration: 0.28 }, 0.14)
        .to(cg, { y: 0, duration: 0.32, ease: 'power2.in' }, 0.44)
        .to(legs, { scaleY: 1, duration: 0.2, ease: 'elastic.out(1, 0.5)' }, 0.76)
      burst(tl, 0.3, [0, 1, 2, 3, 4, 5, 6, 7])
    })
    const reactPointInbox = () => playReaction((tl) => {
      tl.to(nubR, { rotation: 72, y: -8, transformOrigin: '0% 100%', duration: 0.25, ease: 'back.out(2)' }, 0)
        .to(eyes, { y: -1.6, transformOrigin: '50% 50%', duration: 0.2 }, 0)
        .to(cg, { y: -4, transformOrigin: '50% 100%', duration: 0.16, ease: 'power2.out' }, 0)
        .to(cg, { y: 0, duration: 0.4, ease: 'bounce.out' }, 0.16)
        .to(nubR, { rotation: 60, transformOrigin: '0% 100%', duration: 0.12, yoyo: true, repeat: 3 }, 0.5)
        .to(nubR, { rotation: 0, y: 0, transformOrigin: '0% 100%', duration: 0.3 }, 1.1)
        .to(eyes, { y: 0, transformOrigin: '50% 50%', duration: 0.3 }, 1.1)
    })
    reactRef.current = {
      // "we ran something" — wakes any time (incl. night); cheers if already up.
      session: () => { noteActivity(true); reactCelebrate() },
      // new inbox item — points at the nav if awake; counts as light activity.
      inbox: () => { noteActivity(false); reactPointInbox() },
      // connection lost/restored — fret/relief only while awake; never wakes it.
      disconnect: () => reactWorried(),
      reconnect: () => reactRelieved(),
    }

    // ---- ambient activity: any keypress / tab-focus counts as "you're around" ----
    const onKey = () => noteActivity(false)
    const onVisible = () => { if (!document.hidden) noteActivity(false) }
    window.addEventListener('keydown', onKey)
    document.addEventListener('visibilitychange', onVisible)

    gsap.set(svg, { x: 0 })
    // start asleep at night (nudge or a new session wakes it); otherwise get to work
    wait = gsap.delayedCall(isNight() ? 0.8 : 0.5, isNight() ? goSleep : tick)

    return () => {
      dead = true
      breathe.kill(); blink.kill(); wait?.kill(); current?.kill(); zzzTl?.kill(); snoreTl?.kill(); dreamTl?.kill(); dreamWait?.kill()
      if (nudgeTimer) clearTimeout(nudgeTimer)
      if (clickTimer) clearTimeout(clickTimer)
      window.removeEventListener('pointermove', onPointerMove)
      window.removeEventListener('pointerup', onPointerUp)
      window.removeEventListener('keydown', onKey)
      document.removeEventListener('visibilitychange', onVisible)
      reactRef.current = {}
      gsap.set([svg, waveEl, cg, breath, eyes, nubL, nubR, flag, dbL, dbR, mug, book, heart, board, surfRig, dream, laptop, brows, zzzG, ...legs, ...conf, ...zzz, ...codeLines, ...hands, ...spray], { clearProps: 'all' })
      gsap.set([flag, dbL, dbR, mug, book, heart, board, surfRig, dream, laptop, brows], { display: 'none' })
      gsap.set([waveEl, zzzG, ...conf], { opacity: 0 })
    }
  }, [])

  // react to connection drops/recoveries (only on transition)
  useEffect(() => {
    const was = prevConn.current; prevConn.current = conn
    if (conn === undefined || was === undefined) return
    if (conn !== 'open' && was === 'open') reactRef.current.disconnect?.()
    else if (conn === 'open' && was !== 'open') reactRef.current.reconnect?.()
  }, [conn])

  // a session starting to run is "we ran something" — wakes it and cheers
  useEffect(() => {
    const was = prevRun.current; prevRun.current = running
    if (running !== undefined && was !== undefined && running > was) reactRef.current.session?.()
  }, [running])

  // a newly-monitored session counts the same way
  useEffect(() => {
    const was = prevMon.current; prevMon.current = monitored
    if (monitored !== undefined && was !== undefined && monitored > was) reactRef.current.session?.()
  }, [monitored])

  // point at the Inbox nav when a new item arrives
  useEffect(() => {
    const was = prevInbox.current; prevInbox.current = inbox
    if (inbox !== undefined && was !== undefined && inbox > was) reactRef.current.inbox?.()
  }, [inbox])

  return (
    <div ref={wrapRef} className="rail-runner">
      {/* the swell — its own full-rail layer behind the figure (so Claude rides
          ABOVE it). Hidden until a surf set-piece sweeps it in from a corner.
          preserveAspectRatio="none" lets it stretch to WAVE_W×WAVE_H px. */}
      <svg
        ref={waveRef}
        width={WAVE_W}
        height={WAVE_H}
        viewBox="0 -3 56 25"
        preserveAspectRatio="none"
        aria-hidden="true"
        style={{ position: 'absolute', left: 0, bottom: 0, opacity: 0, pointerEvents: 'none' }}
      >
        {/* swell body — brand purple, base sunk past the line so it never floats */}
        <path d="M0 22 C 10 22 16 3 30 2 C 42 1 48 13 56 12 L56 22 Z" fill="#645df6" />
        {/* foam lip riding the crest */}
        <path d="M0 20.5 C 10 20.5 16 2 30 1 C 42 0 48 12 56 11" fill="none" stroke="#cfccfd" strokeWidth="2.2" strokeLinecap="round" />
        {/* the curl breaking over */}
        <path d="M22 4 Q30 -3 41 3 Q31 3 27 12 Z" fill="#8b87f8" />
        {/* spray flecks off the crest (animated during the ride) */}
        <rect data-spray x="29" y="-1" width="3" height="3" rx="1.3" fill="#fff" opacity="0" />
        <rect data-spray x="35" y="1" width="2.4" height="2.4" rx="1" fill="#fff" opacity="0" />
        <rect data-spray x="25" y="-3" width="2" height="2" rx="0.8" fill="#fff" opacity="0" />
      </svg>
      <svg ref={svgRef} width={RUNNER_W} height={RUNNER_W} viewBox="0 -20 64 64" aria-hidden="true" onPointerDown={(e) => downRef.current(e)} style={{ position: 'absolute', left: 0, bottom: 0, cursor: 'pointer', touchAction: 'none', overflow: 'visible' }}>
        <ClaudeFigure r={r} withProps />
        <g ref={browsRef} style={{ display: 'none' }}>
          <rect x="13" y="13" width="9" height="2.6" rx="1" fill={EYE} transform="rotate(20 17.5 14.3)" />
          <rect x="42" y="13" width="9" height="2.6" rx="1" fill={EYE} transform="rotate(-20 46.5 14.3)" />
        </g>
        <g ref={zzzRef} style={{ opacity: 0 }}>
          <text x="39" y="4" fill="var(--text-3)" style={{ fontSize: '7px', fontFamily: 'var(--font-mono)', fontWeight: 700 }}>z</text>
          <text x="44" y="-3" fill="var(--text-3)" style={{ fontSize: '9px', fontFamily: 'var(--font-mono)', fontWeight: 700 }}>z</text>
          <text x="50" y="-11" fill="var(--text-3)" style={{ fontSize: '11px', fontFamily: 'var(--font-mono)', fontWeight: 700 }}>z</text>
        </g>
      </svg>
    </div>
  )
}
