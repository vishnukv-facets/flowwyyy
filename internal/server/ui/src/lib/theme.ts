// Theme: persisted light/dark, applied to <html data-theme>. Initial value is
// set by an inline script in index.html to avoid a flash before React mounts.

export type Theme = 'light' | 'dark'

const KEY = 'flow.theme'
const listeners = new Set<(t: Theme) => void>()

export function getTheme(): Theme {
  const attr = document.documentElement.getAttribute('data-theme')
  return attr === 'light' ? 'light' : 'dark'
}

export function setTheme(t: Theme) {
  document.documentElement.setAttribute('data-theme', t)
  try {
    localStorage.setItem(KEY, t)
  } catch {
    /* noop */
  }
  listeners.forEach((fn) => fn(t))
}

type ViewTransitionDoc = Document & {
  startViewTransition?: (cb: () => void) => { ready: Promise<void> }
}

function prefersReducedMotion(): boolean {
  return window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false
}

// toggleTheme flips light↔dark. When an origin point is given (the toggle
// button's center) and the browser supports the View Transitions API, the new
// theme is revealed with a circular wipe expanding from that point — the
// browser snapshots old/new and we animate a clip-path on the new snapshot, so
// there's no manual color tweening or layout thrash. Falls back to an instant
// swap when unsupported or when the user prefers reduced motion. Returns the
// new theme synchronously either way so React state stays in lockstep.
export function toggleTheme(origin?: { x: number; y: number }): Theme {
  const next: Theme = getTheme() === 'dark' ? 'light' : 'dark'
  const doc = document as ViewTransitionDoc
  if (!origin || typeof doc.startViewTransition !== 'function' || prefersReducedMotion()) {
    setTheme(next)
    return next
  }
  const transition = doc.startViewTransition(() => setTheme(next))
  transition.ready.then(() => {
    // Radius reaches the farthest screen corner so the wipe covers everything.
    const r = Math.hypot(
      Math.max(origin.x, window.innerWidth - origin.x),
      Math.max(origin.y, window.innerHeight - origin.y),
    )
    document.documentElement.animate(
      {
        clipPath: [
          `circle(0px at ${origin.x}px ${origin.y}px)`,
          `circle(${r}px at ${origin.x}px ${origin.y}px)`,
        ],
      },
      { duration: 480, easing: 'cubic-bezier(0.4, 0, 0.2, 1)', pseudoElement: '::view-transition-new(root)' },
    )
  })
  return next
}

export function onThemeChange(fn: (t: Theme) => void): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}
