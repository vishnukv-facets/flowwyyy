// Universal confirm bus. ConfirmHost subscribes and renders the dialog;
// anything can `await confirmAction(...)` before a destructive action.
import { useEffect, useState } from 'react'

export interface ConfirmRequest {
  id: number
  title: string
  body?: string
  confirmLabel?: string
  cancelLabel?: string
  danger?: boolean
  resolve: (ok: boolean) => void
}

let seq = 0
let current: ConfirmRequest | null = null
const listeners = new Set<(c: ConfirmRequest | null) => void>()

function emit() {
  listeners.forEach((fn) => fn(current))
}

export function confirmAction(opts: {
  title: string
  body?: string
  confirmLabel?: string
  cancelLabel?: string
  danger?: boolean
}): Promise<boolean> {
  return new Promise((resolve) => {
    // If a prior dialog is somehow still open, decline it first.
    if (current) current.resolve(false)
    current = { id: ++seq, resolve, ...opts }
    emit()
  })
}

export function resolveConfirm(ok: boolean) {
  if (!current) return
  const c = current
  current = null
  emit()
  c.resolve(ok)
}

export function useConfirm(): ConfirmRequest | null {
  const [c, setC] = useState(current)
  useEffect(() => {
    listeners.add(setC)
    return () => {
      listeners.delete(setC)
    }
  }, [])
  return c
}
