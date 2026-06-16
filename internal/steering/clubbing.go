// internal/steering/clubbing.go
package steering

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// clubCandidateWindow bounds how far back the clubbing candidate search looks:
// only open cards from the same channel within this window are considered for
// merging. Wide enough to cover a conversation that spans a workday, narrow
// enough that yesterday's resolved-but-still-open topic doesn't absorb a new
// one. The matcher is the real guard against over-merging; this just bounds cost.
const clubCandidateWindow = 12 * time.Hour

// clubCandidateLimit caps how many open cards are handed to the matcher per
// message, keeping the prompt small under a busy channel.
const clubCandidateLimit = 10

// conversationGap is the maximum time between two messages in a 1:1 DM for them
// to count as the same ongoing conversation. DM messages are clubbed by
// time+participant, not semantics: each is triaged in isolation so its summary
// is context-free ("bare 'lgtm', no PR reference") and can't be linked by an
// LLM — but a burst from the same person minutes apart is plainly one
// conversation, while a longer pause is a fresh one.
const conversationGap = 30 * time.Minute

// chanTypeDM is the channel_type for a 1:1 Slack direct message.
const chanTypeDM = "im"

// chanTypeMPDM is the channel_type for a multi-person Slack direct message.
const chanTypeMPDM = "mpim"

// isDirectChannelType reports whether a channel is a DM or MPDM — one ongoing
// conversation per channel, where messages are NOT threaded so every top-level
// message anchors its own thread_key.
func isDirectChannelType(t string) bool {
	return t == chanTypeDM || t == chanTypeMPDM
}

// clubbedThreadKeyForReply recovers the clubbed card's thread_key for a
// self-authored DM/MPDM reply whose own (un-threaded) key never matches the card.
// In a DM/MPDM the whole channel is one conversation, so the most recent open
// card in the channel within the conversation gap is the owning card. Returns
// ("", false) for non-DM channels (threaded replies already match by raw key) or
// when there's no recent open card. Deterministic — no matcher call — so it's
// cheap to run on every self-authored reply that missed its raw key.
func (c *Cascade) clubbedThreadKeyForReply(ev monitor.InboundEvent) (string, bool) {
	if !isDirectChannelType(ev.ChannelType) {
		return "", false
	}
	rawKey := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	since := c.now().Add(-clubCandidateWindow).UTC().Format(time.RFC3339)
	cands, err := flowdb.ListOpenClubCandidates(c.DB, ev.Channel, rawKey, since, clubCandidateLimit)
	if err != nil || len(cands) == 0 {
		return "", false
	}
	top := cands[0] // newest-first
	if !feedTSWithinGap(ev.TS, top.TS, conversationGap) {
		return "", false
	}
	return top.ThreadKey, true
}

// Context-aware card clubbing groups attention cards that belong to the SAME
// ongoing conversation even when the deterministic thread_key cannot: in DMs
// every top-level message anchors its own thread_key, and in channels people
// often continue a conversation in a fresh top-level message rather than a
// threaded reply. 1:1 DMs club deterministically by time+participant; multi-
// person channels use a cheap Haiku "same conversation?" judgment. Either way
// the cascade rewrites the incoming card's thread_key to the matched card's, so
// the existing coalesce machinery merges them into one card.

// ClubMessage is the incoming standalone message being considered for clubbing.
type ClubMessage struct {
	Author      string
	Text        string
	ChannelType string
}

// ClubCandidate is one existing open card the incoming message might continue.
type ClubCandidate struct {
	ThreadKey string
	Author    string
	Summary   string
	TS        string
}

// ConversationMatcher decides whether msg continues one of candidates (the same
// conversation/topic). It returns the matched candidate's thread_key plus a
// refreshed combined summary, or ("", "", nil) for a new conversation. An error
// means "could not decide" — the caller fails open and surfaces a fresh card.
type ConversationMatcher func(ctx context.Context, msg ClubMessage, candidates []ClubCandidate) (matchedThreadKey, summary string, err error)

