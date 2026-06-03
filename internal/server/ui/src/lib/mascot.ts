// Client-side mascot preferences (sidebar runner). Persisted in localStorage,
// read live by ClaudeRunner and the Settings screen. Mirrors lib/theme's shape.
import { useEffect, useState } from 'react'

export interface MascotPrefs {
  enabled: boolean
  napSec: number // daytime: this long with no activity → it nods off (wakes when you return)
}

const KEY = 'flow.mascot.v1'
const DEFAULT: MascotPrefs = { enabled: true, napSec: 150 }

// Selectable nap durations (seconds) surfaced in Settings.
export const NAP_OPTIONS: { label: string; sec: number }[] = [
  { label: '15 sec', sec: 15 },
  { label: '30 sec', sec: 30 },
  { label: '1 min', sec: 60 },
  { label: '2 min', sec: 120 },
  { label: '2.5 min', sec: 150 },
  { label: '5 min', sec: 300 },
  { label: '10 min', sec: 600 },
]

function read(): MascotPrefs {
  if (typeof window === 'undefined') return { ...DEFAULT }
  try {
    return { ...DEFAULT, ...(JSON.parse(localStorage.getItem(KEY) || '{}') as Partial<MascotPrefs>) }
  } catch {
    return { ...DEFAULT }
  }
}

let prefs: MascotPrefs = read()
const subs = new Set<(p: MascotPrefs) => void>()

export function getMascotPrefs(): MascotPrefs {
  return prefs
}

export function setMascotPrefs(patch: Partial<MascotPrefs>) {
  prefs = { ...prefs, ...patch }
  try {
    localStorage.setItem(KEY, JSON.stringify(prefs))
  } catch {
    /* ignore */
  }
  subs.forEach((f) => f(prefs))
}

export function onMascotPrefsChange(fn: (p: MascotPrefs) => void): () => void {
  subs.add(fn)
  return () => {
    subs.delete(fn)
  }
}

// Live nap duration in ms (guarded so it can never be absurdly small).
export function napMs(): number {
  return Math.max(3000, getMascotPrefs().napSec * 1000)
}

export function useMascotPrefs(): MascotPrefs {
  const [p, setP] = useState(getMascotPrefs())
  useEffect(() => onMascotPrefsChange(setP), [])
  return p
}
