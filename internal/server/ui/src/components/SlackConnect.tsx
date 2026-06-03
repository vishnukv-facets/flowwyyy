import { useState } from 'react'
import { Check, ExternalLink, Loader2, RefreshCw, Slack, X } from 'lucide-react'
import { apiPost } from '../lib/api'
import { useSlackSetupStatus } from '../lib/query'
import { pushToast } from '../lib/toast'
import type { SlackSetupStatus } from '../lib/types'

// Connect-Slack wizard. Three steps, each resumable from server state — the
// page can be reloaded at any point and the wizard re-derives where you are:
//
//   1. config token  →  flow creates the Slack app via apps.manifest.create
//   2. app-level token (xapp-) — Slack has no API for this one; deep-linked paste
//   3. install — real OAuth through an ephemeral https://localhost callback
//
// Step state lives entirely in GET /api/slack/setup/status.

type StepKey = 'app' | 'token' | 'install'

function deriveStep(st: SlackSetupStatus): StepKey | 'done' {
  if (!st.app_created) return 'app'
  if (!st.app_token_set) return 'token'
  if (!st.bot_token_set) return 'install'
  return 'done'
}

export function SlackConnect() {
  const { data: st, refetch } = useSlackSetupStatus()
  if (!st) return null

  // A pre-wizard manual setup (tokens in env/config but no managed app):
  // don't walk the user backwards through app creation — they're connected.
  const manualSetup = !st.app_created && st.app_token_set && st.bot_token_set

  const step = deriveStep(st)

  return (
    <section className="settings-card slack-wizard">
      <div className="settings-card-head">
        <span><Slack size={17} /></span>
        <h2>Connect Slack</h2>
        <span className="spacer" />
        <ListenerChip st={st} />
      </div>
      <div className="settings-card-body">
        {manualSetup ? (
          <div className="slack-wizard-done">
            <Check size={15} />
            <div>
              Slack is configured from hand-entered tokens. The wizard can take over
              by creating a flow-managed app — paste a config token below if you
              want that; otherwise there's nothing to do.
            </div>
          </div>
        ) : step === 'done' ? (
          <FinishedSummary st={st} onRefetch={refetch} />
        ) : null}

        {(step !== 'done' || !st.bot_token_set) && !manualSetup && (
          <div className="slack-wizard-steps">
            <StepCreateApp st={st} active={step === 'app'} onDone={refetch} />
            <StepAppToken st={st} active={step === 'token'} onDone={refetch} />
            <StepInstall st={st} active={step === 'install'} onDone={refetch} />
          </div>
        )}
      </div>
    </section>
  )
}

