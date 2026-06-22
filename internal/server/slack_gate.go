package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// slackExtVerdict is the cached external-channel send-gate verdict for one
// channel. resolved is false when the conversations.info lookup didn't succeed
// (token missing / API error); such verdicts are NOT cached so they retry.
type slackExtVerdict struct {
	external bool
	reason   string
	label    string
	resolved bool
}

// classifySlackChannel decides whether a Slack channel is outside the operator's
// org (Slack Connect / cross-workspace) — the condition that gates an outbound
// send behind the operator's approval. Cached because conversations.info is an
// API call that would otherwise run on every send. Fail-open: when Slack isn't
// configured or the lookup fails, it reports not-external so sends behave as
// before (the gate engages only on positive evidence; the wake fix is the
// primary incident guard).
func (s *Server) classifySlackChannel(ctx context.Context, channel string) slackExtVerdict {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return slackExtVerdict{}
	}
	if s.slackExtCache != nil {
		if v, ok := s.slackExtCache.get(channel); ok {
			return v
		}
	}
	conv, ok := monitor.LookupConversation(ctx, channel)
	if !ok {
		return slackExtVerdict{} // unresolved → fail-open, not cached (retry next time)
	}
	v := slackExtVerdict{
		external: conv.IsExternalToOrg(monitor.OperatorTeamIDs()),
		label:    slackConversationLabel(conv, channel),
		resolved: true,
	}
	if v.external {
		v.reason = slackExternalReason(conv)
	}
	if s.slackExtCache != nil {
		s.slackExtCache.set(channel, v)
	}
	return v
}

func slackExternalReason(c monitor.SlackConversation) string {
	switch {
	case c.IsOrgShared:
		return "Slack Connect: shared with another organization"
	case c.IsExtShared:
		return "Slack Connect: external organization present"
	case c.IsPendingExtShared:
		return "Slack Connect: external share pending"
	default:
		return "cross-workspace: a participant is in another Slack workspace"
	}
}

func slackConversationLabel(c monitor.SlackConversation, channel string) string {
	if n := strings.TrimSpace(c.Name); n != "" {
		if c.IsChannel || c.IsGroup {
			return "#" + strings.TrimPrefix(n, "#")
		}
		return n
	}
	return channel
}

func newPendingSendID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ps-" + flowdb.NowISO()
	}
	return "ps-" + hex.EncodeToString(b[:])
}

// handleSlackPendingList serves GET /api/slack/pending?status=pending — the
// outbound sends parked for the operator's approval (the inbox gate's data).
func (s *Server) handleSlackPendingList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.DB == nil {
		writeJSON(w, map[string]any{"pending": []flowdb.PendingSend{}})
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "pending"
	}
	items, err := flowdb.ListPendingSends(s.cfg.DB, status)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []flowdb.PendingSend{}
	}
	writeJSON(w, map[string]any{"pending": items})
}

type slackPendingDecideRequest struct {
	ID     string `json:"id"`
	Action string `json:"action"` // send | discard
	Text   string `json:"text"`   // optional edited body; empty keeps the original
}

// handleSlackPendingDecide serves POST /api/slack/pending/decide. "send" is the
// operator's approval: it posts the (optionally edited) message DIRECTLY through
// the send fns, bypassing the gate, then marks the row sent. "discard" drops it.
func (s *Server) handleSlackPendingDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.DB == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	var req slackPendingDecideRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	ps, ok, err := flowdb.GetPendingSend(s.cfg.DB, req.ID)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "pending send not found", http.StatusNotFound)
		return
	}
	if ps.Status != "pending" {
		http.Error(w, "this send was already "+ps.Status, http.StatusConflict)
		return
	}
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "discard", "reject", "cancel":
		if err := flowdb.SetPendingSendStatus(s.cfg.DB, ps.ID, "discarded"); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		s.publishUIChange("slack-pending")
		writeJSON(w, map[string]any{"ok": true, "status": "discarded"})
	case "send", "approve", "":
		text := ps.Text
		if t := strings.TrimSpace(req.Text); t != "" {
			text = req.Text
		}
		var sendErr error
		switch {
		case ps.PostAt != 0:
			_, sendErr = slackScheduleSendFn(ps.Channel, ps.ThreadTS, text, ps.Identity, ps.PostAt)
		case strings.TrimSpace(ps.FilePath) != "":
			sendErr = slackFileSendFn(ps.Channel, ps.ThreadTS, text, ps.FilePath, ps.Identity)
		default:
			sendErr = slackTextSendFn(ps.Channel, ps.ThreadTS, text, ps.Identity)
		}
		if sendErr != nil {
			writeError(w, sendErr, http.StatusBadGateway)
			return
		}
		if err := flowdb.SetPendingSendStatus(s.cfg.DB, ps.ID, "sent"); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		s.publishUIChange("slack-pending")
		writeJSON(w, map[string]any{"ok": true, "status": "sent"})
	default:
		http.Error(w, "action must be send or discard", http.StatusBadRequest)
	}
}
