package steering

import (
	"database/sql"
	"fmt"

	"flow/internal/flowdb"
)

// BackfillFeedTaskThreadTags re-links steerer-created tasks to their source
// thread. Tasks spawned by make_task / send_reply BEFORE the source-thread
// tagging fix carry no slack-thread:/gh- linkage, so a later reply on that
// thread — in-thread OR forwarded into another conversation — has no task to
// route to (the Samarthya case). This sweep walks every resolved ('acted') feed
// item that spawned a task and ensures that task carries the linkage tag derived
// from the feed item's thread key.
//
// Idempotent (AddTaskTag is INSERT OR IGNORE) and deterministic — it derives the
// tag purely from stored feed rows, no network. Safe to run on every boot.
// Returns the number of tasks newly tagged. A per-item failure is logged via
// logf (when non-nil) and skipped rather than aborting the whole sweep.
func BackfillFeedTaskThreadTags(db *sql.DB, logf func(string, ...any)) (int, error) {
	if db == nil {
		return 0, nil
	}
	items, err := flowdb.ListFeedItems(db, "acted")
	if err != nil {
		return 0, fmt.Errorf("steering: backfill list acted feed: %w", err)
	}
	tagged := 0
	for _, item := range items {
		slug := item.LinkedTask
		if slug == "" {
			continue
		}
		tag := feedTrackingTag(item)
		if tag == "" {
			continue
		}
		// Skip tasks that no longer exist (deleted) so we don't leave orphan
		// task_tags rows. GetTask wraps sql.ErrNoRows for a missing slug.
		if _, gerr := flowdb.GetTask(db, slug); gerr != nil {
			continue
		}
		existing, gerr := flowdb.GetTaskTags(db, slug)
		if gerr != nil {
			if logf != nil {
				logf("backfill: read tags for %s: %v", slug, gerr)
			}
			continue
		}
		if containsTag(existing, tag) {
			continue
		}
		if aerr := flowdb.AddTaskTag(db, slug, tag); aerr != nil {
			if logf != nil {
				logf("backfill: tag %s on %s: %v", tag, slug, aerr)
			}
			continue
		}
		tagged++
		if logf != nil {
			logf("backfill: linked task %s to source thread (%s)", slug, tag)
		}
	}
	return tagged, nil
}

// DismissSurfacedDropCards clears feed cards that should never have been
// surfaced: still-'new' rows whose suggested action is 'drop' (cascade-classified
// noise that an earlier bug let through to the feed). They move to 'dismissed' —
// gone from the active feed, still auditable under 'dismissed'/'all' and in the
// trace. Runs on boot; deterministic, no network. Returns the count dismissed.
func DismissSurfacedDropCards(db *sql.DB, logf func(string, ...any)) (int, error) {
	if db == nil {
		return 0, nil
	}
	items, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		return 0, fmt.Errorf("steering: sweep list new feed: %w", err)
	}
	now := nowRFC3339()
	dismissed := 0
	for _, item := range items {
		if item.SuggestedAction != string(ActionDrop) {
			continue
		}
		if err := flowdb.SetFeedItemStatus(db, item.ID, "dismissed", now); err != nil {
			if logf != nil {
				logf("sweep: dismiss drop card %s: %v", item.ID, err)
			}
			continue
		}
		dismissed++
	}
	return dismissed, nil
}

func containsTag(tags []string, want string) bool {
	want = flowdb.NormalizeTag(want)
	for _, t := range tags {
		if flowdb.NormalizeTag(t) == want {
			return true
		}
	}
	return false
}