// isStandaloneThreadKey reports whether threadKey anchors its own message
// (channel:ts) rather than a parent thread. Standalone messages — every DM
// message and every fresh top-level channel post — can't coalesce via
// thread_key, so they are the ones clubbing considers. A genuine threaded reply
// has thread_key = channel:parent_ts != channel:ts, and is left to the
// deterministic thread_key coalesce.
func isStandaloneThreadKey(threadKey, channel, ts string) bool {
	if channel == "" || ts == "" {
		return false
	}
	return threadKey == channel+":"+ts
}

// maybeClub rewrites item.ThreadKey (and Summary) to merge the incoming message
// into an existing open card for the same conversation. It is a no-op — leaving
// item untouched so the normal insert path runs — for threaded replies,
// channel-less items, no candidates, or any error (fail open: a clubbing
// failure must never drop a card). 1:1 DMs club deterministically by time gap;
// other channels consult the LLM matcher.
func (c *Cascade) maybeClub(ctx context.Context, item *flowdb.FeedItem) {
	if !isStandaloneThreadKey(item.ThreadKey, item.Channel, item.TS) {
		return
	}
	since := c.now().Add(-clubCandidateWindow).UTC().Format(time.RFC3339)
	cands, err := flowdb.ListOpenClubCandidates(c.DB, item.Channel, item.ThreadKey, since, clubCandidateLimit)
	if err != nil {
		c.log("clubbing: candidate lookup failed for %s: %v", item.ThreadKey, err)
		return
	}
	if len(cands) == 0 {
		return
	}

	var matchedKey, summary string
	if item.ChannelType == chanTypeDM {
		// 1:1 DM: merge into the most recent open card (cands is newest-first)
		// when within the conversation gap; a longer pause is a new conversation.
		// Preserve that card's summary — the framing message usually summarizes
		// the thread best; later fragments ("lgtm") would only degrade it.
		top := cands[0]
		if feedTSWithinGap(item.TS, top.TS, conversationGap) {
			matchedKey, summary = top.ThreadKey, top.Summary
		}
	} else {
		if c.MatchConversation == nil {
			return
		}
		candidates := toClubCandidates(cands)
		var mErr error
		matchedKey, summary, mErr = c.MatchConversation(ctx, ClubMessage{Author: item.Author, Text: item.Summary, ChannelType: item.ChannelType}, candidates)
		if mErr != nil {
			c.log("clubbing: matcher failed for %s: %v", item.ThreadKey, mErr)
			return
		}
	}
	if matchedKey == "" || matchedKey == item.ThreadKey {
		return
	}
	c.log("clubbing: merged %s into %s", item.ThreadKey, matchedKey)
	item.ThreadKey = matchedKey
	if summary != "" {
		item.Summary = SanitizeOperatorText(summary)
	}
}

// toClubCandidates maps feed rows to matcher candidates.
func toClubCandidates(rows []flowdb.FeedItem) []ClubCandidate {
	out := make([]ClubCandidate, len(rows))
	for i, cd := range rows {
		out[i] = ClubCandidate{ThreadKey: cd.ThreadKey, Author: cd.Author, Summary: cd.Summary, TS: cd.TS}
	}
	return out
}

// feedTSWithinGap reports whether two Slack message timestamps ("seconds.micros")
// are within gap of each other. Unparseable input returns false (fail safe: a
// non-numeric ts never deterministically clubs).
func feedTSWithinGap(a, b string, gap time.Duration) bool {
	fa, ea := strconv.ParseFloat(strings.TrimSpace(a), 64)
	fb, eb := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if ea != nil || eb != nil {
		return false
	}
	d := fa - fb
	if d < 0 {
		d = -d
	}
	return d <= gap.Seconds()
}

// DedupeResult reports what one cleanup pass did.
type DedupeResult struct {
	Examined int // standalone open cards considered (in channels with ≥2)
	Merged   int // cards collapsed into an anchor and dismissed
}

