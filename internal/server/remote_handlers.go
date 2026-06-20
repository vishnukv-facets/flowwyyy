package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"flow/internal/flowdb"
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
	if err := flowdb.InsertRemoteDevice(s.cfg.DB, id, label, hashRemoteToken(token),
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
	list, err := flowdb.ListRemoteDevices(s.cfg.DB)
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
	if err := flowdb.RevokeRemoteDevice(s.cfg.DB, body.ID, flowdb.NowISO()); err != nil {
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
// Implemented in Task 7; stub returns 501 until then.
func (s *Server) handleRemoteEnable(w http.ResponseWriter, r *http.Request) {
	writeError(w, errors.New("not implemented"), http.StatusNotImplemented)
}

// handleRemoteDisable (LOCAL) disables the remote-access surface.
// Implemented in Task 7; stub returns 501 until then.
func (s *Server) handleRemoteDisable(w http.ResponseWriter, r *http.Request) {
	writeError(w, errors.New("not implemented"), http.StatusNotImplemented)
}
