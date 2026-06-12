import { useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowLeftRight, AlertTriangle, Check, CheckCircle2, ChevronRight, Copy, ExternalLink, Globe2, Link2, Loader2, Plug, RotateCw, Save, Terminal, Webhook } from 'lucide-react'
import { useAction, useGitHubAuth, useGitHubWebhookStatus, useIngressStatus, useSettings, useUiData } from '../lib/query'
import { apiPost } from '../lib/api'
import { confirmAction } from '../lib/confirm'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { ErrorNote, Loading, SourceIcon } from '../components/ui'
import { ConfigField, SettingsSection, useConfigDraft } from '../components/SettingsPanels'
import { GitHubConnect } from '../components/GitHubConnect'
import { SlackConnect } from '../components/SlackConnect'
import { CONNECTORS, CONNECTOR_CATEGORIES, type ConnectorDef } from '../lib/connectors'
import type { IngressStatus, SettingField, ToolCapability } from '../lib/types'
import { pushToast } from '../lib/toast'

type Badge = { dot: string; label: string }

// Maps a live integration capability to a status pill. Mirrors the chip tones
// used elsewhere (running = green, waiting = amber, stale = red, idle = grey).
function capBadge(cap?: ToolCapability): Badge {
  if (!cap) return { dot: 'idle', label: 'unknown' }
  if (cap.available) return { dot: 'running', label: cap.status || 'connected' }
  const s = cap.status || 'not configured'
  const dot =
    s === 'connecting' || s === 'configured'
      ? 'waiting'
      : s === 'inactive' || s === 'not authenticated' || s === 'not installed'
      ? 'stale'
      : 'idle'
  return { dot, label: s }
}

function ingressBadge(ing?: IngressStatus): Badge {
  if (!ing || ing.provider === 'none') return { dot: 'idle', label: 'off' }
  if (ing.last_error) return { dot: 'stale', label: 'error' }
  if (ing.running) return { dot: 'running', label: 'live' }
  if (ing.provider === 'zrok' && ing.env_enabled === false) return { dot: 'idle', label: 'not enabled' }
  return { dot: 'waiting', label: 'configured' }
}

function StatusPill({ badge }: { badge: Badge }) {
  return (
    <span className="env-pill" title="Connector status">
      <span className={`dot ${badge.dot}`} />
      {badge.label}
    </span>
  )
}

function Glyph({ def, size = 16 }: { def: ConnectorDef; size?: number }) {
  return <span className="connector-glyph">{def.source ? <SourceIcon source={def.source} size={size} /> : <Plug size={size} />}</span>
}

export function Connectors() {
  useDocumentTitle('Connectors')
  const { data: ui, isLoading, error } = useUiData()
  const { data: settings } = useSettings()
  const { data: ingress } = useIngressStatus()
  const { data: ghAuth } = useGitHubAuth()
  const cfg = useConfigDraft()
  const [openId, setOpenId] = useState<string | null>(null)

  const integrations = ui?.CAPABILITIES?.integrations ?? []
  const fields = useMemo(() => settings?.fields ?? [], [settings?.fields])

  const badgeFor = (def: ConnectorDef): Badge => {
    if (def.id === 'ingress') return ingressBadge(ingress)
    return capBadge(integrations.find((c) => c.id === def.capabilityId))
  }

  // At-a-glance detail under a tile (currently the active GitHub login).
  const detailFor = (def: ConnectorDef): string | undefined =>
    def.id === 'github' && ghAuth?.active_login ? `@${ghAuth.active_login}` : undefined

  if (isLoading) return <div className="page"><Loading /></div>
  if (error) return <div className="page"><ErrorNote error={error} /></div>

  const openDef = openId ? CONNECTORS.find((c) => c.id === openId) ?? null : null

  return (
    <div className="page">
      <div className="page-head mc-head">
        <div>
          <div className="eyebrow">mission control</div>
          <h1 className="h-xl">Connectors</h1>
          <div className="page-sub">Authenticate external systems — pick a tile to set one up.</div>
        </div>
        <div className="spacer" />
        <div className="mc-env-pills">
          {CONNECTORS.map((def) => {
            const b = badgeFor(def)
            return (
              <span key={def.id} className={`env-pill${b.dot === 'running' ? '' : ' off'}`} title={`${def.label}: ${b.label}`}>
                <span className={`dot ${b.dot}`} />
                {def.source ? <SourceIcon source={def.source} size={12} /> : <Plug size={12} />}
                {def.label}
              </span>
            )
          })}
        </div>
      </div>

      {CONNECTOR_CATEGORIES.map((cat) => {
        const defs = CONNECTORS.filter((c) => c.category === cat.id)
        if (defs.length === 0) return null
        return (
          <SettingsSection key={cat.id} title={cat.label} hint={cat.blurb}>
            <div className="connector-tiles">
              {defs.map((def) => (
                <ConnectorTile key={def.id} def={def} badge={badgeFor(def)} detail={detailFor(def)} onOpen={() => setOpenId(def.id)} />
              ))}
            </div>
            {cat.planned && <div className="connector-planned">{cat.planned}</div>}
          </SettingsSection>
        )
      })}

      {openDef && (
        <ConnectorModal
          def={openDef}
          fields={fields.filter((f) => f.connector === openDef.id)}
          ingress={ingress}
          badge={badgeFor(openDef)}
          cfg={cfg}
          onClose={() => setOpenId(null)}
        />
      )}
    </div>
  )
}

