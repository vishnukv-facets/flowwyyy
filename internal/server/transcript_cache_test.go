package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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
