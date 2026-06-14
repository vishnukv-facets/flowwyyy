package server

import (
	"testing"
	"time"
)

func TestNewOwnerScheduler(t *testing.T) {
	t.Run("disabled via env returns nil", func(t *testing.T) {
		t.Setenv("FLOW_OWNER_SCHEDULER", "off")
		if s := newOwnerScheduler(&Server{cfg: Config{CommandPath: "/usr/bin/flow"}}); s != nil {
			t.Fatal("expected nil when FLOW_OWNER_SCHEDULER=off")
		}
	})

	t.Run("no flow binary returns nil", func(t *testing.T) {
		t.Setenv("FLOW_OWNER_SCHEDULER", "")
		if s := newOwnerScheduler(&Server{cfg: Config{CommandPath: ""}}); s != nil {
			t.Fatal("expected nil when CommandPath is empty")
		}
		if s := newOwnerScheduler(nil); s != nil {
			t.Fatal("expected nil for nil server")
		}
	})

	t.Run("default interval", func(t *testing.T) {
		t.Setenv("FLOW_OWNER_SCHEDULER", "")
		t.Setenv("FLOW_OWNER_SCHEDULER_INTERVAL", "")
		s := newOwnerScheduler(&Server{cfg: Config{CommandPath: "/usr/bin/flow"}})
		if s == nil {
			t.Fatal("expected a scheduler")
		}
		if s.interval != defaultOwnerSchedulerInterval {
			t.Fatalf("interval = %s, want default %s", s.interval, defaultOwnerSchedulerInterval)
		}
	})

	t.Run("custom interval honored", func(t *testing.T) {
		t.Setenv("FLOW_OWNER_SCHEDULER", "")
		t.Setenv("FLOW_OWNER_SCHEDULER_INTERVAL", "15s")
		s := newOwnerScheduler(&Server{cfg: Config{CommandPath: "/usr/bin/flow"}})
		if s == nil {
			t.Fatal("expected a scheduler")
		}
		if s.interval != 15*time.Second {
			t.Fatalf("interval = %s, want 15s", s.interval)
		}
	})

	t.Run("invalid interval falls back to default", func(t *testing.T) {
		t.Setenv("FLOW_OWNER_SCHEDULER", "")
		t.Setenv("FLOW_OWNER_SCHEDULER_INTERVAL", "not-a-duration")
		s := newOwnerScheduler(&Server{cfg: Config{CommandPath: "/usr/bin/flow"}})
		if s == nil || s.interval != defaultOwnerSchedulerInterval {
			t.Fatalf("expected default interval on invalid input, got %v", s)
		}
	})
}

// TestOwnerSchedulerStartStop verifies the lifecycle is safe and idempotent:
// double start is a no-op, stop unblocks the loop, and stop without start is a
// no-op. The boot tick is a no-op here because CommandPath points at a binary
// that won't error materially in CombinedOutput's failure path (the loop logs
// and continues), so this exercises wiring, not the shell-out.
func TestOwnerSchedulerStartStop(t *testing.T) {
	t.Setenv("FLOW_OWNER_SCHEDULER", "")
	t.Setenv("FLOW_OWNER_SCHEDULER_INTERVAL", "1h") // long, so only the boot tick can run
	s := newOwnerScheduler(&Server{cfg: Config{CommandPath: "/bin/true"}})
	if s == nil {
		t.Fatal("expected a scheduler")
	}
	s.start()
	s.start() // idempotent: second start must not spawn a second loop or panic
	done := make(chan struct{})
	go func() { s.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return within 5s")
	}
	s.stop() // stop after stop is a no-op
}
