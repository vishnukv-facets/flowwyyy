// internal/steering/session_test.go
package steering

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// pValue extracts the argument that follows "-p" in args.
func pValue(args []string) string {
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// argAfter returns the element after needle in args, or "".
func argAfter(args []string, needle string) string {
	for i, a := range args {
		if a == needle && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// hasArg reports whether needle appears anywhere in args.
func hasArg(args []string, needle string) bool {
	for _, a := range args {
		if a == needle {
			return true
		}
	}
	return false
}

// TestPoolPrimesThenResumes verifies that the first call to a mode creates a
// session (--session-id, prime included) and the second call resumes it
// (--resume, payload only).
func TestPoolPrimesThenResumes(t *testing.T) {
	var recorded [][]string
	uuidCounter := 0
	uuids := []string{"uuid-1", "uuid-2"}

	p := newClassifierPool(10, time.Hour)
	p.newID = func() string {
		id := uuids[uuidCounter]
		uuidCounter++
		return id
	}
	p.exec = func(ctx context.Context, args []string) (string, error) {
		cp := make([]string, len(args))
		copy(cp, args)
		recorded = append(recorded, cp)
		return "ok", nil
	}

	ctx := context.Background()

	// First call: should create a new session.
	_, err := p.run(ctx, "stage1", "PRIME", "PAY1", "k")
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(recorded))
	}
	call1 := recorded[0]
	if !hasArg(call1, "--session-id") {
		t.Errorf("first call: expected --session-id, got %v", call1)
	}
	if argAfter(call1, "--session-id") != "uuid-1" {
		t.Errorf("first call: expected session-id=uuid-1, got %q", argAfter(call1, "--session-id"))
	}
	pVal1 := pValue(call1)
	if pVal1 != "PRIME\n\nPAY1" {
		t.Errorf("first call: expected -p value %q, got %q", "PRIME\n\nPAY1", pVal1)
	}

	// Second call: should resume the existing session.
	_, err = p.run(ctx, "stage1", "PRIME", "PAY2", "k")
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("expected 2 recorded calls, got %d", len(recorded))
	}
	call2 := recorded[1]
	if !hasArg(call2, "--resume") {
		t.Errorf("second call: expected --resume, got %v", call2)
	}
	if argAfter(call2, "--resume") != "uuid-1" {
		t.Errorf("second call: expected resume uuid-1, got %q", argAfter(call2, "--resume"))
	}
	pVal2 := pValue(call2)
	if pVal2 != "PAY2" {
		t.Errorf("second call: expected -p value %q, got %q", "PAY2", pVal2)
	}
}

// TestPoolRotatesAfterMaxTurns verifies that after maxTurns calls the session
// is rotated: the (maxTurns+1)th call gets a new --session-id and re-includes
// the prime.
func TestPoolRotatesAfterMaxTurns(t *testing.T) {
	var recorded [][]string
	uuidCounter := 0
	uuids := []string{"uuid-1", "uuid-2", "uuid-3"}

	p := newClassifierPool(2, time.Hour)
	p.newID = func() string {
		id := uuids[uuidCounter]
		uuidCounter++
		return id
	}
	p.exec = func(ctx context.Context, args []string) (string, error) {
		cp := make([]string, len(args))
		copy(cp, args)
		recorded = append(recorded, cp)
		return "ok", nil
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := p.run(ctx, "stage1", "PRIME", "PAY", "k")
		if err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}
	}

	if len(recorded) != 3 {
		t.Fatalf("expected 3 recorded calls, got %d", len(recorded))
	}

	// Third call (index 2) should be a fresh session.
	call3 := recorded[2]
	if !hasArg(call3, "--session-id") {
		t.Errorf("third call: expected --session-id (rotation), got %v", call3)
	}
	id3 := argAfter(call3, "--session-id")
	id1 := argAfter(recorded[0], "--session-id")
	if id3 == id1 {
		t.Errorf("third call: expected NEW session-id, got same as first (%q)", id1)
	}
	pVal3 := pValue(call3)
	if !strings.HasPrefix(pVal3, "PRIME\n\n") {
		t.Errorf("third call: expected prime re-included, got -p=%q", pVal3)
	}
}

