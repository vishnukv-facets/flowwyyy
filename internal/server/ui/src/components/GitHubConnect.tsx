import { useState } from 'react'
import { AlertTriangle, Building2, Check, ExternalLink, Github, Globe2, Loader2, RefreshCw, Unplug, User } from 'lucide-react'
import { ApiError, apiPost } from '../lib/api'
import { confirmAction } from '../lib/confirm'
import { useGitHubInstallations, useGitHubSetupStatus } from '../lib/query'
import { pushToast } from '../lib/toast'
import type { GitHubInstallation, GitHubSetupStatus } from '../lib/types'

// Connect-GitHub wizard. Built on GitHub's App-manifest flow — one click
// registers a GitHub App, captures its credentials, and wires Flow for
// webhook-first ingress with no manual secret entry. Resumable from server
// state (GET /api/github/setup/status), so the page can be reloaded at any
// point and the wizard re-derives where you are:
//
//   0. ingress   — a public HTTPS base URL must exist first (the App's webhook
//      and the manifest redirect both need it)
//   1. create    — POST the App manifest to github.com; on confirm GitHub
//      redirects back and Flow converts the code into app id + PEM + webhook
//      secret (PEM/secrets land in the OS keyring)
//   2. install   — install the App; the post-install redirect carries the
//      installation id Flow needs to mint tokens
//
// Steps 1 + 2 complete in a separate github.com tab; the wizard learns of
// completion by polling status (+ the github-setup WS event).

type StepKey = 'ingress' | 'app' | 'install'

function deriveStep(st: GitHubSetupStatus): StepKey | 'done' {
  if (!st.ingress_ready) return 'ingress'
  if (!st.app_created) return 'app'
  if (!st.installed) return 'install'
  return 'done'
}

// postManifestForm submits the App manifest to github.com as a form POST (the
// manifest flow requires a form field, not a fetch body), opening GitHub's
// "Create GitHub App" confirmation page in a new tab.
function postManifestForm(action: string, manifest: unknown) {
  const form = document.createElement('form')
  form.method = 'POST'
  form.action = action
  form.target = '_blank'
  const input = document.createElement('input')
  input.type = 'hidden'
  input.name = 'manifest'
  input.value = JSON.stringify(manifest)
  form.appendChild(input)
  document.body.appendChild(form)
  form.submit()
  form.remove()
}

export function GitHubConnect({ framed = true }: { framed?: boolean } = {}) {
  const { data: st, refetch } = useGitHubSetupStatus()
  if (!st) return null

  const step = deriveStep(st)

  const body = (
    <>
      {step === 'done' ? <FinishedSummary st={st} onChange={refetch} /> : null}
      <div className="slack-wizard-steps">
        <StepIngress st={st} active={step === 'ingress'} />
        <StepCreateApp st={st} active={step === 'app'} onDone={refetch} />
        <StepInstall st={st} active={step === 'install'} />
      </div>
    </>
  )

  if (!framed) return <div className="slack-wizard slack-wizard-bare">{body}</div>

  return (
    <section className="settings-card slack-wizard">
      <div className="settings-card-head">
        <span><Github size={17} /></span>
        <h2>Connect GitHub</h2>
      </div>
      <div className="settings-card-body">{body}</div>
    </section>
  )
}

function StepShell({
  index,
  title,
  state,
  children,
  summary,
}: {
  index: number
  title: string
  state: 'done' | 'active' | 'pending'
  children?: React.ReactNode
  summary?: React.ReactNode
}) {
  return (
    <div className={`slack-step ${state}`}>
      <div className="slack-step-head">
        <span className="slack-step-badge">{state === 'done' ? <Check size={12} /> : index}</span>
        <span className="slack-step-title">{title}</span>
        {state === 'done' && summary}
      </div>
      {state === 'active' && <div className="slack-step-body">{children}</div>}
    </div>
  )
}

