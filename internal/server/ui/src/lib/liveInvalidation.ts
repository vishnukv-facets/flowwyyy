const snapshotOnlyEvents = new Set(['agent_hook', 'liveness', 'runtime', 'hook_health'])

export function focusedLiveInvalidationKeys(env: { type?: string; data?: unknown } | null | undefined): string[] | null {
  if (!env?.type) return null
  if (snapshotOnlyEvents.has(env.type)) return ['ui-data']
  // Live cascade stage deltas only touch the steering-runs view — don't broadly
  // refetch the whole UI on every stage of every triaged message.
  if (env.type === 'steering_stage') return ['steering-runs']
  // ui_change carries the changed surface in data.kind. A chat lifecycle change
  // (reopen/archive/unarchive/delete) only touches the Chats list, so refetch
  // just that key rather than broadly invalidating the whole UI. Every other
  // ui_change kind falls through to the broad refetch below.
  if (env.type === 'ui_change') {
    const kind = (env.data as { kind?: string } | undefined)?.kind
    if (kind === 'chats') return ['chats']
    // A kb/*.md change (capture, prune, edit) only touches the Knowledge view.
    if (kind === 'kb') return ['kb']
  }
  return null
}
