export interface TraceWindowAnchor {
  windowId: string
  nowMs: number
}

export function nextTraceWindowAnchor(
  current: TraceWindowAnchor | null | undefined,
  windowId: string,
  nowMs: number,
): TraceWindowAnchor {
  if (current?.windowId === windowId) return current
  return { windowId, nowMs }
}

export function traceSinceForWindow(anchor: TraceWindowAnchor, windowMs: number): string {
  return new Date(anchor.nowMs - windowMs).toISOString()
}