// DedupeOpenFeedConversations is the one-shot cleanup for duplicate cards that
// accumulated before live clubbing existed: cards from one conversation that
// fragmented because each standalone message anchored its own thread_key. Per
// channel it replays the open standalone cards oldest→newest and collapses
// same-conversation cards into the oldest (the anchor) — advancing the anchor's
// recency to the newest absorbed card and marking the redundant cards
// 'dismissed' (reversible, not deleted). 1:1 DMs collapse by the conversation
// time gap (no LLM); other channels use the matcher. Threaded-reply cards (which
// already coalesce) and channels with a single open card are left untouched.
// Fails open per card: any matcher or write error keeps that card as its own.
func (c *Cascade) DedupeOpenFeedConversations(ctx context.Context) (DedupeResult, error) {
	var res DedupeResult
	open, err := flowdb.ListFeedItems(c.DB, "new")
	if err != nil {
		return res, fmt.Errorf("steering: dedupe list open feed: %w", err)
	}
	byChannel := map[string][]flowdb.FeedItem{}
	for _, it := range open {
		if !isStandaloneThreadKey(it.ThreadKey, it.Channel, it.TS) {
			continue
		}
		byChannel[it.Channel] = append(byChannel[it.Channel], it)
	}
	now := c.now().UTC().Format(time.RFC3339)
	for channel, cards := range byChannel {
		if len(cards) < 2 {
			continue
		}
		// ListFeedItems is newest-first; replay oldest-first so the earliest
		// message of a conversation becomes its anchor (matching live behavior).
		sort.SliceStable(cards, func(i, j int) bool {
			if cards[i].CreatedAt != cards[j].CreatedAt {
				return cards[i].CreatedAt < cards[j].CreatedAt
			}
			return cards[i].ID < cards[j].ID
		})
		res.Examined += len(cards)
		if cards[0].ChannelType == chanTypeDM {
			res.Merged += c.dedupeDMByGap(cards, now)
		} else {
			res.Merged += c.dedupeChannelByMatcher(ctx, channel, cards, now)
		}
	}
	if res.Merged > 0 {
		c.log("dedupe: collapsed %d duplicate cards across conversations", res.Merged)
	}
	return res, nil
}

// dedupeDMByGap collapses a 1:1 DM channel's open cards (oldest-first) into
// bursts: each card within conversationGap of the running anchor's latest
// message folds into that anchor; a longer pause starts a new anchor. The
// anchor keeps its (framing) summary and advances its recency. Returns the
// number of cards dismissed.
func (c *Cascade) dedupeDMByGap(cards []flowdb.FeedItem, now string) int {
	merged := 0
	anchor := cards[0]
	anchorTS := anchor.TS
	for _, card := range cards[1:] {
		if !feedTSWithinGap(card.TS, anchorTS, conversationGap) {
			anchor = card // gap exceeded → this card opens a new conversation
			anchorTS = card.TS
			continue
		}
		bumped := anchor
		bumped.CreatedAt = card.CreatedAt // advance recency to the absorbed card
		if _, _, err := flowdb.UpsertFeedItemSurfaced(c.DB, bumped); err != nil {
			c.log("dedupe: bump anchor %s failed: %v", anchor.ID, err)
			anchor, anchorTS = card, card.TS
			continue
		}
		if err := flowdb.SetFeedItemStatus(c.DB, card.ID, "dismissed", now); err != nil {
			c.log("dedupe: dismiss %s failed: %v", card.ID, err)
			continue
		}
		anchor = bumped
		anchorTS = card.TS
		merged++
	}
	return merged
}

// dedupeChannelByMatcher collapses a multi-person channel's open cards
// (oldest-first) using the LLM matcher: each card is matched against the
// accumulated anchors, merging into one on a match or becoming a new anchor.
// Returns the number of cards dismissed.
func (c *Cascade) dedupeChannelByMatcher(ctx context.Context, channel string, cards []flowdb.FeedItem, now string) int {
	if c.MatchConversation == nil {
		return 0
	}
	merged := 0
	var anchors []flowdb.FeedItem
	for _, card := range cards {
		if len(anchors) == 0 {
			anchors = append(anchors, card)
			continue
		}
		matchedKey, summary, mErr := c.MatchConversation(ctx, ClubMessage{Author: card.Author, Text: card.Summary, ChannelType: card.ChannelType}, toClubCandidates(anchors))
		if mErr != nil {
			c.log("dedupe: matcher failed in %s: %v", channel, mErr)
			anchors = append(anchors, card)
			continue
		}
		ai := anchorIndex(anchors, matchedKey)
		if ai < 0 {
			anchors = append(anchors, card)
			continue
		}
		// Merge: refresh the anchor row's summary, advance its recency to the
		// absorbed card, then dismiss the duplicate. UpsertFeedItemSurfaced finds
		// the anchor by thread_key (still 'new') and updates it in place.
		anchor := anchors[ai]
		if summary != "" {
			anchor.Summary = SanitizeOperatorText(summary)
		}
		anchor.CreatedAt = card.CreatedAt
		if _, _, uErr := flowdb.UpsertFeedItemSurfaced(c.DB, anchor); uErr != nil {
			c.log("dedupe: bump anchor %s failed: %v", anchor.ID, uErr)
			anchors = append(anchors, card)
			continue
		}
		if dErr := flowdb.SetFeedItemStatus(c.DB, card.ID, "dismissed", now); dErr != nil {
			c.log("dedupe: dismiss %s failed: %v", card.ID, dErr)
			continue
		}
		anchors[ai] = anchor
		merged++
	}
	return merged
}

