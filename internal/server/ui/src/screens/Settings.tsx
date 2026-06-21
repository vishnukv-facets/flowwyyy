import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Bell, CheckCircle2, Database, Loader2, MonitorCog, Moon, PlugZap, Save, Settings as SettingsIcon, SlidersHorizontal, Sun } from 'lucide-react'
import { useHealth, useSettings, useUiData } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { getTheme, onThemeChange, toggleTheme, type Theme } from '../lib/theme'
import { ErrorNote, Loading, ProviderIcon, SourceIcon } from '../components/ui'
import { ConfigField, SettingsPanel, SettingsSection, useConfigDraft } from '../components/SettingsPanels'
import { RemoteAccessSettings } from './RemoteAccessSettings'
import type { SettingField, ToolCapability } from '../lib/types'
import { useMascotPrefs, setMascotPrefs, NAP_OPTIONS } from '../lib/mascot'
import { pushToast } from '../lib/toast'

type BrowserNotificationPermission = NotificationPermission | 'unsupported'

function notificationPermission(): BrowserNotificationPermission {
  if (typeof window === 'undefined' || !('Notification' in window)) return 'unsupported'
  return Notification.permission
}

export function Settings() {
  useDocumentTitle('Settings')
  const { data: ui, isLoading, error } = useUiData()
  const { data: health } = useHealth()
  const [theme, setTheme] = useState<Theme>(() => getTheme())
  const [permission, setPermission] = useState<BrowserNotificationPermission>(() => notificationPermission())
  const mascot = useMascotPrefs()

  useEffect(() => onThemeChange(setTheme), [])

  const enableNotifications = async () => {
    if (typeof window === 'undefined' || !('Notification' in window)) {
      setPermission('unsupported')
      return
    }
    setPermission(await Notification.requestPermission())
  }

  // Fire an unconditional test notification (bypassing the in-app focus gate) so
  // the user can verify the OS path end-to-end. If it doesn't appear in
  // Notification Center, the block is at the OS/browser level, not in flow.
  const sendTestNotification = () => {
    if (typeof window === 'undefined' || !('Notification' in window) || Notification.permission !== 'granted') return
    try {
      new Notification('flow: test alert', {
        body: 'Desktop alerts are wired up. If this did not appear as a macOS banner, allow your browser in System Settings → Notifications.',
        tag: 'flow-test-alert',
      })
      pushToast('info', 'Test alert sent — check Notification Center if no banner appeared.')
    } catch {
      pushToast('error', 'Could not post a desktop notification from this browser.')
    }
  }

  if (isLoading) return <div className="page"><Loading rows={5} /></div>
  if (error) return <div className="page"><ErrorNote error={error} /></div>

  const db = ui?.FLOWDB
  const user = ui?.USER
  const caps = ui?.CAPABILITIES
  const headerPills = [...(caps?.providers ?? []), ...(caps?.integrations ?? [])]

  return (
    <div className="page">
      <div className="page-head mc-head">
        <div>
          <div className="eyebrow">workspace</div>
          <h1 className="h-xl">Settings</h1>
          <div className="page-sub">Integrations, agents, and workspace — tuned without touching a shell.</div>
        </div>
        <div className="spacer" />
        <div className="mc-env-pills">
          {headerPills.map((c) => (
            <span key={c.id} className={`env-pill${c.available ? '' : ' off'}`} title={c.reason || c.status || ''}>
              <span className={`dot ${c.available ? 'running' : 'idle'}`} />
              {c.id === 'claude' || c.id === 'codex' ? (
                <ProviderIcon provider={c.id} size={13} />
              ) : (
                <SourceIcon source={c.id === 'gh' ? 'github' : c.id} size={12} />
              )}
              {c.label}
            </span>
          ))}
        </div>
      </div>

      <div className="settings-summary card">
        <SummaryItem label="User" value={user?.full_name || user?.name || user?.username || 'unknown'} />
        <SummaryItem label="Flow root" value={health?.flow_root || '—'} mono />
        <SummaryItem label="Version" value={health?.version || 'dev'} mono />
        <SummaryItem label="Database" value={db?.human_size || '—'} />
        <SummaryItem label="DB status" value={db?.exists ? 'available' : 'missing'} />
      </div>

      <SettingsSection title="Preferences">
        <div className="settings-grid">
          <SettingsPanel title="Appearance & alerts" icon={<SettingsIcon size={17} />}>
            <div className="setting-row">
              <div>
                <div className="setting-label">Theme</div>
                <div className="setting-value">{theme}</div>
              </div>
              <button type="button" className="btn" onClick={() => setTheme(toggleTheme())}>
                {theme === 'dark' ? <Sun size={15} /> : <Moon size={15} />}
                {theme === 'dark' ? 'Light' : 'Dark'}
              </button>
            </div>
            <div className="setting-row">
              <div>
                <div className="setting-label">Desktop alerts</div>
                <div className="setting-value">{permission}</div>
              </div>
              {permission === 'default' && (
                <button type="button" className="btn" onClick={enableNotifications}>
                  <Bell size={15} /> Enable
                </button>
              )}
              {permission === 'granted' && (
                <button type="button" className="btn" onClick={sendTestNotification}>
                  <Bell size={15} /> Send test
                </button>
              )}
            </div>
            {permission === 'granted' && (
              <div className="setting-hint">
                Alerts fire when flow isn't the focused window. No macOS banner after a test?
                The browser itself needs OS permission: System Settings → Notifications →
                your browser → Allow Notifications, and turn off Focus / Do Not Disturb.
              </div>
            )}
            {permission === 'denied' && (
              <div className="setting-hint">
                Blocked in your browser. Re-allow notifications for this site
                (address-bar site settings), then reload.
              </div>
            )}
            {permission === 'unsupported' && (
              <div className="setting-hint">This browser doesn't support desktop notifications.</div>
            )}
          </SettingsPanel>
          <SettingsPanel title="Mascot" icon={<SlidersHorizontal size={17} />}>
            <div className="setting-row">
              <div>
                <div className="setting-label">Sidebar mascot</div>
                <div className="setting-value">{mascot.enabled ? 'on' : 'off'}</div>
              </div>
              <button type="button" className="btn" onClick={() => setMascotPrefs({ enabled: !mascot.enabled })}>
                {mascot.enabled ? 'Hide' : 'Show'}
              </button>
            </div>
            <div className="setting-row">
              <div>
                <div className="setting-label">Naps when idle for</div>
                <div className="setting-value">no activity this long → it sleeps</div>
              </div>
              <select className="input" value={mascot.napSec} onChange={(e) => setMascotPrefs({ napSec: Number(e.target.value) })}>
                {NAP_OPTIONS.map((o) => (
                  <option key={o.sec} value={o.sec}>{o.label}</option>
                ))}
              </select>
            </div>
          </SettingsPanel>
          <SettingsPanel title="Database" icon={<Database size={17} />}>
            <KeyValue label="Path" value={db?.display_path || db?.path || 'unknown'} mono />
            <KeyValue label="Size" value={db?.human_size || 'unknown'} />
            <KeyValue label="Status" value={db?.exists ? 'available' : 'missing'} />
          </SettingsPanel>
        </div>
      </SettingsSection>

      <SettingsSection title="Configuration" hint="Applied live — secrets stay on this machine.">
        <div className="settings-grid">
          <ConfigPanels />
        </div>
      </SettingsSection>

      <SettingsSection title="Environment">
        <div className="settings-grid">
          <CapabilityPanel title="Agents" icon={<MonitorCog size={17} />} items={caps?.providers ?? []} />
          <CapabilityPanel title="Terminals" icon={<PlugZap size={17} />} items={caps?.terminals ?? []} />
          <CapabilityPanel title="Integrations" icon={<CheckCircle2 size={17} />} items={caps?.integrations ?? []} />
        </div>
      </SettingsSection>

      <RemoteAccessSettings />
    </div>
  )
}

