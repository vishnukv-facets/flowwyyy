import { useEffect } from 'react'

// The product name kept as the tab suffix so every flow tab is still
// recognizable at a glance among many open tabs, while the page/entity name
// leads (browsers truncate from the right, so the specific part stays visible).
const SUFFIX = 'flowwyyy'
const FALLBACK = 'flowwyyy · operator console'

/**
 * Sets document.title to `<title> · flow` for the lifetime of the calling
 * screen. Pass the page label for static screens ("Sessions") or the live
 * entity name for detail screens (a task/project/playbook name). Passing
 * undefined (e.g. while data is still loading) leaves the generic title so the
 * tab never shows a blank or "undefined".
 *
 * No reset on unmount: the next screen sets its own title on mount, so resetting
 * here would only cause a flash of the generic title during route transitions.
 */
export function useDocumentTitle(title: string | undefined | null) {
  useEffect(() => {
    const trimmed = (title ?? '').trim()
    document.title = trimmed ? `${trimmed} · ${SUFFIX}` : FALLBACK
  }, [title])
}