// anchorIndex returns the index of the anchor with the given thread_key, or -1.
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

// defaultConversationMatcher asks the cheap classifier whether the incoming
// message continues one of the candidate conversations. No candidates → no
// call, no match.
func defaultConversationMatcher(ctx context.Context, msg ClubMessage, candidates []ClubCandidate) (string, string, error) {
	if len(candidates) == 0 {
		return "", "", nil
	}
	raw, err := runClassifier(ctx, "club", clubPrime(), clubPayload(msg, candidates), "club-v1")
	if err != nil {
		return "", "", err
	}
	idx, summary, err := parseConversationMatch(raw, len(candidates))
	if err != nil {
		return "", "", err
	}
	if idx < 0 {
		return "", "", nil
	}
	return candidates[idx].ThreadKey, summary, nil
}

// parseConversationMatch extracts the club verdict from raw model output. It
// returns the matched candidate index in [0,n) or -1 for a new conversation,
// plus the combined summary. An index outside [0,n) (including a missing or
// negative "match") is normalized to -1: when the model is unsure we start a
// new card rather than merging into the wrong conversation. Returns an error
// only when no JSON object is present at all.
func parseConversationMatch(raw string, n int) (idx int, summary string, err error) {
	jsonText, err := extractJSON(raw)
	if err != nil {
		return -1, "", fmt.Errorf("steering: club parse: %w", err)
	}
	var v struct {
		Match   *int   `json:"match"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(jsonText), &v); err != nil {
		return -1, "", fmt.Errorf("steering: club unmarshal: %w", err)
	}
	if v.Match == nil || *v.Match < 0 || *v.Match >= n {
		return -1, "", nil
	}
	return *v.Match, strings.TrimSpace(v.Summary), nil
}

func clubPrime() string {
	return `MODE: club-conversation

You group an operator's incoming message with their existing open attention cards. Decide whether the NEW message continues the SAME ongoing conversation or topic as one of the existing cards, or starts a NEW one.

People often continue a conversation with a fresh top-level message instead of a threaded reply, so judge by participants and topic — not by threading. Be conservative: merge ONLY when it is clearly the same conversation or directly follows up on one card's topic. A merely similar theme is NOT a match; when in doubt, answer new (-1).

Always refer to people and channels by name; never output raw platform IDs.

Respond with ONLY a minified JSON object, no prose, no code fences:
{"match": <index of the matching card, or -1 for a new conversation>, "summary": "<= 140 char summary of the combined conversation when matched, else empty>"}`
}

func clubPayload(msg ClubMessage, candidates []ClubCandidate) string {
	type indexed struct {
		Index   int    `json:"index"`
		Author  string `json:"author,omitempty"`
		Summary string `json:"summary"`
	}
	cards := make([]indexed, len(candidates))
	for i, c := range candidates {
		cards[i] = indexed{Index: i, Author: c.Author, Summary: c.Summary}
	}
	cardsJSON, _ := json.Marshal(cards)
	in := ClubMessage{Author: msg.Author, Text: compactClassifierText(msg.Text), ChannelType: msg.ChannelType}
	msgJSON, _ := json.Marshal(in)
	return "New message (JSON):\n" + string(msgJSON) +
		"\n\nExisting open cards (JSON array; \"index\" is the value to return in \"match\"):\n" + string(cardsJSON)
}