// Compact entry tile — glyph, label, live status, one-line purpose, and an
// optional at-a-glance detail (e.g. the active GitHub login). Click to open the
// setup popup.
function ConnectorTile({ def, badge, detail, onOpen }: { def: ConnectorDef; badge: Badge; detail?: string; onOpen: () => void }) {
  return (
    <button type="button" className="connector-tile" onClick={onOpen}>
      <div className="connector-tile-head">
        <Glyph def={def} />
        <span className="connector-tile-label">{def.label}</span>
        <span className="spacer" />
        <StatusPill badge={badge} />
      </div>
      <div className="connector-tile-powers">{def.powers}</div>
      <div className="connector-tile-foot">
        {detail && <span className="connector-tile-detail mono">{detail}</span>}
        <span className="spacer" />
        <span className="connector-tile-cta">
          Set up <ChevronRight size={13} />
        </span>
      </div>
    </button>
  )
}

// Setup popup. Reuses the app's modal chrome (.scrim/.modal) so the panel,
// scrim, animation, and scroll behaviour match every other dialog — only the
// header carries the connector identity instead of the flow mark.
function ConnectorModal({
  def,
  fields,
  ingress,
  badge,
  cfg,
  onClose,
}: {
  def: ConnectorDef
  fields: SettingField[]
  ingress?: IngressStatus
  badge: Badge
  cfg: ReturnType<typeof useConfigDraft>
  onClose: () => void
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div className="scrim" onMouseDown={onClose}>
      <div className="modal connector-modal" style={{ width: 620 }} onMouseDown={(e) => e.stopPropagation()} role="dialog" aria-modal="true">
        <div className="modal-head">
          <div className="connector-modal-title">
            <Glyph def={def} size={18} />
            <span className="h-lg">{def.label}</span>
            <StatusPill badge={badge} />
          </div>
          <button className="btn icon ghost sm" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>
        <div className="modal-body">
          <ConnectorForm def={def} fields={fields} ingress={ingress} cfg={cfg} />
        </div>
      </div>
    </div>
  )
}

