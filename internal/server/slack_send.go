package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

type slackSendRequest struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Text     string `json:"text"`
	// As forces the send identity ("bot" | "user"); empty honors the server's
	// FLOW_SLACK_SEND_AS. `flow slack send --as bot` sets this so automation
	// posts as the bot (which carries chat:write).
	As string `json:"as"`
	// File is an optional local path to upload as an attachment; when set, Text
	// becomes the initial comment. Requires files:write (bot scope).
	File   string `json:"file"`
	PostAt int64  `json:"post_at,omitempty"`
}

var (
	slackTextSendFn     = monitor.SendAsThread
	slackFileSendFn     = monitor.SendFileAsThread
	slackScheduleSendFn = monitor.ScheduleAsThread
)

// handleSlackSend posts a Slack message as the flow bot using the SERVER's
// in-process token. The CLI (`flow slack send`) routes here so the message is
// sent with the freshly-validated token the running server holds in its
// environment, rather than a stale token captured by a tmux-spawned agent.
func (s *Server) handleSlackSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req slackSendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid slack send payload", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Channel) == "" {
		http.Error(w, "channel is required", http.StatusBadRequest)
		return
	}
	hasFile := strings.TrimSpace(req.File) != ""
	if strings.TrimSpace(req.Text) == "" && !hasFile {
		http.Error(w, "text or file is required", http.StatusBadRequest)
		return
	}
	if hasFile && req.PostAt != 0 {
		http.Error(w, "cannot schedule file uploads", http.StatusBadRequest)
		return
	}
	// External-channel send gate: a send to a channel OUTSIDE the operator's org
	// (Slack Connect / cross-workspace) is parked for the operator's explicit
	// approval in the inbox instead of going out. Every send path — manual CLI,
	// agent session, auto-permit, steerer — routes through here, so this is the
	// single chokepoint. The ONLY bypass is the operator approving it via
	// /api/slack/pending/decide, which posts through the send fns directly.
	if s.cfg.DB != nil {
		if v := s.classifySlackChannel(r.Context(), req.Channel); v.external {
			id := newPendingSendID()
			if err := flowdb.CreatePendingSend(s.cfg.DB, flowdb.PendingSend{
				ID: id, Channel: req.Channel, ChannelLabel: v.label, ThreadTS: req.ThreadTS,
				Text: req.Text, Identity: req.As, FilePath: req.File, PostAt: req.PostAt,
				Reason: v.reason, Status: "pending",
			}); err != nil {
				writeError(w, err, http.StatusInternalServerError)
				return
			}
			s.publishUIChange("slack-pending")
			writeJSONStatus(w, map[string]any{
				"queued": true, "pending_id": id, "reason": v.reason,
				"channel": req.Channel, "channel_label": v.label,
			}, http.StatusAccepted)
			return
		}
	}
	var sendErr error
	if req.PostAt != 0 {
		var scheduledMessageID string
		scheduledMessageID, sendErr = slackScheduleSendFn(req.Channel, req.ThreadTS, req.Text, req.As, req.PostAt)
		if sendErr == nil {
			writeJSON(w, map[string]any{"ok": true, "scheduled_message_id": scheduledMessageID, "post_at": req.PostAt})
			return
		}
	} else if hasFile {
		// File upload; Text (if any) rides along as the initial comment.
		sendErr = slackFileSendFn(req.Channel, req.ThreadTS, req.Text, req.File, req.As)
	} else {
		sendErr = slackTextSendFn(req.Channel, req.ThreadTS, req.Text, req.As)
	}
	if sendErr != nil {
		// 502: we reached the server but Slack (or the writes gate) rejected
		// the send. The CLI surfaces this and must NOT fall back to its own
		// (potentially stale) token.
		writeError(w, sendErr, http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
