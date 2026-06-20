import type { SettingField } from './types'

// Frontend connector registry. The backend owns which settings belong to which
// connector (SettingField.category / .connector); this module owns the *display*
// concerns the API shouldn't dictate: category order + copy, per-connector
// labels, brand glyph, the live-capability id to read status from, and the
// one-line "what it powers in Flow" blurb. Adding Bitbucket/GitLab or another
// messaging provider is an entry here plus backend metadata — never a new
// branch in Connectors.tsx.

export interface ConnectorCategory {
  /** Matches SettingField.category emitted by /api/settings. */
  id: string
  label: string
  blurb: string
  /** Muted footnote naming providers planned but not yet implemented. */
  planned?: string
}

export interface ConnectorDef {
  /** Matches SettingField.connector emitted by /api/settings. */
  id: string
  category: string
  label: string
  /** Brand glyph id understood by <SourceIcon>; omitted → a fallback icon. */
  source?: 'slack' | 'github'
  /** id in CAPABILITIES.integrations to read live status from (e.g. gh, slack). */
  capabilityId?: string
  /** One line: what this connector powers in Flow. */
  powers: string
  /** Not yet implemented — renders as a muted, non-interactive "coming soon" tile. */
  soon?: boolean
}

// Display order is array order.
export const CONNECTOR_CATEGORIES: ConnectorCategory[] = [
  {
    id: 'messaging',
    label: 'Messaging',
    blurb: 'Human conversation — reactions become sessions; threads route to the inbox and attention router.',
  },
  {
    id: 'git',
    label: 'Git',
    blurb: 'Repositories, issues, PRs, and reviews — assigned and mentioned items route to task inboxes.',
  },
  {
    id: 'ticketing',
    label: 'Ticketing',
    blurb: 'Issue trackers and project tools — assigned tickets and mentions route to task inboxes.',
  },
  {
    id: 'network',
    label: 'Network',
    blurb: 'Public ingress and keep-awake controls for connector callbacks, Slack Socket Mode, and GitHub webhooks.',
  },
]

export const CONNECTORS: ConnectorDef[] = [
  {
    id: 'slack',
    category: 'messaging',
    label: 'Slack',
    source: 'slack',
    capabilityId: 'slack',
    powers: 'Reaction-triggered sessions, DM/thread following, and inbox + attention routing.',
  },
  {
    id: 'github',
    category: 'git',
    label: 'GitHub',
    source: 'github',
    capabilityId: 'gh',
    powers: 'A GitHub App delivers assigned/mentioned issues & PRs and review requests over signed webhooks into task inboxes.',
  },
  // ---- coming soon (display-only placeholders; no backend wiring yet) ----
  {
    id: 'teams',
    category: 'messaging',
    label: 'Microsoft Teams',
    powers: 'Channel and chat messages route to the inbox and attention router.',
    soon: true,
  },
  {
    id: 'mattermost',
    category: 'messaging',
    label: 'Mattermost',
    powers: 'Self-hosted team chat — messages route to the inbox and attention router.',
    soon: true,
  },
  {
    id: 'rocketchat',
    category: 'messaging',
    label: 'Rocket.Chat',
    powers: 'Open-source team chat — messages route to the inbox and attention router.',
    soon: true,
  },
  {
    id: 'gitlab',
    category: 'git',
    label: 'GitLab',
    powers: 'Merge requests, issues, and reviews route to task inboxes.',
    soon: true,
  },
  {
    id: 'bitbucket',
    category: 'git',
    label: 'Bitbucket',
    powers: 'Pull requests and issues route to task inboxes.',
    soon: true,
  },
  {
    id: 'jira',
    category: 'ticketing',
    label: 'Jira',
    powers: 'Assigned issues and mentions route to task inboxes.',
    soon: true,
  },
  {
    id: 'linear',
    category: 'ticketing',
    label: 'Linear',
    powers: 'Assigned issues and mentions route to task inboxes.',
    soon: true,
  },
  {
    id: 'asana',
    category: 'ticketing',
    label: 'Asana',
    powers: 'Assigned tasks and mentions route to task inboxes.',
    soon: true,
  },
  {
    id: 'clickup',
    category: 'ticketing',
    label: 'ClickUp',
    powers: 'Assigned tasks and mentions route to task inboxes.',
    soon: true,
  },
  {
    id: 'ingress',
    category: 'network',
    label: 'Public ingress',
    capabilityId: undefined,
    powers: 'A public HTTPS base URL plus an opt-in keep-awake toggle for always-on connector delivery.',
  },
]

export function connectorById(id: string): ConnectorDef | undefined {
  return CONNECTORS.find((c) => c.id === id)
}

// Groups the live settings fields by connector, preserving registry order and
// dropping connectors that have no fields. Fields without a connector tag are
// ignored here — those stay on the Settings page.
export function connectorsInCategory(category: string, fields: SettingField[]): { def: ConnectorDef; fields: SettingField[] }[] {
  return CONNECTORS.filter((c) => c.category === category).map((def) => ({
    def,
    fields: fields.filter((f) => f.connector === def.id),
  }))
}
