package flowdb

import "testing"

// TestGitHubWebhookDeliveryIdempotent pins delivery-level idempotency keyed on
// X-GitHub-Delivery: the first receipt is new, a redelivery (GitHub reuses the
// delivery id) is not, so the receiver can skip reprocessing it.
func TestGitHubWebhookDeliveryIdempotent(t *testing.T) {
	db := openTempDB(t)

	isNew, err := RecordGitHubDelivery(db, GitHubDeliveryEntry{
		DeliveryID: "abc-123",
		EventType:  "pull_request",
		Action:     "review_requested",
	})
	if err != nil {
		t.Fatalf("RecordGitHubDelivery first: %v", err)
	}
	if !isNew {
		t.Fatal("first delivery should be new")
	}

	isNew, err = RecordGitHubDelivery(db, GitHubDeliveryEntry{
		DeliveryID: "abc-123",
		EventType:  "pull_request",
		Action:     "review_requested",
	})
	if err != nil {
		t.Fatalf("RecordGitHubDelivery redelivery: %v", err)
	}
	if isNew {
		t.Fatal("redelivery of the same delivery id should not be new")
	}
}

// TestFinishGitHubDelivery records the terminal outcome of processing so the
// connector status can surface last-delivery state and errors.
func TestFinishGitHubDelivery(t *testing.T) {
	db := openTempDB(t)

	if _, err := RecordGitHubDelivery(db, GitHubDeliveryEntry{
		DeliveryID: "d1", EventType: "pull_request_review_comment", Action: "created",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := FinishGitHubDelivery(db, "d1", "processed", "", 1); err != nil {
		t.Fatalf("finish: %v", err)
	}

	health, err := GitHubWebhookHealth(db)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Total != 1 {
		t.Errorf("Total = %d, want 1", health.Total)
	}
	if health.LastStatus != "processed" {
		t.Errorf("LastStatus = %q, want processed", health.LastStatus)
	}
	if health.LastReceivedAt == "" {
		t.Error("LastReceivedAt should be set")
	}
	if health.LastError != "" {
		t.Errorf("LastError = %q, want empty", health.LastError)
	}
}

// TestGitHubWebhookHealthSurfacesLastError proves an errored delivery is
// retrievable so Mission Control can show "last delivery error".
func TestGitHubWebhookHealthSurfacesLastError(t *testing.T) {
	db := openTempDB(t)
	if _, err := RecordGitHubDelivery(db, GitHubDeliveryEntry{
		DeliveryID: "bad", EventType: "pull_request", Action: "closed",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := FinishGitHubDelivery(db, "bad", "error", "normalize failed", 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	health, err := GitHubWebhookHealth(db)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.LastStatus != "error" || health.LastError != "normalize failed" {
		t.Errorf("health = %+v, want status=error error=%q", health, "normalize failed")
	}
}

// TestGitHubWebhookHealthEmpty: a never-touched install reports zero deliveries
// and no last-received timestamp, which the status layer reads as "configured
// but not yet receiving".
func TestGitHubWebhookHealthEmpty(t *testing.T) {
	db := openTempDB(t)
	health, err := GitHubWebhookHealth(db)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Total != 0 || health.LastReceivedAt != "" {
		t.Errorf("empty health = %+v, want zero/empty", health)
	}
}