function SummaryItem({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="sum-item">
      <div className="sum-label">{label}</div>
      <div className={`sum-value clip${mono ? ' mono' : ''}`}>{value}</div>
    </div>
  )
}

// ConfigPanels renders one editable card per setting group, sourced from the
// server registry. Connector-owned settings (Slack/GitHub/ingress) live on the
// Connectors page and every Steering key lives on the Attention → config view,
// so both are excluded here — Settings keeps only generic workspace groups
// (General). Edits stage through useConfigDraft; Save submits only the changed
// keys for that group (empty secret fields keep the stored value).
function ConfigPanels() {
  const { data } = useSettings()
  const cfg = useConfigDraft()
  const fields = useMemo(
    () => (data?.fields ?? []).filter((f) => !f.connector && f.group !== 'Steering'),
    [data?.fields],
  )

  const groups = useMemo(() => {
    const order: string[] = []
    const byGroup: Record<string, SettingField[]> = {}
    for (const f of fields) {
      if (!byGroup[f.group]) {
        byGroup[f.group] = []
        order.push(f.group)
      }
      byGroup[f.group].push(f)
    }
    return order.map((group) => ({ group, fields: byGroup[group] }))
  }, [fields])

  if (fields.length === 0) return null

  return (
    <>
      {groups.map(({ group, fields: gfields }) => {
        const dirty = Object.keys(cfg.changesFor(gfields)).length > 0
        return (
          <SettingsPanel key={group} title={group} icon={<SlidersHorizontal size={17} />}>
            <div className="config-form">
              {gfields.map((f) => (
                <ConfigField key={f.key} field={f} draft={cfg.draft[f.key]} onChange={(v) => cfg.setField(f.key, v)} />
              ))}
              <div className="config-actions">
                <button type="button" className="btn primary" disabled={!dirty || cfg.isPending} onClick={() => cfg.save(gfields)}>
                  {cfg.isPending ? <Loader2 size={14} className="spin" /> : <Save size={14} />}
                  Save {group}
                </button>
              </div>
            </div>
          </SettingsPanel>
        )
      })}
    </>
  )
}

function CapabilityPanel({ title, icon, items }: { title: string; icon: ReactNode; items: ToolCapability[] }) {
  return (
    <SettingsPanel title={title} icon={icon}>
      <div className="cap-list">
        {items.length === 0 ? (
          <div className="setting-value">none reported</div>
        ) : (
          items.map((item) => (
            <div key={item.id} className="cap-row">
              <span className={`cap-dot ${item.available ? 'on' : 'off'}`} />
              <div className="lrow-main">
                <div className="cap-title">{item.label || item.id}</div>
                <div className="cap-sub clip">{item.path || item.reason || item.status || item.id}</div>
              </div>
              <span className="tag">{item.available ? 'ready' : 'off'}</span>
            </div>
          ))
        )}
      </div>
    </SettingsPanel>
  )
}

function KeyValue({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="setting-row compact">
      <div className="setting-label">{label}</div>
      <div className={`setting-value clip${mono ? ' mono' : ''}`}>{value}</div>
    </div>
  )
}