function ListenerChip({ st }: { st: SlackSetupStatus }) {
  let label = 'not configured'
  let cls = 'idle'
  if (st.listener_suppressed) {
    label = 'suppressed'
    cls = 'stale'
  } else if (st.listener_connected) {
    label = 'connected'
    cls = 'running'
  } else if (st.listener_running) {
    label = 'connecting'
    cls = 'waiting'
  } else if (st.bot_token_set && st.app_token_set) {
    label = 'configured'
    cls = 'waiting'
  }
  return (
    <span className="env-pill" title="Socket Mode listener state">
      <span className={`dot ${cls}`} />
      {label}
    </span>
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

function StepCreateApp({ st, active, onDone }: { st: SlackSetupStatus; active: boolean; onDone: () => void }) {
  const [token, setToken] = useState('')
  const [busy, setBusy] = useState(false)

  const create = async () => {
    if (!token.trim()) return
    setBusy(true)
    try {
      await apiPost('/api/slack/setup/create-app', { config_token: token.trim() })
      setToken('')
      pushToast('ok', 'Slack app created')
      onDone()
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : 'create app failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <StepShell
      index={1}
      title="Create the Slack app"
      state={st.app_created ? 'done' : active ? 'active' : 'pending'}
      summary={
        st.manage_url && (
          <a className="slack-step-link" href={st.manage_url} target="_blank" rel="noreferrer">
            {st.app_id} <ExternalLink size={11} />
          </a>
        )
      }
    >
      <p className="config-help">
        Mint an <strong>app configuration token</strong> at{' '}
        <a href="https://api.slack.com/apps" target="_blank" rel="noreferrer">
          api.slack.com/apps <ExternalLink size={11} />
        </a>{' '}
        — scroll to “Your App Configuration Tokens”, Generate, copy the access token
        (<code>xoxe.xoxp-…</code>, lives 12 h). flow uses it once to create a fully
        configured app: scopes, events, and Socket Mode in one shot.
      </p>
      <div className="slack-step-controls">
        <input
          className="input mono"
          type="password"
          autoComplete="off"
          placeholder="xoxe.xoxp-…"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && create()}
        />
        <button type="button" className="btn primary" disabled={busy || !token.trim()} onClick={create}>
          {busy ? <Loader2 size={14} className="spin" /> : null}
          Create app
        </button>
      </div>
    </StepShell>
  )
}

function StepAppToken({ st, active, onDone }: { st: SlackSetupStatus; active: boolean; onDone: () => void }) {
  const [token, setToken] = useState('')
  const [busy, setBusy] = useState(false)

  const save = async () => {
    if (!token.trim()) return
    setBusy(true)
    try {
      await apiPost('/api/slack/setup/app-token', { app_token: token.trim() })
      setToken('')
      pushToast('ok', 'app-level token verified')
      onDone()
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : 'token check failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <StepShell
      index={2}
      title="App-level token (Socket Mode)"
      state={st.app_token_set ? 'done' : active ? 'active' : 'pending'}
    >
      <p className="config-help">
        Slack offers no API for this one. Open{' '}
        {st.app_token_url ? (
          <a href={st.app_token_url} target="_blank" rel="noreferrer">
            your app's Basic Information page <ExternalLink size={11} />
          </a>
        ) : (
          <span>your app's Basic Information page</span>
        )}{' '}
        → <strong>App-Level Tokens → Generate</strong> with the{' '}
        <code>connections:write</code> scope, then paste the <code>xapp-…</code> token.
        flow verifies it against Slack before saving.
      </p>
      <div className="slack-step-controls">
        <input
          className="input mono"
          type="password"
          autoComplete="off"
          placeholder="xapp-…"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && save()}
        />
        <button type="button" className="btn primary" disabled={busy || !token.trim()} onClick={save}>
          {busy ? <Loader2 size={14} className="spin" /> : null}
          Verify &amp; save
        </button>
      </div>
    </StepShell>
  )
}

function StepInstall({ st, active, onDone }: { st: SlackSetupStatus; active: boolean; onDone: () => void }) {
  const [busy, setBusy] = useState(false)

  const start = async () => {
    setBusy(true)
    try {
      const res = await apiPost<{ authorize_url: string }>('/api/slack/setup/oauth/start', {})
      window.open(res.authorize_url, '_blank', 'noopener')
      onDone()
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : 'could not start the install')
    } finally {
      setBusy(false)
    }
  }

  const cancel = async () => {
    try {
      await apiPost('/api/slack/setup/oauth/cancel', {})
    } finally {
      onDone()
    }
  }

  return (
    <StepShell
      index={3}
      title="Install to your workspace"
      state={st.bot_token_set ? 'done' : active ? 'active' : 'pending'}
      summary={st.self_user_ids ? <span className="slack-step-link mono">{st.self_user_ids}</span> : undefined}
    >
      <p className="config-help">
        One browser approval. Slack hands back the bot token, your user token
        (DM following), and your member ID in a single round trip — nothing to
        copy. The redirect lands on <code>{st.redirect_url}</code> with a
        locally-generated certificate, so your browser will warn once —{' '}
        <strong>Advanced → Proceed</strong> is expected; the code never leaves
        this machine.
      </p>
      {st.oauth_active ? (
        <div className="slack-step-controls">
          <span className="slack-wait">
            <Loader2 size={14} className="spin" /> waiting for you to approve in Slack…
          </span>
          {st.oauth_authorize_url && (
            <a className="btn" href={st.oauth_authorize_url} target="_blank" rel="noreferrer">
              Reopen approval page <ExternalLink size={12} />
            </a>
          )}
          <button type="button" className="btn" onClick={cancel}>
            <X size={13} /> Cancel
          </button>
        </div>
      ) : (
        <div className="slack-step-controls">
          <button type="button" className="btn primary" disabled={busy} onClick={start}>
            {busy ? <Loader2 size={14} className="spin" /> : <Slack size={14} />}
            Install — opens Slack
          </button>
          {st.oauth_status === 'error' && st.oauth_error && (
            <span className="slack-error">{st.oauth_error}</span>
          )}
        </div>
      )}
    </StepShell>
  )
}

function FinishedSummary({ st, onRefetch }: { st: SlackSetupStatus; onRefetch: () => void }) {
  const [busy, setBusy] = useState(false)

  const reinstall = async () => {
    setBusy(true)
    try {
      const res = await apiPost<{ authorize_url: string }>('/api/slack/setup/oauth/start', {})
      window.open(res.authorize_url, '_blank', 'noopener')
      onRefetch()
    } catch (err) {
      pushToast('error', err instanceof Error ? err.message : 'could not start the reinstall')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="slack-wizard-done">
      <Check size={15} />
      <div>
        Slack is connected{st.oauth_team ? <> to <strong>{st.oauth_team}</strong></> : null}
        {st.self_user_ids ? <> as <code>{st.self_user_ids}</code></> : null}. React to any
        message with <code>:claude:</code> to spawn a session.
        {!st.user_token_set && (
          <div className="slack-error">
            No user token came back — DM following won't work. Reinstall and approve the
            user-scope prompt.
          </div>
        )}
      </div>
      <span className="spacer" />
      <button
        type="button"
        className="btn"
        disabled={busy}
        onClick={reinstall}
        title="Re-run the OAuth install (needed after scope changes)"
      >
        {busy ? <Loader2 size={14} className="spin" /> : <RefreshCw size={13} />}
        Reinstall
      </button>
    </div>
  )
}
