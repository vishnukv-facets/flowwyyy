package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func webhookTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", root)
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Server{cfg: Config{DB: db}}, db
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, s *Server, event, delivery, signature string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/github/webhook", bytes.NewReader(body))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, req)
	return rec
}

func TestGitHubWebhookRequiresSecret(t *testing.T) {
	s, _ := webhookTestServer(t)
	// No FLOW_GH_WEBHOOK_SECRET set.
	rec := postWebhook(t, s, "pull_request", "d1", "sha256=deadbeef", []byte(`{}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when secret unconfigured", rec.Code)
	}
}

func TestGitHubWebhookRejectsBadSignature(t *testing.T) {
	s, db := webhookTestServer(t)
	t.Setenv("FLOW_GH_WEBHOOK_SECRET", "topsecret")
	body := []byte(`{"action":"opened"}`)
	rec := postWebhook(t, s, "pull_request", "d1", "sha256=0000", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for bad signature", rec.Code)
	}
	// An unauthorized delivery must never be recorded.
	health, _ := productdb.GitHubWebhookHealth(db)
	if health.Total != 0 {
		t.Fatalf("bad-signature delivery recorded (Total=%d)", health.Total)
	}
}

func TestGitHubWebhookRejectsMissingHeaders(t *testing.T) {
	s, _ := webhookTestServer(t)
	t.Setenv("FLOW_GH_WEBHOOK_SECRET", "topsecret")
	body := []byte(`{"action":"opened"}`)
	sig := signBody("topsecret", body)
	// Valid signature but no event/delivery headers.
	rec := postWebhook(t, s, "", "", sig, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing headers", rec.Code)
	}
}

func TestGitHubWebhookAcceptsAndIsIdempotent(t *testing.T) {
	s, db := webhookTestServer(t)
	t.Setenv("FLOW_GH_WEBHOOK_SECRET", "topsecret")
	body := []byte(`{"action":"review_requested","repository":{"full_name":"o/r"},
		"pull_request":{"number":5,"html_url":"https://github.com/o/r/pull/5","user":{"login":"a"}}}`)
	sig := signBody("topsecret", body)

	rec := postWebhook(t, s, "pull_request", "dup-1", sig, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	// Redelivery with the SAME delivery id must not create a second row.
	rec = postWebhook(t, s, "pull_request", "dup-1", sig, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("redelivery status = %d, want 202", rec.Code)
	}
	health, err := productdb.GitHubWebhookHealth(db)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Total != 1 {
		t.Fatalf("delivery Total = %d, want 1 (idempotent)", health.Total)
	}
}

// processGitHubDelivery is the synchronous core; testing it directly avoids the
// handler's async goroutine. With a nil listener the dispatch step is a no-op,
// so we can assert normalization/ignore bookkeeping without spawning sessions.
func TestProcessGitHubDeliveryMarksSupportedProcessed(t *testing.T) {
	s, db := webhookTestServer(t)
	body := []byte(`{"action":"review_requested","repository":{"full_name":"o/r"},
		"pull_request":{"number":5,"html_url":"https://github.com/o/r/pull/5","user":{"login":"a"}}}`)
	if _, err := productdb.RecordGitHubDelivery(db, productdb.GitHubDeliveryEntry{DeliveryID: "p1", EventType: "pull_request", Action: "review_requested"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	s.processGitHubDelivery(context.Background(), "pull_request", "p1", body)
	health, _ := productdb.GitHubWebhookHealth(db)
	if health.LastStatus != "processed" {
		t.Fatalf("LastStatus = %q, want processed", health.LastStatus)
	}
}

func TestProcessGitHubDeliveryMarksUnsupportedIgnored(t *testing.T) {
	s, db := webhookTestServer(t)
	body := []byte(`{"action":"opened","repository":{"full_name":"o/r"},"pull_request":{"number":5,"user":{"login":"a"}}}`)
	if _, err := productdb.RecordGitHubDelivery(db, productdb.GitHubDeliveryEntry{DeliveryID: "p2", EventType: "pull_request", Action: "opened"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	s.processGitHubDelivery(context.Background(), "pull_request", "p2", body)
	health, _ := productdb.GitHubWebhookHealth(db)
	if health.LastStatus != "ignored" {
		t.Fatalf("LastStatus = %q, want ignored", health.LastStatus)
	}
}

func TestProcessGitHubDeliveryMarksMalformedErrored(t *testing.T) {
	s, db := webhookTestServer(t)
	if _, err := productdb.RecordGitHubDelivery(db, productdb.GitHubDeliveryEntry{DeliveryID: "p3", EventType: "pull_request", Action: ""}); err != nil {
		t.Fatalf("record: %v", err)
	}
	s.processGitHubDelivery(context.Background(), "pull_request", "p3", []byte(`{bad json`))
	health, _ := productdb.GitHubWebhookHealth(db)
	if health.LastStatus != "error" || health.LastError == "" {
		t.Fatalf("health = %+v, want status=error with message", health)
	}
}

func decodeWebhookStatus(t *testing.T, s *Server) GitHubWebhookStatusView {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleGitHubWebhookStatus(rec, httptest.NewRequest(http.MethodGet, "/api/github/webhook/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	var v GitHubWebhookStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, rec.Body.String())
	}
	return v
}

func TestGitHubWebhookStatusNeedsSecret(t *testing.T) {
	s, _ := webhookTestServer(t)
	t.Setenv("FLOW_GH_TRANSPORT", "webhook")
	// No secret set.
	v := decodeWebhookStatus(t, s)
	if v.Transport != "webhook" {
		t.Errorf("Transport = %q, want webhook", v.Transport)
	}
	if v.SecretConfigured {
		t.Error("SecretConfigured should be false with no secret")
	}
	if v.Receiving {
		t.Error("Receiving should be false")
	}
}

func TestGitHubWebhookStatusAwaitingThenReceiving(t *testing.T) {
	s, db := webhookTestServer(t)
	t.Setenv("FLOW_GH_TRANSPORT", "webhook")
	t.Setenv("FLOW_GH_WEBHOOK_SECRET", "topsecret")

	v := decodeWebhookStatus(t, s)
	if !v.SecretConfigured || v.Receiving || v.DeliveriesTotal != 0 {
		t.Fatalf("pre-delivery status = %+v, want configured/not-receiving/0", v)
	}

	// Simulate a processed delivery.
	if _, err := productdb.RecordGitHubDelivery(db, productdb.GitHubDeliveryEntry{DeliveryID: "s1", EventType: "pull_request", Action: "review_requested"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := productdb.FinishGitHubDelivery(db, "s1", "processed", "", 1); err != nil {
		t.Fatalf("finish: %v", err)
	}
	v = decodeWebhookStatus(t, s)
	if !v.Receiving || v.LastStatus != "processed" || v.DeliveriesTotal != 1 {
		t.Fatalf("post-delivery status = %+v, want receiving/processed/1", v)
	}
}

func TestGitHubWebhookStatusPollingMode(t *testing.T) {
	s, _ := webhookTestServer(t)
	t.Setenv("FLOW_GH_TRANSPORT", "polling")
	v := decodeWebhookStatus(t, s)
	if v.Transport != "polling" {
		t.Errorf("Transport = %q, want polling", v.Transport)
	}
}
