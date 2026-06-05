package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The "Current state" block in a task's brief.md is delimited by these
// HTML-comment markers. They are invisible in rendered markdown and let
// writeBriefCurrentState replace exactly this block on each refresh without
// disturbing the original (human/spawn-authored) brief content around it.
const (
	briefStateStartMarker = "<!-- flow:state:start -->"
	briefStateEndMarker   = "<!-- flow:state:end -->"
)

// renderBriefStateBlock builds the machine-maintained "Current state" block:
// the markers, a stamped header line, and the (terse) body. No leading or
// trailing newline — callers control surrounding whitespace.
func renderBriefStateBlock(body, date string) string {
	return strings.Join([]string{
		briefStateStartMarker,
		"**Current state** · updated " + date,
		strings.TrimRight(body, "\n"),
		briefStateEndMarker,
	}, "\n")
}

// writeBriefCurrentState refreshes the "Current state" block in briefPath,
// preserving everything outside the flow:state markers byte-for-byte. If the
// markers are present, the block between them (inclusive) is replaced; if not,
// the block is appended at EOF after a blank line; if the file is missing, it's
// created containing just the block. The write is atomic (temp + rename).
func writeBriefCurrentState(briefPath, body, date string) error {
	block := renderBriefStateBlock(body, date)

	existing, err := os.ReadFile(briefPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read brief: %w", err)
	}

	var out string
	switch {
	case os.IsNotExist(err):
		out = block + "\n"
	default:
		cur := string(existing)
		start := strings.Index(cur, briefStateStartMarker)
		end := strings.Index(cur, briefStateEndMarker)
		if start >= 0 && end > start {
			// Replace the existing block (inclusive); keep both sides intact.
			out = cur[:start] + block + cur[end+len(briefStateEndMarker):]
		} else {
			// No block yet — append, preserving the original brief exactly.
			switch {
			case cur == "":
				out = block + "\n"
			case strings.HasSuffix(cur, "\n\n"):
				out = cur + block + "\n"
			case strings.HasSuffix(cur, "\n"):
				out = cur + "\n" + block + "\n"
			default:
				out = cur + "\n\n" + block + "\n"
			}
		}
	}

	return writeFileAtomic(briefPath, []byte(out), 0o644)
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a reader never observes a half-written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".brief-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
