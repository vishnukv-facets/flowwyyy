/**
 * navDefs — shared navigation destination definitions.
 *
 * Both the desktop rail (Shell.tsx) and the mobile bottom nav (MobileNav.tsx)
 * consume the SAME groups array from this module so the destination list is
 * never duplicated.  Badges and tones are passed in at render time because
 * they depend on live query data.
 */
import type { ReactNode } from 'react'
import {
  BarChart3,
  Bell,
  BookText,
  Bot,
  Brain,
  FolderGit2,
  HardDrive,
  Inbox,
  LayoutDashboard,
  ListTodo,
  MessagesSquare,
  Network,
  Plug,
  Repeat,
  Settings,
  TerminalSquare,
  Trash2,
} from 'lucide-react'

export interface NavDef {
  to: string
  label: string
  /** Short label used in the mobile tab bar (≤8 chars keeps it from wrapping) */
  shortLabel?: string
  icon: ReactNode
  match: (p: string) => boolean
  badge?: number
  tone?: string
}

export type NavGroup = { label: string; items: NavDef[] }

/**
 * Build the full nav groups.  Call this inside a component so that icon sizes
 * can be passed in (rail uses 16px; mobile nav uses 20px for touch targets).
 */
export function buildNavGroups(opts: {
  iconSize?: number
  running?: number
  backlog?: number
  unread?: number
  attentionCount?: number
}): NavGroup[] {
  const sz = opts.iconSize ?? 16
  const { running = 0, backlog = 0, unread = 0, attentionCount = 0 } = opts

  return [
    {
      label: 'Workspace',
      items: [
        {
          to: '/',
          label: 'Mission Control',
          shortLabel: 'Home',
          icon: <LayoutDashboard size={sz} />,
          match: (p) => p === '/',
        },
        {
          to: '/sessions',
          label: 'Sessions',
          icon: <TerminalSquare size={sz} />,
          match: (p) => p === '/sessions' || p.startsWith('/session/'),
          badge: running || undefined,
          tone: 'var(--ok)',
        },
        {
          to: '/tasks',
          label: 'Tasks',
          icon: <ListTodo size={sz} />,
          match: (p) => p === '/tasks',
          badge: backlog || undefined,
        },
        {
          to: '/owners',
          label: 'Owners',
          icon: <Bot size={sz} />,
          match: (p) => p === '/owners',
        },
        {
          to: '/graph',
          label: 'Graph',
          icon: <Network size={sz} />,
          match: (p) => p === '/graph' || p === '/brain',
        },
        {
          to: '/inbox',
          label: 'Inbox',
          icon: <Inbox size={sz} />,
          match: (p) => p === '/inbox',
          badge: unread || undefined,
          tone: 'var(--accent-hi)',
        },
        {
          to: '/chats',
          label: 'Chats',
          icon: <MessagesSquare size={sz} />,
          match: (p) => p === '/chats',
        },
        {
          to: '/attention',
          label: 'Attention',
          icon: <Bell size={sz} />,
          match: (p) => p === '/attention',
          badge: attentionCount || undefined,
          tone: 'var(--warn)',
        },
        {
          to: '/analytics',
          label: 'Analytics',
          icon: <BarChart3 size={sz} />,
          match: (p) => p === '/analytics',
        },
      ],
    },
    {
      label: 'Library',
      items: [
        {
          to: '/projects',
          label: 'Projects',
          icon: <FolderGit2 size={sz} />,
          match: (p) => p === '/projects' || p.startsWith('/project/'),
        },
        {
          to: '/playbooks',
          label: 'Playbooks',
          icon: <Repeat size={sz} />,
          match: (p) => p === '/playbooks' || p.startsWith('/playbook/'),
        },
        {
          to: '/kb',
          label: 'Knowledge',
          icon: <BookText size={sz} />,
          match: (p) => p === '/kb',
        },
        {
          to: '/memories',
          label: 'Memories',
          icon: <Brain size={sz} />,
          match: (p) => p === '/memories',
        },
      ],
    },
    {
      label: 'System',
      items: [
        {
          to: '/workdirs',
          label: 'Workdirs',
          icon: <HardDrive size={sz} />,
          match: (p) => p === '/workdirs',
        },
        {
          to: '/connectors',
          label: 'Connectors',
          icon: <Plug size={sz} />,
          match: (p) => p === '/connectors',
        },
        {
          to: '/settings',
          label: 'Settings',
          icon: <Settings size={sz} />,
          match: (p) => p === '/settings',
        },
        {
          to: '/trash',
          label: 'Trash',
          icon: <Trash2 size={sz} />,
          match: (p) => p === '/trash',
        },
      ],
    },
  ]
}
