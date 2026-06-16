package server

import (
	"encoding/json"
	"net/http"
	"strings"

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
	File string `json:"file"`
}

var (
	slackTextSendFn = monitor.SendAsThread
	slackFileSendFn = monitor.SendFileAsThread
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
	var sendErr error
	if hasFile {
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
