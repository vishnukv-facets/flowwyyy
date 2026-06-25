package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

func TestProviderRateLimitHoldQueuesConnectorEvents(t *testing.T) {
	root, db := testRootDB(t)
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	cache := filepath.Join(root, "provider_usage", "claude.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(`{
		"rate_limits": {
			"five_hour": {"used_percentage": 100, "resets_at": "`+reset+`"}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DB: db, FlowRoot: root})

	held, err := srv.HoldSlackEvent(context.Background(), monitor.InboundEvent{
		Kind: "message", Channel: "C1", TS: "100.000", ThreadTS: "100.000", UserID: "U1", Text: "hello",
	})
	if err != nil || !held {
		t.Fatalf("HoldSlackEvent held=%v err=%v; want true,nil", held, err)
	}
	held, err = srv.HoldGitHubEvent(context.Background(), monitor.GitHubEvent{
		Kind: monitor.GitHubEventPRReviewRequested, Owner: "o", Repo: "r", Number: 1, Title: "review",
	})
	if err != nil || !held {
		t.Fatalf("HoldGitHubEvent held=%v err=%v; want true,nil", held, err)
	}

	pending, err := flowdb.CountPendingRateLimitQueue(db)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 2 {
		t.Fatalf("pending queue count = %d; want 2", pending)
	}
	ready, err := flowdb.ListReadyRateLimitQueue(db, time.Now().UTC().Format(time.RFC3339), 10)
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("ready queue = %+v; want none before reset", ready)
	}
	next, ok, err := flowdb.NextRateLimitQueueRunAfter(db)
	if err != nil || !ok || next != reset {
		t.Fatalf("next run_after = %q ok=%v err=%v; want %q", next, ok, err, reset)
	}
}

func TestQueueWakeAfterRateLimitUsesResetTime(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root})
	until := time.Now().Add(time.Hour).UTC()
	hold := providerLimitHold{Provider: "claude", Until: until}

	if err := srv.queueWakeAfterRateLimit("demo", "wake", hold); err != nil {
		t.Fatalf("queue wake: %v", err)
	}
	pw, ok, err := flowdb.PeekPendingWake(db, "demo")
	if err != nil || !ok {
		t.Fatalf("peek wake: ok=%v err=%v", ok, err)
	}
	if pw.Prompt != "wake" || pw.NotBefore != until.Format(time.RFC3339) {
		t.Fatalf("pending wake = %+v; want wake not_before %q", pw, until.Format(time.RFC3339))
	}
}
