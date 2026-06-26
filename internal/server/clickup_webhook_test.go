package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"flow/internal/flowdb"
)

func signClickUpBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func postClickUpWebhook(t *testing.T, s *Server, signature string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/clickup/webhook", bytes.NewReader(body))
	if signature != "" {
		req.Header.Set("X-Signature", signature)
	}
	rec := httptest.NewRecorder()
	s.handleClickUpWebhook(rec, req)
	return rec
}

func TestClickUpWebhookRequiresSecret(t *testing.T) {
	s, _ := webhookTestServer(t)
	rec := postClickUpWebhook(t, s, "deadbeef", []byte(`{"event":"taskDeleted","task_id":"cu-1","webhook_id":"wh-1"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when secret unconfigured", rec.Code)
	}
}

func TestClickUpWebhookRejectsBadSignature(t *testing.T) {
	s, db := webhookTestServer(t)
	t.Setenv("FLOW_CLICKUP_WEBHOOK_SECRET", "topsecret")
	body := []byte(`{"event":"taskDeleted","task_id":"cu-1","webhook_id":"wh-1"}`)
	rec := postClickUpWebhook(t, s, "0000", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for bad signature", rec.Code)
	}
	health, _ := flowdb.ClickUpWebhookHealth(db)
	if health.Total != 0 {
		t.Fatalf("bad-signature delivery recorded (Total=%d)", health.Total)
	}
}

func TestClickUpWebhookAcceptsAndIsIdempotent(t *testing.T) {
	s, db := webhookTestServer(t)
	t.Setenv("FLOW_CLICKUP_WEBHOOK_SECRET", "topsecret")
	body := []byte(`{"event":"taskDeleted","task_id":"cu-1","webhook_id":"wh-1"}`)
	sig := signClickUpBody("topsecret", body)

	rec := postClickUpWebhook(t, s, sig, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	rec = postClickUpWebhook(t, s, sig, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("redelivery status = %d, want 202", rec.Code)
	}
	health, err := flowdb.ClickUpWebhookHealth(db)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Total != 1 {
		t.Fatalf("delivery Total = %d, want 1 (idempotent)", health.Total)
	}
}

func TestProcessClickUpDeliveryMarksMalformedErrored(t *testing.T) {
	s, db := webhookTestServer(t)
	if _, err := flowdb.RecordClickUpDelivery(db, flowdb.ClickUpDeliveryEntry{DeliveryID: "bad", EventType: "", WebhookID: "wh-1"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	s.processClickUpDelivery("bad", []byte(`{bad json`))
	health, _ := flowdb.ClickUpWebhookHealth(db)
	if health.LastStatus != "error" || health.LastError == "" {
		t.Fatalf("health = %+v, want status=error with message", health)
	}
}

func TestClickUpWebhookStatus(t *testing.T) {
	s, db := webhookTestServer(t)
	t.Setenv("FLOW_CLICKUP_WEBHOOK_SECRET", "topsecret")
	t.Setenv("FLOW_CLICKUP_TEAM_ID", "321")
	t.Setenv("FLOW_CLICKUP_TEAM_NAME", "Engineering")
	t.Setenv("FLOW_CLICKUP_WEBHOOK_ID", "wh-1")
	if _, err := flowdb.RecordClickUpDelivery(db, flowdb.ClickUpDeliveryEntry{
		DeliveryID: "wh-1:taskDeleted:cu-1",
		EventType:  "taskDeleted",
		TaskID:     "cu-1",
		WebhookID:  "wh-1",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := flowdb.FinishClickUpDelivery(db, "wh-1:taskDeleted:cu-1", "processed", "", 1); err != nil {
		t.Fatalf("finish: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleClickUpWebhookStatus(rec, httptest.NewRequest(http.MethodGet, "/api/clickup/webhook/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	var v ClickUpWebhookStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !v.SecretConfigured || !v.Registered || !v.Receiving || v.TeamName != "Engineering" {
		t.Fatalf("status = %+v, want configured/registered/receiving/team", v)
	}
}
