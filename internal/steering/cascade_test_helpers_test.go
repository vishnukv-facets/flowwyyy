package steering

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"flow/internal/flowdb"
)

func newSteeringTestCascade(t *testing.T) *Cascade {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	c := NewCascade(db, WatchConfig{})
	var n int
	c.newID = func() string { n++; return fmt.Sprintf("id%d", n) }
	c.now = func() time.Time { return time.Date(2026, 6, 12, 6, 40, 0, 0, time.UTC) }
	return c
}

func openCardCount(t *testing.T, c *Cascade) int {
	t.Helper()
	items, err := flowdb.ListFeedItems(c.DB, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	return len(items)
}
