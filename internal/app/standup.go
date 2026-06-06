package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"flow/internal/briefing"
	"flow/internal/flowdb"
)

var standupNow = time.Now

func cmdStandup(args []string) int {
	fs := flagSet("standup")
	window := fs.String("for", "today", "window: today|monday|24h")
	clipboard := fs.Bool("clipboard", false, "copy markdown to clipboard with pbcopy")
	limit := fs.Int("limit", 40, "max items per section")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}
	now := standupNow()
	since, err := standupWindow(*window, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	root, err := flowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	b, err := briefing.Build(db, root, briefing.Options{Now: now, Since: since, Limit: *limit})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	out := briefing.RenderMarkdown(b)
	if *clipboard {
		if err := copyToClipboard(out); err != nil {
			fmt.Fprintf(os.Stderr, "error: --clipboard: %v\n", err)
			return 1
		}
	}
	fmt.Print(out)
	return 0
}

func standupWindow(raw string, now time.Time) (time.Time, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "today":
		return startOfDay(now), nil
	case "24h", "day":
		return now.Add(-24 * time.Hour), nil
	case "monday", "week":
		day := startOfDay(now)
		daysSinceMonday := (int(day.Weekday()) - int(time.Monday) + 7) % 7
		return day.AddDate(0, 0, -daysSinceMonday), nil
	default:
		return time.Time{}, fmt.Errorf("bad --for %q (want today|monday|24h)", raw)
	}
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func copyToClipboard(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = bytes.NewBufferString(text)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