// TestPoolRotatesOnTTL verifies that after the TTL expires the next call
// re-creates the session with a new --session-id and re-includes the prime.
func TestPoolRotatesOnTTL(t *testing.T) {
	var recorded [][]string
	uuidCounter := 0
	uuids := []string{"uuid-1", "uuid-2"}

	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := t0

	p := newClassifierPool(10, time.Minute)
	p.now = func() time.Time { return clock }
	p.newID = func() string {
		id := uuids[uuidCounter]
		uuidCounter++
		return id
	}
	p.exec = func(ctx context.Context, args []string) (string, error) {
		cp := make([]string, len(args))
		copy(cp, args)
		recorded = append(recorded, cp)
		return "ok", nil
	}

	ctx := context.Background()

	// First call at T0.
	_, err := p.run(ctx, "stage1", "PRIME", "PAY1", "k")
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}

	// Advance clock by 2 minutes (past 1-minute TTL).
	clock = t0.Add(2 * time.Minute)

	// Second call should re-create.
	_, err = p.run(ctx, "stage1", "PRIME", "PAY2", "k")
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("expected 2 recorded calls, got %d", len(recorded))
	}
	call2 := recorded[1]
	if !hasArg(call2, "--session-id") {
		t.Errorf("second call (after TTL): expected --session-id, got %v", call2)
	}
	id2 := argAfter(call2, "--session-id")
	id1 := argAfter(recorded[0], "--session-id")
	if id2 == id1 {
		t.Errorf("second call (after TTL): expected NEW session-id, got same as first (%q)", id1)
	}
	pVal2 := pValue(call2)
	if !strings.HasPrefix(pVal2, "PRIME\n\n") {
		t.Errorf("second call (after TTL): expected prime re-included, got -p=%q", pVal2)
	}
}

// TestPoolRotatesOnPrimeKeyChange verifies that changing the primeKey causes
// the session to be rotated (re-created with prime).
func TestPoolRotatesOnPrimeKeyChange(t *testing.T) {
	var recorded [][]string
	uuidCounter := 0
	uuids := []string{"uuid-1", "uuid-2"}

	p := newClassifierPool(10, time.Hour)
	p.newID = func() string {
		id := uuids[uuidCounter]
		uuidCounter++
		return id
	}
	p.exec = func(ctx context.Context, args []string) (string, error) {
		cp := make([]string, len(args))
		copy(cp, args)
		recorded = append(recorded, cp)
		return "ok", nil
	}

	ctx := context.Background()

	// First call with primeKey="a".
	_, err := p.run(ctx, "stage1", "PRIME", "PAY1", "a")
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}

	// Second call with primeKey="b" — should re-create.
	_, err = p.run(ctx, "stage1", "PRIME", "PAY2", "b")
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("expected 2 recorded calls, got %d", len(recorded))
	}
	call2 := recorded[1]
	if !hasArg(call2, "--session-id") {
		t.Errorf("second call (key change): expected --session-id, got %v", call2)
	}
	id2 := argAfter(call2, "--session-id")
	id1 := argAfter(recorded[0], "--session-id")
	if id2 == id1 {
		t.Errorf("second call (key change): expected NEW session-id, got same (%q)", id1)
	}
	pVal2 := pValue(call2)
	if !strings.HasPrefix(pVal2, "PRIME\n\n") {
		t.Errorf("second call (key change): expected prime re-included, got -p=%q", pVal2)
	}
}

// TestPoolResetsOnError verifies that an exec error deletes the session slot,
// so the next call re-creates it with a fresh --session-id and the prime.
func TestPoolResetsOnError(t *testing.T) {
	var recorded [][]string
	uuidCounter := 0
	uuids := []string{"uuid-1", "uuid-2"}
	callCount := 0
	execErr := errors.New("claude exploded")

	p := newClassifierPool(10, time.Hour)
	p.newID = func() string {
		id := uuids[uuidCounter]
		uuidCounter++
		return id
	}
	p.exec = func(ctx context.Context, args []string) (string, error) {
		callCount++
		cp := make([]string, len(args))
		copy(cp, args)
		recorded = append(recorded, cp)
		if callCount == 1 {
			return "", execErr
		}
		return "ok", nil
	}

	ctx := context.Background()

	// First call — should fail.
	_, err := p.run(ctx, "stage1", "PRIME", "PAY1", "k")
	if err == nil {
		t.Fatal("expected error from first run, got nil")
	}
	if !errors.Is(err, execErr) {
		t.Errorf("expected execErr, got %v", err)
	}

	// Second call — session was dropped, so re-creates.
	_, err = p.run(ctx, "stage1", "PRIME", "PAY2", "k")
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("expected 2 recorded calls, got %d", len(recorded))
	}
	call2 := recorded[1]
	if !hasArg(call2, "--session-id") {
		t.Errorf("second call (after error reset): expected --session-id, got %v", call2)
	}
	pVal2 := pValue(call2)
	if !strings.HasPrefix(pVal2, "PRIME\n\n") {
		t.Errorf("second call (after error reset): expected prime re-included, got -p=%q", pVal2)
	}
}
