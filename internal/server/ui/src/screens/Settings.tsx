import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Bell, CheckCircle2, Database, Loader2, MonitorCog, Moon, PlugZap, Save, Settings as SettingsIcon, SlidersHorizontal, Sun } from 'lucide-react'
import { useAction, useHealth, useSettings, useUiData } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { getTheme, onThemeChange, toggleTheme, type Theme } from '../lib/theme'
import { ErrorNote, Loading, ProviderIcon, SourceIcon } from '../components/ui'
import type { SettingField, ToolCapability } from '../lib/types'

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

  useEffect(() => onThemeChange(setTheme), [])

  const enableNotifications = async () => {
    if (typeof window === 'undefined' || !('Notification' in window)) {
      setPermission('unsupported')
      return
    }
    setPermission(await Notification.requestPermission())
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
    </div>
  )
}

function SettingsSection({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section className="settings-section">
      <div className="settings-section-head">
        <span className="eyebrow">{title}</span>
        {hint && <span className="settings-section-hint">{hint}</span>}
      </div>
      {children}
    </section>
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

function SettingsPanel({ title, icon, children }: { title: string; icon: ReactNode; children: ReactNode }) {
  return (
    <section className="settings-card">
      <div className="settings-card-head">
        <span>{icon}</span>
        <h2>{title}</h2>
      </div>
      <div className="settings-card-body">{children}</div>
    </section>
  )
}

const BOOL_TRUE = ['1', 'true', 'yes', 'y', 'on']
const isBoolOn = (s: string) => BOOL_TRUE.includes(s.trim().toLowerCase())

// ConfigPanels renders one editable card per setting group, sourced from the
// server registry. Edits are staged in `draft`; Save submits only the changed
// keys for that group (empty secret fields are skipped → keep the stored value).
function ConfigPanels() {
  const { data } = useSettings()
  const action = useAction()
  const [draft, setDraft] = useState<Record<string, string>>({})
  const fields = useMemo(() => data?.fields ?? [], [data?.fields])

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

  const changesFor = (gfields: SettingField[]) => {
    const out: Record<string, string> = {}
    for (const f of gfields) {
      const v = draft[f.key]
      if (v === undefined) continue
      if (f.type === 'secret') {
        if (v.trim() !== '') out[f.key] = v
      } else if (v !== f.value) {
        out[f.key] = v
      }
    }
    return out
  }

  const saveGroup = (gfields: SettingField[]) => {
    const changes = changesFor(gfields)
    if (Object.keys(changes).length === 0) return
    action.mutate(
      { kind: 'update-settings', settings: changes },
      {
        onSuccess: () =>
          setDraft((d) => {
            const next = { ...d }
            for (const k of Object.keys(changes)) delete next[k]
            return next
          }),
      },
    )
  }

  if (fields.length === 0) return null

  return (
    <>
      {groups.map(({ group, fields: gfields }) => {
        const dirty = Object.keys(changesFor(gfields)).length > 0
        return (
          <SettingsPanel key={group} title={group} icon={<SlidersHorizontal size={17} />}>
            <div className="config-form">
              {gfields.map((f) => (
                <ConfigField key={f.key} field={f} draft={draft[f.key]} onChange={(v) => setDraft((d) => ({ ...d, [f.key]: v }))} />
              ))}
              <div className="config-actions">
                <button type="button" className="btn primary" disabled={!dirty || action.isPending} onClick={() => saveGroup(gfields)}>
                  {action.isPending ? <Loader2 size={14} className="spin" /> : <Save size={14} />}
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

function ConfigField({ field, draft, onChange }: { field: SettingField; draft: string | undefined; onChange: (v: string) => void }) {
  const checked = draft !== undefined ? isBoolOn(draft) : isBoolOn(field.value)
  return (
    <div className="config-field">
      <div className="config-field-head">
        <label className="setting-label" htmlFor={field.key}>{field.label}</label>
        {field.source !== 'default' && <span className={`config-src ${field.source}`}>{field.source}</span>}
      </div>
      {field.type === 'bool' ? (
        <label className="config-toggle">
          <input id={field.key} type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked ? 'true' : 'false')} />
          <span>{checked ? 'On' : 'Off'}</span>
        </label>
      ) : field.type === 'enum' ? (
        <select id={field.key} className="input" value={draft ?? field.value} onChange={(e) => onChange(e.target.value)}>
          {(field.options ?? []).map((o) => (
            <option key={o} value={o}>{o}</option>
          ))}
        </select>
      ) : field.type === 'secret' ? (
        <input
          id={field.key}
          className="input mono"
          type="password"
          autoComplete="off"
          placeholder={field.set ? '•••••••• (set — blank keeps it)' : 'not set'}
          value={draft ?? ''}
          onChange={(e) => onChange(e.target.value)}
        />
      ) : (
        <input
          id={field.key}
          className="input"
          type={field.type === 'int' ? 'number' : 'text'}
          value={draft ?? field.value}
          placeholder={field.default || ''}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
      {field.help && <div className="config-help">{field.help}</div>}
    </div>
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
