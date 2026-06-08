export const UI_DATA_IDLE_REFETCH_MS = 30_000

const snapshotOnlyEvents = new Set(['agent_hook', 'liveness', 'runtime', 'hook_health'])

export function focusedLiveInvalidationKeys(env: { type?: string } | null | undefined): string[] | null {
  if (!env?.type) return null
  if (snapshotOnlyEvents.has(env.type)) return ['ui-data']
  return null
}
