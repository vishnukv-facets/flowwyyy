package steering

import (
	"context"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// Retriage re-runs the per-item cascade tail (task index → stage 2 → deep triage
// → writeFeed) for an already-surfaced feed item, reconstructing the inbound
// event from the stored row. It deliberately skips Stage 0/1 and the verdict
// cache so the operator can FORCE a fresh decision — e.g. to re-evaluate a card
// after the matching logic or a task's brief/updates changed. writeFeed coalesces
// by thread_key, so the existing 'new' card is updated in place rather than
// duplicated.
func (c *Cascade) Retriage(ctx context.Context, item flowdb.FeedItem) error {
	ev := feedItemToEvent(item)
	cleaned := c.cleanText(ctx, item.Summary)
	tr := c.newTrace(ev, "retriage", cleaned)
	tr.ThreadKey = item.ThreadKey
	relevant := true
	tr.Stage1Relevant = &relevant
	in := ClassifyInput{ThreadKey: item.ThreadKey, Source: connectorOf(ev), Author: ev.UserID, Text: cleaned}
	return c.finishItem(ctx, in, tr, c.now(), ev)
}

// feedItemToEvent reconstructs the InboundEvent a feed item came from, enough for
// writeFeed (source context) and matchExistingTask (connector + thread). The
// thread_key encodes channel + thread anchor: "<channel>:<thread_ts>" for Slack,
// "<owner/repo>:<gh-pr|gh-issue:owner/repo#N>" for GitHub — split on the FIRST
// colon so the GitHub link tag (which itself contains colons) stays intact as the
// thread anchor.
func feedItemToEvent(item flowdb.FeedItem) monitor.InboundEvent {
	channel, anchor := splitThreadKeyFirst(item.ThreadKey)
	ts := strings.TrimSpace(item.TS)
	if ts == "" {
		ts = anchor
	}
	return monitor.InboundEvent{
		Kind:        "message",
		Channel:     channel,
		ChannelType: item.ChannelType,
		ThreadTS:    anchor,
		TS:          ts,
		UserID:      item.Author,
		Text:        item.Summary,
		TeamID:      item.TeamID,
		URL:         item.URL,
	}
}

func splitThreadKeyFirst(key string) (channel, anchor string) {
	key = strings.TrimSpace(key)
	if i := strings.Index(key, ":"); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}
