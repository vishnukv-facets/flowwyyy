import { useEffect, useState } from 'react'
import QRCode from 'qrcode'
import { Wifi, WifiOff, Smartphone, Trash2, RefreshCw } from 'lucide-react'
import { rpc } from '../lib/rpc'
import { SettingsPanel, SettingsSection } from '../components/SettingsPanels'

interface Device {
  ID: string
  Label: string
  CreatedAt: string
  ExpiresAt: string
  LastSeenAt: string
  Revoked: boolean
}

export function RemoteAccessSettings() {
  const [enabled, setEnabled] = useState(false)
  const [publicUrl, setPublicUrl] = useState('')
  const [devices, setDevices] = useState<Device[]>([])
  const [qr, setQr] = useState<string>('')
  const [pairUrl, setPairUrl] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)
  const [toggling, setToggling] = useState(false)
  const [pairing, setPairing] = useState(false)

  async function refresh() {
    const st = await rpc.request({ method: 'GET', path: '/api/remote/status' })
    const s = (st.json ?? {}) as { enabled?: boolean; public_url?: string }
    setEnabled(!!s.enabled)
    setPublicUrl(s.public_url ?? '')
    const dl = await rpc.request({ method: 'GET', path: '/api/remote/devices' })
    setDevices(((dl.json ?? {}) as { devices?: Device[] }).devices ?? [])
  }

  useEffect(() => {
    refresh().finally(() => setLoading(false))
  }, [])

  async function toggle(on: boolean) {
    setErr('')
    setToggling(true)
    try {
      const r = await rpc.request({ method: 'POST', path: on ? '/api/remote/enable' : '/api/remote/disable' })
      if (r.status >= 400) {
        setErr((r.json as any)?.error || r.error || 'failed')
        return
      }
      await refresh()
    } finally {
      setToggling(false)
    }
  }

  async function addDevice() {
    setErr('')
    setQr('')
    setPairUrl('')
    setPairing(true)
    try {
      const r = await rpc.request({ method: 'POST', path: '/api/remote/pair-code' })
      if (r.status >= 400) {
        setErr((r.json as any)?.error || r.error || 'failed')
        return
      }
      const url = (r.json as any).pair_url as string
      setPairUrl(url)
      setQr(await QRCode.toDataURL(url, { margin: 1, width: 240 }))
    } finally {
      setPairing(false)
    }
  }

  async function revoke(id: string) {
    await rpc.request({ method: 'POST', path: '/api/remote/devices/revoke', body: { id } })
    await refresh()
  }

  if (loading) {
    return (
      <SettingsSection title="Remote access">
        <div className="settings-grid">
          <SettingsPanel title="Remote access" icon={<Wifi size={17} />}>
            <div className="setting-value">Loading…</div>
          </SettingsPanel>
        </div>
      </SettingsSection>
    )
  }

  const activeDevices = devices.filter((d) => !d.Revoked)
  const revokedDevices = devices.filter((d) => d.Revoked)

  return (
    <SettingsSection title="Remote access" hint="Reach Mission Control from your phone. Device tokens expire 12h after pairing.">
      <div className="settings-grid">
        <SettingsPanel title="Remote access" icon={enabled ? <Wifi size={17} /> : <WifiOff size={17} />}>
          {err && <div className="error-note">{err}</div>}
          <div className="setting-row">
            <div>
              <div className="setting-label">Remote access</div>
              <div className="setting-value">{enabled ? 'enabled' : 'disabled'}</div>
            </div>
            <button
              type="button"
              className={`btn${enabled ? '' : ' primary'}`}
              disabled={toggling}
              onClick={() => toggle(!enabled)}
            >
              {toggling ? <RefreshCw size={14} className="spin" /> : enabled ? <WifiOff size={14} /> : <Wifi size={14} />}
              {enabled ? 'Disable' : 'Enable'}
            </button>
          </div>
          {enabled && publicUrl && (
            <div className="setting-row compact">
              <div className="setting-label">Public URL</div>
              <code className="setting-value clip mono">{publicUrl}</code>
            </div>
          )}
          {enabled && (
            <div className="config-actions">
              <button type="button" className="btn" disabled={pairing} onClick={addDevice}>
                {pairing ? <RefreshCw size={14} className="spin" /> : <Smartphone size={14} />}
                Add device (show QR)
              </button>
            </div>
          )}
          {qr && (
            <div className="pair-qr">
              <img src={qr} alt="Pairing QR code" width={240} height={240} style={{ display: 'block', borderRadius: 8 }} />
              <div className="setting-hint">
                Scan within 5 minutes to pair your device.
                <br />
                <code className="mono" style={{ wordBreak: 'break-all', fontSize: '0.75em' }}>{pairUrl}</code>
              </div>
            </div>
          )}
        </SettingsPanel>

        <SettingsPanel title="Paired devices" icon={<Smartphone size={17} />}>
          {devices.length === 0 ? (
            <div className="setting-value">No devices paired yet.</div>
          ) : (
            <div className="cap-list">
              {activeDevices.map((d) => (
                <div key={d.ID} className="cap-row">
                  <span className="cap-dot on" />
                  <div className="lrow-main">
                    <div className="cap-title">{d.Label || d.ID}</div>
                    <div className="cap-sub clip">expires {d.ExpiresAt}</div>
                  </div>
                  <button
                    type="button"
                    className="btn"
                    title="Revoke this device"
                    onClick={() => revoke(d.ID)}
                  >
                    <Trash2 size={13} />
                    Revoke
                  </button>
                </div>
              ))}
              {revokedDevices.length > 0 && (
                <>
                  <div className="setting-hint" style={{ marginTop: 8 }}>Revoked</div>
                  {revokedDevices.map((d) => (
                    <div key={d.ID} className="cap-row" style={{ opacity: 0.5 }}>
                      <span className="cap-dot off" />
                      <div className="lrow-main">
                        <div className="cap-title">{d.Label || d.ID}</div>
                        <div className="cap-sub clip">revoked · expired {d.ExpiresAt}</div>
                      </div>
                      <span className="tag">revoked</span>
                    </div>
                  ))}
                </>
              )}
            </div>
          )}
        </SettingsPanel>
      </div>
    </SettingsSection>
  )
}