// The setup form: purpose line, the connector's own auth flow, derived callback
// URLs, and the editable config fields with a Save button.
function ConnectorForm({
  def,
  fields,
  ingress,
  cfg,
}: {
  def: ConnectorDef
  fields: SettingField[]
  ingress?: IngressStatus
  cfg: ReturnType<typeof useConfigDraft>
}) {
  const primary = fields.filter((f) => f.type !== 'secret')
  const advanced = fields.filter((f) => f.type === 'secret')
  const dirty = Object.keys(cfg.changesFor(fields)).length > 0

  return (
    <div className="connector-form">
      <div className="connector-powers">{def.powers}</div>

      <ConnectorSetup def={def} />

      {def.id === 'ingress' && <IngressDetail ingress={ingress} />}

      {fields.length > 0 && (
        <div className="connector-config">
          {primary.length > 0 && (
            <div className="config-form">
              {primary.map((f) => (
                <ConfigField key={f.key} field={f} draft={cfg.draft[f.key]} onChange={(v) => cfg.setField(f.key, v)} />
              ))}
            </div>
          )}
          {advanced.length > 0 && (
            <details className="connector-advanced">
              <summary>Manual tokens{def.id === 'slack' ? ' (set by the wizard)' : ''}</summary>
              <div className="config-form">
                {advanced.map((f) => (
                  <ConfigField key={f.key} field={f} draft={cfg.draft[f.key]} onChange={(v) => cfg.setField(f.key, v)} />
                ))}
              </div>
            </details>
          )}
          <div className="config-actions">
            <button type="button" className="btn primary" disabled={!dirty || cfg.isPending} onClick={() => cfg.save(fields)}>
              {cfg.isPending ? <Loader2 size={14} className="spin" /> : <Save size={14} />}
              Save {def.label}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// Per-connector setup/auth area. Each connector has its own auth method; the
// rest of the form (config, derived URLs) is shared. Ingress has no auth step —
// its config + IngressDetail cover it.
function ConnectorSetup({ def }: { def: ConnectorDef }) {
  if (def.id === 'slack') return <SlackConnect framed={false} />
  if (def.id === 'github') {
    // The App-manifest wizard is the primary path (App auth + auto webhook
    // config). Live webhook transport status sits below it; the gh-CLI identity
    // panel powers event polling (polling transport) and branch→PR linking
    // (always), and labels itself accordingly.
    return (
      <>
        <GitHubConnect framed={false} />
        <GitHubWebhookTransport />
        <GitHubAuth />
      </>
    )
  }
  return null
}

// GitHubWebhookTransport surfaces the live webhook ingress state: which transport
// is active, whether a signing secret is configured, the URL to register, and
// whether deliveries are arriving. Shown regardless of gh-auth state because
// webhook mode does not use gh.
function GitHubWebhookTransport() {
  const { data: wh } = useGitHubWebhookStatus()
  if (!wh) return null
  const webhooky = wh.transport === 'webhook' || wh.transport === 'hybrid'
  let tone: 'ok' | 'warn' | 'error' | '' = ''
  if (webhooky) {
    if (!wh.secret_configured) tone = 'warn'
    else if (wh.last_status === 'error') tone = 'error'
    else if (wh.receiving) tone = 'ok'
  }
  return (
    <div className={`connector-auth${tone === 'ok' ? ' ok' : ''}`}>
      <span className="connector-auth-icon">
        {tone === 'warn' || tone === 'error' ? <AlertTriangle size={15} /> : <Webhook size={15} />}
      </span>
      <div className="connector-auth-main">
        <div>Transport: <strong>{wh.transport}</strong></div>
        <div className="connector-auth-src">{wh.summary}</div>
        {webhooky && wh.webhook_url && <div className="connector-auth-path mono">{wh.webhook_url}</div>}
      </div>
    </div>
  )
}

// GitHubAuth surfaces the live `gh` CLI identity and an account switcher. The
// `gh` CLI serves two distinct roles depending on transport: under polling it
// fetches your assigned/mentioned issues & PRs, but under webhook those arrive
// over the webhook above — so the CLI is used ONLY to link PRs you open on task
// branches (GitHub sends no webhook for a self-authored PR). The label reflects
// which role is active so it never implies event polling that isn't happening.
function GitHubAuth() {
  const { data: gh } = useGitHubAuth()
  const { data: wh } = useGitHubWebhookStatus()
  const qc = useQueryClient()
  const [busy, setBusy] = useState<string | null>(null)
  const webhooky = wh?.transport === 'webhook' || wh?.transport === 'hybrid'

  if (!gh) {
    return (
      <div className="connector-auth">
        <span className="connector-auth-icon"><Loader2 size={15} className="spin" /></span>
        <div>Checking the <code>gh</code> CLI…</div>
      </div>
    )
  }
  if (!gh.installed) {
    return (
      <div className="connector-auth">
        <span className="connector-auth-icon"><Terminal size={15} /></span>
        <div>The <code>gh</code> CLI isn't on PATH. Install it, then run <code>gh auth login</code> to connect.</div>
      </div>
    )
  }
  if (!gh.authenticated) {
    return (
      <div className="connector-auth">
        <span className="connector-auth-icon"><Terminal size={15} /></span>
        <div>Not authenticated. Run <code>gh auth login</code> in your terminal, then flip on GitHub polling below.</div>
      </div>
    )
  }

  const switchTo = async (login: string) => {
    setBusy(login)
    try {
      await apiPost('/api/github/auth/switch', { login })
      pushToast('ok', `Switched to @${login}`)
      await Promise.all([
        qc.invalidateQueries({ queryKey: ['github-auth'] }),
        qc.invalidateQueries({ queryKey: ['ui-data'] }),
      ])
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : 'switch failed')
    } finally {
      setBusy(null)
    }
  }

  const canSwitch = gh.accounts.length > 1 && !gh.env_pinned

  return (
    <div className="connector-auth ok">
      <span className="connector-auth-icon"><Check size={15} /></span>
      <div className="connector-auth-main">
        <div>
          {webhooky ? 'PR linking' : 'Polling'} as <strong>@{gh.active_login}</strong> via the <code>gh</code> CLI
          {gh.active_source && <span className="connector-auth-src"> ({gh.active_source})</span>}.
        </div>
        {webhooky && (
          <div className="connector-auth-src">
            Events arrive over the webhook above — the <code>gh</code> CLI is used only to link PRs you open on task branches.
          </div>
        )}
        {gh.path && <div className="connector-auth-path mono">{gh.path}</div>}

        {gh.accounts.length > 1 && (
          <div className="gh-accounts">
            {gh.accounts.map((a) => (
              <div key={a.login} className={`gh-account${a.active ? ' active' : ''}`}>
                <span className="gh-account-login mono">@{a.login}</span>
                {a.active ? (
                  <span className="gh-account-tag">active</span>
                ) : canSwitch ? (
                  <button type="button" className="btn sm" disabled={busy !== null} onClick={() => switchTo(a.login)}>
                    {busy === a.login ? <Loader2 size={13} className="spin" /> : <ArrowLeftRight size={13} />}
                    Switch
                  </button>
                ) : null}
              </div>
            ))}
          </div>
        )}

        {gh.env_pinned && gh.accounts.length > 1 && (
          <div className="gh-pinned-note">
            The active account is pinned by the <code>{gh.active_source}</code> environment variable, which overrides
            the others — unset it to switch accounts here.
          </div>
        )}
      </div>
    </div>
  )
}

function ingressRuntimeState(ingress: IngressStatus): 'online' | 'failed' | 'waiting' | 'off' {
  if (ingress.running) return 'online'
  if (ingress.last_error) return 'failed'
  if (ingress.provider === 'none') return 'off'
  return 'waiting'
}
function ingressRuntimeLabel(ingress: IngressStatus): string {
  const state = ingressRuntimeState(ingress)
  if (state === 'online') return 'public URL live'
  if (state === 'failed') return 'share failed'
  if (state === 'off') return 'off'
  return 'waiting for URL'
}

// Public-ingress runtime panel: live provider/URL/secret state plus webhook
// secret reveal/rotate and public-URL rotation. The editable provider/share
// settings render below as ordinary config fields; this panel is the runtime
// view + the actions that mutate gh-webhook credentials.
function IngressDetail({ ingress }: { ingress?: IngressStatus }) {
  const action = useAction()
  if (!ingress) return null

  const copyToClipboard = async (value: string, label: string) => {
    try {
      await navigator.clipboard.writeText(value)
      pushToast('ok', `${label} copied to clipboard`)
    } catch {
      pushToast('error', 'Could not copy — the value is in ~/.flow/config.json')
    }
  }
  const copySecret = () =>
    action.mutate({ kind: 'reveal-webhook-secret' }, { onSuccess: (data) => data.output && copyToClipboard(data.output, 'Webhook secret') })
  const rotateSecret = async () => {
    const ok = await confirmAction({
      title: 'Rotate GitHub webhook secret?',
      body: 'A new signing secret is generated, saved, and copied to your clipboard now. You must paste it into your GitHub webhook settings afterward — until you do, GitHub deliveries will fail signature verification.',
      confirmLabel: 'Rotate secret',
      danger: true,
    })
    if (ok)
      action.mutate({ kind: 'rotate-webhook-secret' }, { onSuccess: (data) => data.output && copyToClipboard(data.output, 'New webhook secret') })
  }
  const rotateUrl = async () => {
    const ok = await confirmAction({
      title: 'Rotate public callback URL?',
      body: 'A new zrok reserved share (and public URL) is created and the old one released. You must update the Payload URL in your GitHub webhook to the new URL once it appears.',
      confirmLabel: 'Rotate URL',
      danger: true,
    })
    if (ok) action.mutate({ kind: 'rotate-ingress-url' })
  }

  const state = ingressRuntimeState(ingress)
  return (
    <div className={`ingress-runtime ${state}`}>
      <div className="ingress-runtime-head">
        <div className="setting-label">Runtime status</div>
        <span className={`ingress-state ${state}`}>
          {ingress.running ? <CheckCircle2 size={13} /> : ingress.last_error ? <AlertTriangle size={13} /> : <Globe2 size={13} />}
          {ingressRuntimeLabel(ingress)}
        </span>
      </div>
      <div className="ingress-kv">
        <IngressRuntimeValue label="Provider" value={ingress.provider || 'none'} />
        {ingress.provider === 'zrok' && (
          <IngressRuntimeValue label="zrok env" value={ingress.env_enabled ? 'enabled' : 'not enabled'} warn={!ingress.env_enabled} />
        )}
        {ingress.share_name && <IngressRuntimeValue label="Share" value={ingress.share_name} mono />}
        {ingress.base_url ? (
          <IngressRuntimeLink label="Public URL" value={ingress.base_url} />
        ) : (
          ingress.provider !== 'none' && <IngressRuntimeValue label="Public URL" value="not created yet" />
        )}
        {ingress.github_webhook_url && <IngressRuntimeLink label="GitHub webhook" value={ingress.github_webhook_url} />}
        {ingress.provider !== 'none' && (
          <IngressRuntimeValue label="Webhook secret" value={ingress.webhook_secret_set ? 'configured' : 'not set yet'} warn={!ingress.webhook_secret_set} />
        )}
        {ingress.last_error && <IngressRuntimeValue label="Last error" value={ingress.last_error} mono warn />}
      </div>
      {ingress.provider === 'none' ? (
        <div className="config-help">No public ingress. Slack OAuth stays local; GitHub webhooks need a provider below (zrok, or your own URL).</div>
      ) : (
        <div className="ingress-actions">
          {ingress.webhook_secret_set && (
            <button type="button" className="btn ghost sm" onClick={copySecret} disabled={action.isPending} title="Copy the GitHub webhook signing secret to paste into GitHub">
              <Copy size={13} /> Copy webhook secret
            </button>
          )}
          <button type="button" className="btn ghost sm" onClick={rotateSecret} disabled={action.isPending} title="Generate a fresh GitHub webhook signing secret">
            <RotateCw size={13} /> Rotate webhook secret
          </button>
          {ingress.provider === 'zrok' && (
            <button type="button" className="btn ghost sm" onClick={rotateUrl} disabled={action.isPending} title="Generate a fresh public callback URL">
              <RotateCw size={13} /> Rotate public URL
            </button>
          )}
        </div>
      )}
    </div>
  )
}

function IngressRuntimeValue({ label, value, mono = false, warn = false }: { label: string; value: string; mono?: boolean; warn?: boolean }) {
  return (
    <div className="ingress-row">
      <div className="setting-label">{label}</div>
      <div className={`ingress-value${mono ? ' mono' : ''}${warn ? ' warn' : ''}`} title={value}>{value}</div>
    </div>
  )
}

function IngressRuntimeLink({ label, value }: { label: string; value: string }) {
  return (
    <div className="ingress-row">
      <div className="setting-label">{label}</div>
      <a className="ingress-value link mono" href={value} target="_blank" rel="noreferrer" title={value}>
        <Link2 size={12} />
        <span>{value}</span>
        <ExternalLink size={11} />
      </a>
    </div>
  )
}
