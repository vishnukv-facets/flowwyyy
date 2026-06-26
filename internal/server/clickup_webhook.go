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

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

const clickUpWebhookMaxBody = 1 << 20

func clickUpWebhookSecret() string {
	return strings.TrimSpace(os.Getenv("FLOW_CLICKUP_WEBHOOK_SECRET"))
}

func handleClickUpWebhookMethodAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func (s *Server) handleClickUpWebhook(w http.ResponseWriter, r *http.Request) {
	if !handleClickUpWebhookMethodAllowed(w, r) {
		return
	}
	secret := clickUpWebhookSecret()
	if secret == "" {
		http.Error(w, "ClickUp webhook secret is not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, clickUpWebhookMaxBody))
	if err != nil {
		http.Error(w, "webhook body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !verifyClickUpWebhookSignature(secret, body, r.Header.Get("X-Signature")) {
		http.Error(w, "invalid ClickUp webhook signature", http.StatusUnauthorized)
		return
	}
	deliveryID, eventType, taskID, webhookID := clickUpDeliveryIdentity(body)
	if deliveryID == "" {
		http.Error(w, "invalid ClickUp webhook payload", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"ok":true}` + "\n"))

	if s.cfg.DB == nil {
		return
	}
	isNew, err := flowdb.RecordClickUpDelivery(s.cfg.DB, flowdb.ClickUpDeliveryEntry{
		DeliveryID: deliveryID,
		EventType:  eventType,
		TaskID:     taskID,
		WebhookID:  webhookID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "clickup webhook: record delivery %s: %v\n", deliveryID, err)
		return
	}
	if !isNew {
		return
	}
	go s.processClickUpDelivery(deliveryID, body)
}

func (s *Server) processClickUpDelivery(deliveryID string, body []byte) {
	if s.cfg.DB == nil {
		return
	}
	events, err := monitor.NormalizeClickUpWebhook(body)
	if err != nil {
		_ = flowdb.FinishClickUpDelivery(s.cfg.DB, deliveryID, "error", err.Error(), 0)
		return
	}
	if len(events) == 0 {
		_ = flowdb.FinishClickUpDelivery(s.cfg.DB, deliveryID, "ignored", "", 0)
		return
	}
	var dispatchErr error
	for _, ev := range events {
		if ev.TeamID == "" {
			ev.TeamID = strings.TrimSpace(os.Getenv("FLOW_CLICKUP_TEAM_ID"))
		}
		if s.clickupDispatcher == nil {
			continue
		}
		if e := s.clickupDispatcher.Dispatch(context.Background(), ev); e != nil {
			fmt.Fprintf(os.Stderr, "clickup webhook: dispatch %s: %v\n", ev.EventKeyValue(), e)
			dispatchErr = e
		}
	}
	status, msg := "processed", ""
	if dispatchErr != nil {
		status, msg = "error", dispatchErr.Error()
	}
	_ = flowdb.FinishClickUpDelivery(s.cfg.DB, deliveryID, status, msg, len(events))
}

type clickUpDeliveryMeta struct {
	Event        string `json:"event"`
	TaskID       string `json:"task_id"`
	WebhookID    string `json:"webhook_id"`
	HistoryItems []struct {
		ID string `json:"id"`
	} `json:"history_items"`
}

func clickUpDeliveryIdentity(body []byte) (deliveryID, eventType, taskID, webhookID string) {
	var p clickUpDeliveryMeta
	if err := json.Unmarshal(body, &p); err != nil {
		return "", "", "", ""
	}
	eventType = strings.TrimSpace(p.Event)
	taskID = strings.TrimSpace(p.TaskID)
	webhookID = strings.TrimSpace(p.WebhookID)
	if webhookID == "" || eventType == "" {
		return "", eventType, taskID, webhookID
	}
	if len(p.HistoryItems) > 0 {
		if id := strings.TrimSpace(p.HistoryItems[0].ID); id != "" {
			return webhookID + ":" + id, eventType, taskID, webhookID
		}
	}
	if taskID == "" {
		return "", eventType, taskID, webhookID
	}
	return webhookID + ":" + eventType + ":" + taskID, eventType, taskID, webhookID
}

type ClickUpWebhookStatusView struct {
	SecretConfigured bool   `json:"secret_configured"`
	WebhookURL       string `json:"webhook_url,omitempty"`
	WebhookID        string `json:"webhook_id,omitempty"`
	Registered       bool   `json:"registered"`
	TeamID           string `json:"team_id,omitempty"`
	TeamName         string `json:"team_name,omitempty"`
	DeliveriesTotal  int    `json:"deliveries_total"`
	LastReceivedAt   string `json:"last_received_at,omitempty"`
	LastStatus       string `json:"last_status,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	LastEventType    string `json:"last_event_type,omitempty"`
	Receiving        bool   `json:"receiving"`
	Summary          string `json:"summary"`
}

func (s *Server) handleClickUpWebhookStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.clickUpWebhookStatus())
}

func (s *Server) clickUpWebhookStatus() ClickUpWebhookStatusView {
	v := ClickUpWebhookStatusView{
		SecretConfigured: clickUpWebhookSecret() != "",
		WebhookURL:       s.connectorCallbackURL("/api/clickup/webhook"),
		WebhookID:        strings.TrimSpace(os.Getenv("FLOW_CLICKUP_WEBHOOK_ID")),
		TeamID:           strings.TrimSpace(os.Getenv("FLOW_CLICKUP_TEAM_ID")),
		TeamName:         strings.TrimSpace(os.Getenv("FLOW_CLICKUP_TEAM_NAME")),
	}
	v.Registered = v.WebhookID != ""
	if s.cfg.DB != nil {
		if h, err := flowdb.ClickUpWebhookHealth(s.cfg.DB); err == nil {
			v.DeliveriesTotal = h.Total
			v.LastReceivedAt = h.LastReceivedAt
			v.LastStatus = h.LastStatus
			v.LastError = h.LastError
			v.LastEventType = h.LastEventType
			v.Receiving = h.Total > 0
		}
	}
	v.Summary = clickUpWebhookSummary(v)
	return v
}

func clickUpWebhookSummary(v ClickUpWebhookStatusView) string {
	if !v.SecretConfigured {
		return "Webhook secret not configured"
	}
	if !v.Registered {
		return "Webhook not registered"
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

func verifyClickUpWebhookSignature(secret string, body []byte, header string) bool {
	header = strings.TrimSpace(header)
	if secret == "" || header == "" {
		return false
	}
	got, err := hex.DecodeString(header)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
