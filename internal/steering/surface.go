package steering

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"flow/internal/flowdb"
)

// SurfaceCardParams is the structured verdict a per-channel steerer session
// emits via `flow attention surface`.
type SurfaceCardParams struct {
	Source      string
	Channel     string
	ChannelType string
	ThreadKey   string
	TS          string
	ThreadTS    string
	Author      string
	Action      string
	MatchedTask string
	Summary     string
	Draft       string
	Confidence  float64
	Reason      string
	ContextOnly bool
}

const surfaceClubWindow = 12 * time.Hour

// SurfaceCard validates a proposed thread_key against open cards in the same
// channel, then records the thread decision and optionally surfaces a feed row.
func SurfaceCard(ctx context.Context, db *sql.DB, p SurfaceCardParams) (string, bool, error) {
	_ = ctx
	rawKey := p.Channel + ":" + firstNonEmpty(p.ThreadTS, p.TS)
	key := validateSurfaceThreadKey(db, p, rawKey)
	action := firstNonEmpty(p.Action, string(ActionDrop))
	now := flowdb.NowISO()
	v := Verdict{
		Source:          p.Source,
		ThreadKey:       key,
		SuggestedAction: Action(action),
		MatchedTask:     p.MatchedTask,
		Summary:         p.Summary,
		Draft:           p.Draft,
		Confidence:      p.Confidence,
		Reason:          p.Reason,
	}
	item := flowdb.FeedItem{
		ID:              randomUUID(),
		Source:          p.Source,
		ThreadKey:       key,
		Summary:         SanitizeOperatorText(p.Summary),
		SuggestedAction: action,
		MatchedTask:     p.MatchedTask,
		Confidence:      p.Confidence,
		Draft:           SanitizeOperatorText(p.Draft),
		Reason:          SanitizeOperatorText(p.Reason),
		Channel:         p.Channel,
		ChannelType:     p.ChannelType,
		Author:          p.Author,
		TS:              p.TS,
		Status:          "new",
		CreatedAt:       now,
	}

	if Action(action) == ActionDrop {
		id, _, err := resolveOpenSurfaceCard(db, key, now)
		recordSurfaceThreadDecision(db, key, v, p.TS, now)
		return id, false, err
	}

	if p.ContextOnly {
		id, refreshed, err := refreshOpenSurfaceCard(db, item)
		recordSurfaceThreadDecision(db, key, v, p.TS, now)
		return id, refreshed, err
	}

	id, surfaced, err := flowdb.UpsertFeedItemSurfaced(db, item)
	if err != nil {
		return "", false, err
	}
	recordSurfaceThreadDecision(db, key, v, p.TS, now)
	return id, surfaced, nil
}

func refreshOpenSurfaceCard(db *sql.DB, item flowdb.FeedItem) (string, bool, error) {
	existing, ok, err := flowdb.LatestFeedItemByThread(db, item.ThreadKey)
	if err != nil || !ok || existing.Status != "new" {
		return "", false, err
	}
	id, surfaced, err := flowdb.UpsertFeedItemSurfaced(db, item)
	if err != nil {
		return "", false, err
	}
	return id, surfaced, nil
}

func resolveOpenSurfaceCard(db *sql.DB, threadKey, at string) (string, bool, error) {
	existing, ok, err := flowdb.LatestFeedItemByThread(db, threadKey)
	if err != nil || !ok || existing.Status != "new" {
		return "", false, err
	}
	n, err := flowdb.ResolveOpenFeedItemsByThread(db, threadKey, at)
	return existing.ID, n > 0, err
}

func validateSurfaceThreadKey(db *sql.DB, p SurfaceCardParams, rawKey string) string {
	proposed := strings.TrimSpace(p.ThreadKey)
	if proposed == "" || proposed == rawKey {
		return rawKey
	}
	since := time.Now().Add(-surfaceClubWindow).UTC().Format(time.RFC3339)
	cands, err := flowdb.ListOpenClubCandidates(db, p.Channel, "", since, 50)
	if err != nil {
		return rawKey
	}
	if anchorIndex(cands, proposed) >= 0 {
		return proposed
	}
	return rawKey
}

func anchorIndex(anchors []flowdb.FeedItem, threadKey string) int {
	if threadKey == "" {
		return -1
	}
	for i, a := range anchors {
		if a.ThreadKey == threadKey {
			return i
		}
	}
	return -1
}

func recordSurfaceThreadDecision(db *sql.DB, key string, v Verdict, ts, at string) {
	_ = flowdb.RecordThreadDecision(db, flowdb.ThreadDecision{
		ThreadKey:  key,
		Source:     v.Source,
		Action:     string(v.SuggestedAction),
		Confidence: v.Confidence,
		Reason:     v.Reason,
		Summary:    SanitizeOperatorText(v.Summary),
		LastSeenTS: ts,
		At:         at,
	})
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
