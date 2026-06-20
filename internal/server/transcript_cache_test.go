package server

import (
	"fmt"
	"os"
	"path/filepath"
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