function StepIngress({ st, active }: { st: GitHubSetupStatus; active: boolean }) {
  return (
    <StepShell index={1} title="Public ingress" state={st.ingress_ready ? 'done' : active ? 'active' : 'pending'}>
      <p className="config-help">
        GitHub signs webhook deliveries to a public HTTPS URL, and the App-creation
        redirect lands there too — so a public ingress must be running before you
        create the App. Open the <strong>Public ingress</strong> connector (Network)
        and start it, then come back here.
      </p>
      <div className="slack-step-controls">
        <span className="env-pill">
          <Globe2 size={13} /> {st.ingress_ready ? 'ingress ready' : 'ingress not running'}
        </span>
      </div>
    </StepShell>
  )
}

function StepCreateApp({ st, active, onDone }: { st: GitHubSetupStatus; active: boolean; onDone: () => void }) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)

  const create = async (replaceExisting = false) => {
    setBusy(true)
    try {
      const res = await apiPost<{ create_url: string; manifest: unknown }>('/api/github/setup/create-app', {
        name: name.trim(),
        target: 'user',
        replace_existing: replaceExisting,
      })
      postManifestForm(res.create_url, res.manifest)
      pushToast('ok', 'Opening GitHub to create the App…')
      onDone()
    } catch (err) {
      if (err instanceof ApiError && err.status === 409 && !replaceExisting) {
        const ok = await confirmAction({
          title: 'Replace connected GitHub App?',
          body: `${err.message}. The current App will stop verifying webhooks once its stored credentials are replaced.`,
          confirmLabel: 'Replace App',
          danger: true,
        })
        if (ok) {
          await create(true)
          return
        }
      }
      pushToast('error', err instanceof Error ? err.message : 'could not start App creation')
    } finally {
      setBusy(false)
    }
  }

  return (
    <StepShell
      index={2}
      title="Create the GitHub App"
      state={st.app_created ? 'done' : active ? 'active' : 'pending'}
      summary={
        st.html_url && (
          <a className="slack-step-link" href={st.html_url} target="_blank" rel="noreferrer">
            {st.app_slug || st.app_id} <ExternalLink size={11} />
          </a>
        )
      }
    >
      <p className="config-help">
        Flow builds one public GitHub App <strong>manifest</strong> and hands it to
        GitHub. Create it once here; the next step installs that same App on your
        personal account and any orgs you want Flow to watch.
      </p>
      <div className="slack-step-controls">
        <input
          className="input"
          aria-label="GitHub App name"
          placeholder="App name (optional)"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <button
          type="button"
          className="btn primary"
          disabled={busy || !st.ingress_ready}
          onClick={() => void create()}
          title={!st.ingress_ready ? 'Start public ingress first' : undefined}
        >
          {busy ? <Loader2 size={14} className="spin" /> : <Github size={14} />}
          Create App — opens GitHub
        </button>
      </div>
    </StepShell>
  )
}

// AccountPills renders the accounts the App is installed on — a personal-account
// pill and one per org — so the operator can see "both" coverage at a glance.
function AccountPills({ installs }: { installs: GitHubInstallation[] }) {
  if (installs.length === 0) return null
  return (
    <div className="gh-install-pills">
      {installs.map((i) => (
        <span key={i.id} className="env-pill" title={i.type}>
          {i.type === 'Organization' ? <Building2 size={12} /> : <User size={12} />} {i.account}
        </span>
      ))}
    </div>
  )
}

function StepInstall({ st, active }: { st: GitHubSetupStatus; active: boolean }) {
  // Enabled once the App exists — the installations call authenticates as the App.
  const { data: instData } = useGitHubInstallations(st.app_created)
  const installs = instData?.installations ?? []
  const install = () => {
    if (st.install_url) window.open(st.install_url, '_blank', 'noopener')
  }
  return (
    <StepShell index={3} title="Install the App" state={st.installed ? 'done' : active ? 'active' : 'pending'}>
      <p className="config-help">
        Install the App on every account whose repos Flow should watch — your{' '}
        <strong>personal account and any org</strong>. You can install on more than one:
        pick the account on GitHub, and Flow captures each installation automatically.
      </p>
      <AccountPills installs={installs} />
      <div className="slack-step-controls">
        <button type="button" className="btn primary" disabled={!st.install_url} onClick={install}>
          <Github size={14} /> {installs.length > 0 ? 'Install on another account' : 'Install'} — opens GitHub
        </button>
      </div>
    </StepShell>
  )
}

