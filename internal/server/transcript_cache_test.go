package server

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A trailing "<synthetic>" record (Claude Code's session-limit / interrupt
// notice) must NOT clobber the real model — otherwise the context window
// collapses to the 200k default and a 1M-context Opus session reads as a bogus
// 100% instead of its true occupancy.
func TestSyntheticModelDoesNotInflateOccupancy(t *testing.T) {
	var stats transcriptUsageStats
	// A real opus-4-8 turn with ~572k of live context (mostly cache_read).
	real := `{"timestamp":"2026-06-20T10:00:00Z","message":{"model":"claude-opus-4-8","usage":{"input_tokens":388,"cache_creation_input_tokens":2985,"cache_read_input_tokens":568575,"output_tokens":120}}}`
	// The trailing session-limit notice: synthetic model, zero usage.
	synthetic := `{"timestamp":"2026-06-20T10:05:00Z","message":{"model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`

	accumulateTranscriptUsage(&stats, []byte(real))
	accumulateTranscriptUsage(&stats, []byte(synthetic))

	if stats.Model != "claude-opus-4-8" {
		t.Fatalf("model = %q, want claude-opus-4-8 (synthetic trailing record clobbered it)", stats.Model)
	}
	used, max := steererCompactUsage("claude", stats)
	if max != 1_000_000 {
		t.Errorf("context window = %d, want 1,000,000 (opus-4-8 is 1M)", max)
	}
	if occ := used * 100 / max; occ < 50 || occ > 65 {
		t.Errorf("occupancy = %d%%, want ~57%% (572k of 1M), not a clamped 100%%", occ)
	}
}

// claudeTurnLine builds one Claude usage record. Distinct ids per turn so the
// dedup (claudeSeen) counts each once — exercising that the dedup set carries
// across an incremental resume.
func claudeTurnLine(i int) string {
	return fmt.Sprintf(
		`{"timestamp":"2026-06-%02dT06:%02d:00Z","requestId":"req-%d","message":{"id":"msg-%d","model":"claude-opus-4-8","usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":1000,"output_tokens":40}}}`,
		(i%20)+1, i%60, i, i)
}

// Incremental re-scan (resume from a prior entry, parse only appended lines)
// must produce byte-identical accumulators to a full scan of the whole file.
func TestScanTranscriptIncrementalEqualsFull(t *testing.T) {
	dir := t.TempDir()
	const total = 80
	lines := make([]string, total)
	for i := 0; i < total; i++ {
		lines[i] = claudeTurnLine(i)
	}

	// Incremental: write first 30, scan; append the rest, resume-scan.
	incPath := filepath.Join(dir, "inc.jsonl")
	writeLines(t, incPath, lines[:30], false)
	mid, err := scanTranscriptFrom(incPath, nil)
	if err != nil {
		t.Fatalf("scan mid: %v", err)
	}
	appendLines(t, incPath, lines[30:])
	inc, err := scanTranscriptFrom(incPath, mid)
	if err != nil {
		t.Fatalf("scan incremental: %v", err)
	}

	// Full: write all lines at once, single scan.
	fullPath := filepath.Join(dir, "full.jsonl")
	writeLines(t, fullPath, lines, false)
	full, err := scanTranscriptFrom(fullPath, nil)
	if err != nil {
		t.Fatalf("scan full: %v", err)
	}

	if inc.usage.TokensSession != full.usage.TokensSession {
		t.Fatalf("TokensSession incremental=%d full=%d", inc.usage.TokensSession, full.usage.TokensSession)
	}
	if inc.usage.TokensUsed != full.usage.TokensUsed || inc.usage.Model != full.usage.Model {
		t.Fatalf("used/model mismatch: inc(%d,%q) full(%d,%q)", inc.usage.TokensUsed, inc.usage.Model, full.usage.TokensUsed, full.usage.Model)
	}
	if len(inc.usage.claudeSeen) != len(full.usage.claudeSeen) {
		t.Fatalf("claudeSeen size incremental=%d full=%d", len(inc.usage.claudeSeen), len(full.usage.claudeSeen))
	}
	if !reflect.DeepEqual(inc.usage.TokensByDay, full.usage.TokensByDay) {
		t.Fatalf("TokensByDay mismatch:\n inc=%v\nfull=%v", inc.usage.TokensByDay, full.usage.TokensByDay)
	}
	if !reflect.DeepEqual(inc.usage.CostByDay, full.usage.CostByDay) {
		t.Fatalf("CostByDay mismatch:\n inc=%v\nfull=%v", inc.usage.CostByDay, full.usage.CostByDay)
	}
	if inc.consumed != full.consumed {
		t.Fatalf("consumed incremental=%d full=%d", inc.consumed, full.consumed)
	}
	if len(inc.entries) != len(full.entries) {
		t.Fatalf("entries len incremental=%d full=%d", len(inc.entries), len(full.entries))
	}
}

// A trailing partial line (a record still being written, no newline yet) must
// not be consumed: `consumed` stays at its start so the next scan re-reads it
// once it's complete.
func TestScanTranscriptLeavesPartialLineUnconsumed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, path, []string{claudeTurnLine(0), claudeTurnLine(1)}, false)
	full, err := scanTranscriptFrom(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumedAfterTwo := full.consumed

	// Append a partial (unterminated) third line.
	appendRaw(t, path, claudeTurnLine(2)) // no trailing newline
	partial, err := scanTranscriptFrom(path, full)
	if err != nil {
		t.Fatal(err)
	}
	if partial.consumed != consumedAfterTwo {
		t.Fatalf("partial line advanced consumed to %d, want %d (unchanged)", partial.consumed, consumedAfterTwo)
	}
	if partial.usage.TokensSession != full.usage.TokensSession {
		t.Fatalf("partial line was counted: %d vs %d", partial.usage.TokensSession, full.usage.TokensSession)
	}

	// Complete the line; now it should be counted.
	appendRaw(t, path, "\n")
	done, err := scanTranscriptFrom(path, partial)
	if err != nil {
		t.Fatal(err)
	}
	if done.usage.TokensSession <= full.usage.TokensSession {
		t.Fatalf("completed line not counted: %d should exceed %d", done.usage.TokensSession, full.usage.TokensSession)
	}
}

func writeLines(t *testing.T, path string, lines []string, _ bool) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestPopulateTranscriptCacheEntryCapsStoredEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < transcriptCacheEntryLimit+50; i++ {
		_, _ = fmt.Fprintf(f, `{"type":"user","timestamp":"2026-06-08T06:%02d:00Z","message":{"role":"user","content":"msg-%03d"}}`+"\n", i%60, i)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	entry, err := populateTranscriptCacheEntry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(entry.entries); got != transcriptCacheEntryLimit {
		t.Fatalf("cached transcript entries = %d, want %d", got, transcriptCacheEntryLimit)
	}
	if got, want := entry.entries[0].Text, "msg-050"; got != want {
		t.Fatalf("first retained text = %q, want %q", got, want)
	}
	if got, want := entry.entries[len(entry.entries)-1].Text, "msg-249"; got != want {
		t.Fatalf("last retained text = %q, want %q", got, want)
	}
}
