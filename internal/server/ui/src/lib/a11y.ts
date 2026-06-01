// Keyboard/assistive-tech helpers. The console grew up on bare `<div onClick>`
// containers (cards, rows, group headers) that are unreachable by keyboard and
// invisible to screen readers. `clickable()` retrofits the missing button
// semantics onto any element without changing its tag or layout.

import type { KeyboardEvent as ReactKeyboardEvent, MouseEvent as ReactMouseEvent } from 'react'

export interface ClickableOpts {
  /** Stop the activation event bubbling to an outer clickable container. */
  stopPropagation?: boolean
  /** ARIA role for the element. Defaults to 'button'. */
  role?: string
  /** When true the element is inert: not focusable and ignores activation. */
  disabled?: boolean
}

export interface ClickableProps {
  role: string
  tabIndex: number
  onClick: (e: ReactMouseEvent) => void
  onKeyDown: (e: ReactKeyboardEvent) => void
}

// Selector for genuinely-interactive descendants. When a keypress originates
// inside one of these (a rename input, an inner action button, a link) the
// container must NOT also activate — otherwise typing a space in an inline
// editor would "click" the row behind it.
const NESTED_INTERACTIVE = 'input, textarea, select, button, a, [contenteditable="true"]'

/**
 * Make a non-button element behave like a button for keyboard and screen-reader
 * users: focusable (tabIndex 0) and activated by Enter or Space. The global
 * `:focus-visible` rule in styles/base.css already paints the focus ring, so no
 * per-element focus CSS is needed.
 *
 * Spread the result onto the element and drop its raw `onClick`:
 *   <article {...clickable(() => navigate(href))} className="card">…</article>
 *
 * Inner interactive controls keep their own handlers; this helper ignores
 * key activation that originates inside them, and `opts.stopPropagation` covers
 * the click-bubbling case for nested clickables.
 */
export function clickable(onActivate: () => void, opts: ClickableOpts = {}): ClickableProps {
  const { stopPropagation = false, role = 'button', disabled = false } = opts
  return {
    role,
    tabIndex: disabled ? -1 : 0,
    onClick: (e) => {
      if (disabled) return
      if (stopPropagation) e.stopPropagation()
      onActivate()
    },
    onKeyDown: (e) => {
      if (disabled) return
      if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return
      const target = e.target as HTMLElement
      if (target !== e.currentTarget && target.closest(NESTED_INTERACTIVE)) return
      e.preventDefault() // stop Space from scrolling the page
      if (stopPropagation) e.stopPropagation()
      onActivate()
    },
  }
}