function FinishedSummary({ st, onChange }: { st: GitHubSetupStatus; onChange: () => void }) {
  const [busy, setBusy] = useState(false)
  const [disconnecting, setDisconnecting] = useState(false)
  const { data: instData } = useGitHubInstallations(true)
  const installs = instData?.installations ?? []
  const installMore = () => {
    if (st.install_url) window.open(st.install_url, '_blank', 'noopener')
  }

  const backfill = async () => {
    setBusy(true)
    try {
      const res = await apiPost<{ replayed: number }>('/api/github/setup/backfill', {})
      pushToast('ok', res.replayed > 0 ? `Replayed ${res.replayed} missed deliver${res.replayed === 1 ? 'y' : 'ies'}` : 'No missed deliveries to replay')
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : 'backfill failed')
    } finally {
      setBusy(false)
    }
  }

  // Disconnect forgets Flow's copy of the App credentials (keyring + config). The
  // App itself stays on github.com — only the operator can delete it there — so
  // the confirm spells that out and the summary links to the App's page.
  const disconnect = async () => {
    const ok = await confirmAction({
      title: 'Disconnect GitHub App?',
      body: `Flow will erase this App's credentials (private key, webhook secret, installation) from this machine and stop receiving webhooks. The App${st.app_slug ? ` "${st.app_slug}"` : ''} still exists on GitHub — open it there to uninstall or delete it for good.`,
      confirmLabel: 'Disconnect',
      danger: true,
    })
    if (!ok) return
    setDisconnecting(true)
    try {
      await apiPost('/api/github/setup/disconnect', {})
      pushToast('ok', 'Disconnected — GitHub App credentials cleared from this machine')
      onChange()
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : 'disconnect failed')
    } finally {
      setDisconnecting(false)
    }
  }

  return (
    <div className="slack-wizard-done">
      {!st.self_logins_set ? (
        <div className="gh-selflogins-warn" role="alert">
          <AlertTriangle size={15} />
          <div>
            <strong>Your GitHub login isn't set.</strong> Until you add it under{' '}
            <em>Your GitHub logins</em> in Settings, Flow drops every webhook event
            as out-of-scope — including your own PRs and issues. Set it so Flow acts
            only on items that involve you.
          </div>
        </div>
      ) : null}
      <div className="slack-wizard-done-head">
        <Check size={15} />
        <div>
          GitHub is connected
          {st.app_slug ? (
            <>
              {' '}as{' '}
              <a href={st.html_url} target="_blank" rel="noreferrer">
                <code>{st.app_slug}</code>
              </a>
            </>
          ) : null}
          . Assigned/mentioned issues &amp; PRs and review requests now arrive over signed
          webhooks — no <code>gh</code> polling.
          {installs.length > 0 ? (
            <div className="gh-install-line">
              Installed on <AccountPills installs={installs} />
            </div>
          ) : null}
        </div>
      </div>
      <div className="slack-wizard-done-actions">
        <button
          type="button"
          className="btn"
          disabled={!st.install_url}
          onClick={installMore}
          title="Install this App on another account or org (e.g. add your org alongside your personal account)"
        >
          <Github size={13} /> Install on another account
        </button>
        <button
          type="button"
          className="btn"
          disabled={busy}
          onClick={backfill}
          title="Replay GitHub webhook deliveries missed while Flow or the public ingress was down"
        >
          {busy ? <Loader2 size={14} className="spin" /> : <RefreshCw size={13} />}
          Replay missed
        </button>
        <button
          type="button"
          className="btn danger"
          disabled={disconnecting}
          onClick={disconnect}
          title="Forget this App's credentials on this machine (the App stays on GitHub until you delete it there)"
        >
          {disconnecting ? <Loader2 size={14} className="spin" /> : <Unplug size={13} />}
          Disconnect
        </button>
      </div>
    </div>
  )
}
