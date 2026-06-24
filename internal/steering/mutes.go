package steering

import (
	"database/sql"
	"fmt"

	"flow/internal/productdb"
)

// MuteAndSweep records a permanent suppression (channel / author / thread) and
// immediately dismisses every still-'new' feed card that matches it — so the
// operator's "perma drop" both stops future events (Stage 0 reads steering_mutes
// via the cascade ConfigFn) and clears the noise already sitting in the feed.
// Returns the number of open cards dismissed. Scope must be one of the
// productdb.MuteScope* constants.
func MuteAndSweep(db *sql.DB, scope, value string) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("steering: mute requires a db")
	}
	if err := productdb.AddSteeringMute(db, scope, value); err != nil {
		return 0, err
	}
	items, err := productdb.ListFeedItems(db, "new")
	if err != nil {
		return 0, fmt.Errorf("steering: mute sweep list feed: %w", err)
	}
	now := nowRFC3339()
	swept := 0
	for _, it := range items {
		if !muteMatches(scope, value, it) {
			continue
		}
		if err := productdb.SetFeedItemStatus(db, it.ID, "dismissed", now); err == nil {
			swept++
		}
	}
	return swept, nil
}

func muteMatches(scope, value string, it productdb.FeedItem) bool {
	switch scope {
	case productdb.MuteScopeChannel:
		return it.Channel == value
	case productdb.MuteScopeAuthor:
		return it.Author == value
	case productdb.MuteScopeThread:
		return it.ThreadKey == value
	}
	return false
}
