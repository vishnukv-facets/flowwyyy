/**
 * MobileNav — fixed bottom tab bar + "More" drawer, visible only at ≤640px.
 *
 * CSS hides this component at ≥641px via `.mobile-nav { display: none !important }`.
 * The 5 primary tabs are hardcoded from the primary destinations; the "More"
 * drawer lists everything else using the shared navDefs source.
 *
 * Z-index: 90 — below the floating-terminal/modal band (95–100), above content.
 */
import { useEffect, useCallback, useState } from 'react'
import { Link, useLocation } from 'wouter'
import { MoreHorizontal, X } from 'lucide-react'
import { buildNavGroups, type NavDef } from './navDefs'
import { useAttention, useInbox, useUiData } from '../lib/query'

// ---- Primary destinations shown in the tab bar --------------------------
const PRIMARY_SLUGS = ['/', '/sessions', '/tasks', '/inbox', '/attention'] as const

// ---- Component ----------------------------------------------------------

export function MobileNav() {
  const [loc, navigate] = useLocation()
  const [drawerOpen, setDrawerOpen] = useState(false)

  const { data: ui } = useUiData()
  const { data: inbox } = useInbox()
  const { data: attentionItems } = useAttention('new')

  const running = (ui?.AGENTS ?? []).filter((a) => a.status === 'running').length
  const backlog = ui?.BACKLOG?.length ?? 0
  const unread = inbox?.unread_count ?? 0
  const attentionCount = attentionItems?.length ?? 0

  // Build nav groups (icon size 20px for better touch affordance)
  const groups = buildNavGroups({ iconSize: 20, running, backlog, unread, attentionCount })
  const allItems = groups.flatMap((g) => g.items)

  // Split into primary tabs and drawer items
  const primaryItems = PRIMARY_SLUGS.map((slug) => allItems.find((n) => n.to === slug)).filter(
    (n): n is NavDef => n !== undefined,
  )
  // Everything NOT in the primary bar goes into the drawer
  const primarySet = new Set(PRIMARY_SLUGS as readonly string[])
  const drawerGroups = groups.map((g) => ({
    ...g,
    items: g.items.filter((n) => !primarySet.has(n.to)),
  })).filter((g) => g.items.length > 0)

  // Close drawer on route change
  useEffect(() => {
    setDrawerOpen(false)
  }, [loc])

  // Dismiss drawer on Escape
  const onKeyDown = useCallback((e: KeyboardEvent) => {
    if (e.key === 'Escape' && drawerOpen) {
      e.preventDefault()
      setDrawerOpen(false)
    }
  }, [drawerOpen])
  useEffect(() => {
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onKeyDown])

  return (
    <>
      {/* ---- Bottom tab bar ---- */}
      <nav className="mobile-nav" aria-label="Mobile navigation">
        {primaryItems.map((n) => {
          const active = n.match(loc)
          return (
            <Link
              key={n.to}
              href={n.to}
              className={`mobile-nav-tab${active ? ' active' : ''}`}
              aria-current={active ? 'page' : undefined}
              aria-label={n.label}
            >
              <span className="mobile-nav-tab-icon">
                {n.icon}
                {n.badge ? (
                  <span className="mobile-nav-tab-badge" style={n.tone ? { background: n.tone } : undefined}>
                    {n.badge > 99 ? '99+' : n.badge}
                  </span>
                ) : null}
              </span>
              <span className="mobile-nav-tab-label">{n.shortLabel ?? n.label}</span>
            </Link>
          )
        })}

        {/* ---- More button ---- */}
        <button
          type="button"
          className={`mobile-nav-tab${drawerOpen ? ' active' : ''}`}
          onClick={() => setDrawerOpen((o) => !o)}
          aria-label="More navigation options"
          aria-expanded={drawerOpen}
        >
          <span className="mobile-nav-tab-icon">
            {drawerOpen ? <X size={20} /> : <MoreHorizontal size={20} />}
          </span>
          <span className="mobile-nav-tab-label">More</span>
        </button>
      </nav>

      {/* ---- Drawer + backdrop ---- */}
      {drawerOpen && (
        <>
          {/* Backdrop — tap to dismiss */}
          <div
            className="mobile-nav-backdrop"
            onClick={() => setDrawerOpen(false)}
            aria-hidden="true"
          />

          {/* Sheet */}
          <div
            className="mobile-nav-drawer"
            role="dialog"
            aria-modal="true"
            aria-label="More destinations"
          >
            <div className="mobile-nav-drawer-handle" aria-hidden="true" />

            {drawerGroups.map((g) => (
              <div key={g.label}>
                <div className="mobile-nav-drawer-section">{g.label}</div>
                {g.items.map((n) => {
                  const active = n.match(loc)
                  return (
                    <Link
                      key={n.to}
                      href={n.to}
                      className={`mobile-nav-drawer-item${active ? ' active' : ''}`}
                      aria-current={active ? 'page' : undefined}
                      onClick={() => {
                        // navigate happens via href; just close the drawer
                        setDrawerOpen(false)
                        navigate(n.to)
                      }}
                    >
                      <span className="mobile-nav-drawer-item-icon">{n.icon}</span>
                      <span>{n.label}</span>
                      {n.badge ? (
                        <span
                          className="mobile-nav-drawer-item-badge"
                          style={n.tone ? { color: n.tone, borderColor: 'currentColor' } : undefined}
                        >
                          {n.badge}
                        </span>
                      ) : null}
                    </Link>
                  )
                })}
              </div>
            ))}
          </div>
        </>
      )}
    </>
  )
}
