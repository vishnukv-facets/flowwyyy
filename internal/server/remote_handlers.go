package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"flow/internal/productdb"
)

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleRemotePair (REMOTE mux, unauthenticated, rate-limited) redeems a pairing
// code and mints a 12h device token. This is the ONLY remote endpoint that does
// not require a device token — it is how a device obtains its first one.
func (s *Server) handleRemotePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.remoteLimiter.allowAt(clientIP(r), time.Now()) {
		writeError(w, errors.New("too many attempts"), http.StatusTooManyRequests)
		return
	}
	var body struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if !s.pairing.redeemAt(strings.TrimSpace(body.Code), time.Now()) {
		writeError(w, errors.New("invalid or expired pairing code"), http.StatusForbidden)
		return
	}
	token := mintRemoteToken()
	if token == "" {
		writeError(w, errors.New("token generation failed"), http.StatusInternalServerError)
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = "Paired device"
	}
	now := time.Now()
	expires := now.Add(remoteDeviceTokenTTL)
	id := mintRemoteToken()[:16]
	if err := productdb.InsertRemoteDevice(s.cfg.DB, id, label, hashRemoteToken(token),
		now.Format(time.RFC3339), expires.Format(time.RFC3339)); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"token":      token,
		"device_id":  id,
		"expires_at": expires.Format(time.RFC3339),
	})
}

// handleRemotePairCode (LOCAL) mints a pairing code + QR URL for the laptop UI.
func (s *Server) handleRemotePairCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !remoteAccessEnabled() {
		writeError(w, errors.New("enable remote access first"), http.StatusConflict)
		return
	}
	base := s.publicBaseURL()
	if base == "" {
		writeError(w, errors.New("public ingress not ready yet"), http.StatusServiceUnavailable)
		return
	}
	code, exp := s.pairing.createAt(time.Now())
	writeJSON(w, map[string]any{
		"code":       code,
		"expires_at": exp.Format(time.RFC3339),
		"pair_url":   strings.TrimRight(base, "/") + "/?pair=" + code,
	})
}

// handleRemoteDevices (LOCAL) lists paired devices.
func (s *Server) handleRemoteDevices(w http.ResponseWriter, r *http.Request) {
	list, err := productdb.ListRemoteDevices(s.cfg.DB)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	type view struct {
		ID, Label, CreatedAt, ExpiresAt, LastSeenAt string
		Revoked                                     bool
	}
	out := make([]view, 0, len(list))
	for _, d := range list {
		out = append(out, view{
			ID: d.ID, Label: d.Label, CreatedAt: d.CreatedAt, ExpiresAt: d.ExpiresAt,
			LastSeenAt: d.LastSeenAt.String, Revoked: d.RevokedAt.Valid,
		})
	}
	writeJSON(w, map[string]any{"devices": out})
}

// handleRemoteDeviceRevoke (LOCAL) revokes one device by id.
func (s *Server) handleRemoteDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeError(w, errors.New("device id required"), http.StatusBadRequest)
		return
	}
	if err := productdb.RevokeRemoteDevice(s.cfg.DB, body.ID, productdb.NowISO()); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleRemoteDeviceDelete (LOCAL) permanently removes one device row by id —
// the "clear it from the list" action for already-revoked devices (and a hard
// remove for any device). Localhost-only, like revoke: the /api/remote/ prefix
// keeps it off the remote phone's RPC surface (remoteForbiddenRPCPath).
func (s *Server) handleRemoteDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeError(w, errors.New("device id required"), http.StatusBadRequest)
		return
	}
	if err := productdb.DeleteRemoteDevice(s.cfg.DB, body.ID); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleRemoteStatus (LOCAL) reports the toggle state + public URL.
func (s *Server) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"enabled":    remoteAccessEnabled(),
		"public_url": s.publicBaseURL(),
	})
}

// handleRemoteEnable (LOCAL) enables the remote-access surface.
func (s *Server) handleRemoteEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if activeIngressProvider() != ingressProviderZrok || !zrokAutoStart() {
		writeError(w, errors.New("set up public ingress (zrok) first — see Connectors"), http.StatusConflict)
		return
	}
	s.setRemoteAccessConfig(true)
	s.ensureZrokIngressCredentials()
	if s.zrok != nil {
		s.zrok.start() // idempotent — no-op if already serving
	}
	writeJSON(w, map[string]any{"enabled": true, "public_url": s.publicBaseURL()})
}

// handleRemoteDisable (LOCAL) disables the remote-access surface.
func (s *Server) handleRemoteDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.setRemoteAccessConfig(false)
	// The zrok share stays up to keep serving the GitHub webhook; the composite
	// handler now 404s all app paths because remoteAccessEnabled() is false.
	writeJSON(w, map[string]any{"enabled": false})
}

// setRemoteAccessConfig persists FLOW_REMOTE_ACCESS to config.json and the env,
// mirroring ensureZrokIngressCredentials' load/save pattern.
func (s *Server) setRemoteAccessConfig(on bool) {
	val := "0"
	if on {
		val = "1"
	}
	os.Setenv("FLOW_REMOTE_ACCESS", val)
	path := s.configPath()
	if path == "" {
		return
	}
	cfg := loadConfigFile(path)
	cfg["FLOW_REMOTE_ACCESS"] = val
	_ = saveConfigFile(path, cfg)
}
