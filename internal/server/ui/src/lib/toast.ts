// Minimal toast bus. The Toaster component subscribes; anything can push.
import { useEffect, useState } from 'react'

export interface Toast {
  id: number
  kind: 'ok' | 'error' | 'info'
  message: string
}

let seq = 0
const toasts: Toast[] = []
const listeners = new Set<(t: Toast[]) => void>()

function emit() {
  listeners.forEach((fn) => fn([...toasts]))
}

export function pushToast(kind: Toast['kind'], message: string) {
  const id = ++seq
  toasts.push({ id, kind, message })
  emit()
  setTimeout(() => dismissToast(id), kind === 'error' ? 6000 : 3600)
}

export function dismissToast(id: number) {
  const i = toasts.findIndex((t) => t.id === id)
  if (i >= 0) {
    toasts.splice(i, 1)
    emit()
  }
}

export function useToasts(): Toast[] {
  const [list, setList] = useState<Toast[]>(toasts)
  useEffect(() => {
    listeners.add(setList)
    return () => {
      listeners.delete(setList)
    }
  }, [])
  return list
}
