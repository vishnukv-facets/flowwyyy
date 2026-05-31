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

export function toggleTheme(): Theme {
  const next: Theme = getTheme() === 'dark' ? 'light' : 'dark'
  setTheme(next)
  return next
}

export function onThemeChange(fn: (t: Theme) => void): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}
