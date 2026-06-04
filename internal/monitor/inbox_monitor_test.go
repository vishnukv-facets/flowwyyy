package monitor

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

type recordingWakeTarget struct {
	calls [][]InboxEntry
	err   error
}

func (r *recordingWakeTarget) WakeTask(ctx context.Context, slug string, entries []InboxEntry) error {
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, append([]InboxEntry(nil), entries...))
	return nil
}

func TestInboxMonitorScanOnce_WakesForActionableBatch(t *testing.T) {
	slug := inboxTestSlug(t)
	if err := AppendInboxEvent(slug, InboundEvent{Kind: "pr_review_comment", ChannelType: "github", Text: "fix this"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	// reaction_added is recorded but non-actionable, so it must NOT wake the
	// session — it's the control here. (All github events are actionable now,
	// so a github event can no longer serve as the non-actionable case.)
	if err := AppendInboxEvent(slug, InboundEvent{Kind: "reaction_added", ChannelType: "slack", Text: "noise"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	target := &recordingWakeTarget{}
	m := NewInboxMonitor(slug, target, InboxMonitorOptions{})

	if err := m.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce() error = %v", err)
	}
	if len(target.calls) != 1 {
		t.Fatalf("wake calls = %d, want 1", len(target.calls))
	}
	if len(target.calls[0]) != 1 {
		t.Fatalf("woken entries = %d, want 1", len(target.calls[0]))
	}
	if got := target.calls[0][0].Event.Text; got != "fix this" {
		t.Fatalf("woken text = %q", got)
	}
	offset, err := ReadInboxMonitorCursor(slug)
	if err != nil {
		t.Fatalf("ReadInboxMonitorCursor() error = %v", err)
	}
	if offset == 0 {
		t.Fatalf("cursor = 0, want advanced offset")
	}
}

func TestInboxMonitorScanOnce_DoesNotAdvanceCursorWhenWakeFails(t *testing.T) {
	slug := inboxTestSlug(t)
	if err := AppendInboxEvent(slug, InboundEvent{Kind: "message", ChannelType: "slack", Text: "new reply"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	target := &recordingWakeTarget{err: errors.New("terminal unavailable")}
	m := NewInboxMonitor(slug, target, InboxMonitorOptions{})

	if err := m.ScanOnce(context.Background()); err == nil {
		t.Fatalf("ScanOnce() error = nil, want error")
	}
	offset, err := ReadInboxMonitorCursor(slug)
	if err != nil {
		t.Fatalf("ReadInboxMonitorCursor() error = %v", err)
	}
	if offset != 0 {
		t.Fatalf("cursor = %d, want 0", offset)
	}
}

// flakyWakeTarget fails its first okAfter calls, then succeeds — modelling a
// transient inject failure (the live path RESERVES errors for retry).
type flakyWakeTarget struct {
	mu      sync.Mutex
	calls   int
	okAfter int
	woken   chan struct{}
}

func (f *flakyWakeTarget) WakeTask(_ context.Context, _ string, _ []InboxEntry) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n <= f.okAfter {
		return errors.New("transient inject failure")
	}
	select {
	case f.woken <- struct{}{}:
	default:
	}
	return nil
}

func TestInboxMonitorRun_SurvivesTransientWakeError(t *testing.T) {
	slug := inboxTestSlug(t)
	if err := AppendInboxEvent(slug, InboundEvent{Kind: "message", ChannelType: "slack", Text: "reply"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	// First wake fails; the monitor must NOT die — it should retry on the next
	// tick (cursor wasn't advanced) and deliver successfully.
	target := &flakyWakeTarget{okAfter: 1, woken: make(chan struct{}, 1)}
	m := NewInboxMonitor(slug, target, InboxMonitorOptions{PollInterval: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	select {
	case <-target.woken:
		// Monitor survived the first error and delivered on retry. Good.
	case err := <-done:
		cancel()
		t.Fatalf("monitor exited before retrying after a transient wake error: %v", err)
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("monitor did not retry within 2s after a transient wake error")
	}
	// Stop the goroutine and wait for it to fully exit before the test's TempDir
	// cleanup runs — otherwise the still-ticking monitor races the rmdir.
	cancel()
	<-done
}

func TestInboxMonitorScanOnce_IgnoresAlreadyProcessedBytes(t *testing.T) {
	slug := inboxTestSlug(t)
	if err := AppendInboxEvent(slug, InboundEvent{Kind: "message", ChannelType: "slack", Text: "old reply"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	info, err := os.Stat(InboxPath(slug))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if err := WriteInboxMonitorCursor(slug, info.Size()); err != nil {
		t.Fatalf("WriteInboxMonitorCursor() error = %v", err)
	}
	if err := AppendInboxEvent(slug, InboundEvent{Kind: "message", ChannelType: "slack", Text: "new reply"}); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}
	target := &recordingWakeTarget{}
	m := NewInboxMonitor(slug, target, InboxMonitorOptions{})

	if err := m.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce() error = %v", err)
	}
	if len(target.calls) != 1 || len(target.calls[0]) != 1 {
		t.Fatalf("calls = %+v, want one new entry", target.calls)
	}
	if got := target.calls[0][0].Event.Text; got != "new reply" {
		t.Fatalf("woken text = %q, want new reply", got)
	}
}
