package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

const githubWebhookMaxBody = 1 << 20 // 1 MiB

func githubWebhookSecret() string {
	return strings.TrimSpace(os.Getenv("FLOW_GH_WEBHOOK_SECRET"))
}

// handleGitHubWebhook is the webhook-first ingress endpoint. It validates the
// HMAC signature and required headers synchronously, records the raw delivery
// for idempotency, answers GitHub with a fast 202, then normalizes + dispatches
// the delivery off the request goroutine. Unlike the earlier "doorbell" design
// it does NOT trigger a poll — the payload itself is parsed into GitHubEvents
// and pushed through the same dispatcher the poller feeds, so no GitHub API
// call is needed to act on the event.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	secret := githubWebhookSecret()
	if secret == "" {
		http.Error(w, "GitHub webhook secret is not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, githubWebhookMaxBody))
	if err != nil {
		http.Error(w, "webhook body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !verifyGitHubWebhookSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid GitHub webhook signature", http.StatusUnauthorized)
		return
	}

	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	delivery := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if event == "" || delivery == "" {
		http.Error(w, "missing GitHub webhook headers", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"ok":true}` + "\n"))

	if s.cfg.DB == nil {
		return
	}
	// Delivery-level idempotency: GitHub redelivers with the same
	// X-GitHub-Delivery id. Record once; if this id was already seen, the prior
	// receipt already handled it, so don't reprocess.
	isNew, err := flowdb.RecordGitHubDelivery(s.cfg.DB, flowdb.GitHubDeliveryEntry{
		DeliveryID: delivery,
		EventType:  event,
		Action:     webhookAction(body),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "github webhook: record delivery %s: %v\n", delivery, err)
		return
	}
	if !isNew {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.processGitHubDelivery(ctx, event, delivery, body)
	}()
}

// processGitHubDelivery normalizes a raw delivery into GitHubEvents, dispatches
// each through the GitHub dispatcher, and records the terminal delivery status.
// It is synchronous (the handler runs it in a goroutine) so it can be tested
// directly. Domain-level dedupe still happens inside the dispatcher via
// github_event_log, so a redelivery that slips past delivery-id idempotency
// still won't double-append the inbox.
func (s *Server) processGitHubDelivery(ctx context.Context, eventType, deliveryID string, body []byte) {
	if s.cfg.DB == nil {
		return
	}
	events, err := monitor.NormalizeGitHubWebhook(eventType, deliveryID, body)
	if err != nil {
		_ = flowdb.FinishGitHubDelivery(s.cfg.DB, deliveryID, "error", err.Error(), 0)
		return
	}
	if len(events) == 0 {
		_ = flowdb.FinishGitHubDelivery(s.cfg.DB, deliveryID, "ignored", "", 0)
		return
	}
	var dispatchErr error
	for _, ev := range events {
		if s.githubListener == nil {
			continue
		}
		if e := s.githubListener.Dispatch(ctx, ev); e != nil {
			fmt.Fprintf(os.Stderr, "github webhook: dispatch %s: %v\n", ev.EventKeyValue(), e)
			dispatchErr = e
		}
	}
	status, msg := "processed", ""
	if dispatchErr != nil {
		status, msg = "error", dispatchErr.Error()
	}
	_ = flowdb.FinishGitHubDelivery(s.cfg.DB, deliveryID, status, msg, len(events))
}

// webhookAction best-effort extracts the top-level "action" for the delivery
// log. Parse failures are fine here — full normalization (and any hard parse
// error) happens in processGitHubDelivery.
func webhookAction(body []byte) string {
	var a struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(body, &a)
	return strings.TrimSpace(a.Action)
}

// GitHubWebhookStatusView is the JSON payload GET /api/github/webhook/status
// returns so Mission Control's Git connector card can show the live transport
// state: which transport is active, whether a signing secret is configured, the
// public webhook URL to register, and the health of recent deliveries.
type GitHubWebhookStatusView struct {
	Transport        string `json:"transport"`
	SecretConfigured bool   `json:"secret_configured"`
	WebhookURL       string `json:"webhook_url,omitempty"`
	DeliveriesTotal  int    `json:"deliveries_total"`
	LastReceivedAt   string `json:"last_received_at,omitempty"`
	LastStatus       string `json:"last_status,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	Receiving        bool   `json:"receiving"`
	Summary          string `json:"summary"`
}

func (s *Server) handleGitHubWebhookStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.githubWebhookStatus())
}

func (s *Server) githubWebhookStatus() GitHubWebhookStatusView {
	v := GitHubWebhookStatusView{
		Transport:        string(monitor.GitHubTransport()),
		SecretConfigured: githubWebhookSecret() != "",
		WebhookURL:       s.connectorCallbackURL("/api/github/webhook"),
	}
	if s.cfg.DB != nil {
		if h, err := flowdb.GitHubWebhookHealth(s.cfg.DB); err == nil {
			v.DeliveriesTotal = h.Total
			v.LastReceivedAt = h.LastReceivedAt
			v.LastStatus = h.LastStatus
			v.LastError = h.LastError
			v.Receiving = h.Total > 0
		}
	}
	v.Summary = githubWebhookSummary(v)
	return v
}

func githubWebhookSummary(v GitHubWebhookStatusView) string {
	switch v.Transport {
	case "off":
		return "GitHub event ingress is off"
	case "polling":
		return "Polling GitHub via the gh API (no webhook)"
	}
	// webhook or hybrid
	if !v.SecretConfigured {
		return "Webhook secret not configured"
	}
	if v.DeliveriesTotal == 0 {
		return "Configured — awaiting first delivery"
	}
	if v.LastStatus == "error" {
		if v.LastError != "" {
			return "Last delivery error: " + v.LastError
		}
		return "Last delivery errored"
	}
	return fmt.Sprintf("Receiving — last delivery %s at %s", v.LastStatus, v.LastReceivedAt)
}

func verifyGitHubWebhookSignature(secret string, body []byte, header string) bool {
	header = strings.TrimSpace(header)
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(got, want)
}
