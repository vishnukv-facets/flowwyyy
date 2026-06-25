package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// transcriptCache memoizes derived contents of a session jsonl file
// keyed by (path, mtime, size). The UI tick calls into three different
// derivations — the recent-messages list, the token-usage stats, and the
// pending-user-input check — and pre-cache they all independently read
// and JSON-decode the entire file on every tick. For done sessions
// (the common case in any active flow user's task list) the file never
// changes, so a cache keyed on stat metadata is a near-perfect fit:
// 100% hit rate after the first miss, with the stat as the
// invalidation mechanism.
//
// All three outputs are produced from a single pass over the file
// during populate, so a cache miss costs one open + one line scan
// instead of three. The transcript text itself is capped to the recent
// tail; usage and pending-input stats still scan the full file.
type transcriptCache struct {
	mu sync.RWMutex
	m  map[string]*transcriptCacheEntry
}

type transcriptCacheEntry struct {
	mtime time.Time
	size  int64

	// entries is intentionally a bounded tail. Some long sessions have
	// enormous transcript files; keeping every parsed text/tool/result in
	// memory made a small /api/ui-data response retain gigabytes of heap.
	entries []TranscriptEntry
	usage   transcriptUsageStats
	pending *codexPendingUserInput
}

const transcriptCacheEntryLimit = 200

func newTranscriptCache() *transcriptCache {
	return &transcriptCache{m: map[string]*transcriptCacheEntry{}}
}

// get returns a cache entry for path. Stats the file (cheap), returns
// the cached entry when mtime and size both match, otherwise repopulates.
// Concurrent callers may briefly populate twice on a miss — accepted
// because the result is deterministic and the duplicate write is
// harmless.
func (c *transcriptCache) get(path string) (*transcriptCacheEntry, error) {
	if c == nil {
		return populateTranscriptCacheEntry(path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	cached, ok := c.m[path]
	c.mu.RUnlock()
	if ok && cached.mtime.Equal(st.ModTime()) && cached.size == st.Size() {
		return cached, nil
	}
	entry, err := populateTranscriptCacheEntry(path)
	if err != nil {
		return nil, err
	}
	entry.mtime = st.ModTime()
	entry.size = st.Size()
	c.mu.Lock()
	c.m[path] = entry
	c.mu.Unlock()
	return entry, nil
}

// populateTranscriptCacheEntry does a single pass over a jsonl file,
// producing all three derived outputs (transcript entries, token usage
// stats, pending user-input state). Splitting these across three
// separate scans — as the original free functions did — is what made
// the per-tick CPU cost N×; consolidating to one scan is what makes
// the cache miss cheap enough to absorb invisibly when a live session
// appends a record.
func populateTranscriptCacheEntry(path string) (*transcriptCacheEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	entry := &transcriptCacheEntry{}
	pending := map[string]codexPendingUserInput{}
	var offset int64
	seq := 0

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		lineOffset := offset
		offset += int64(len(line)) + 1
		seq++
		if len(line) == 0 {
			continue
		}
		// 1) Recent transcript entries (existing dispatch handles both
		//    Claude and Codex shapes). Keep only the tail; display,
		//    terminal preview, last-action, and activity strips only need
		//    recent rows, while usage stats below still scan everything.
		if parsed := parseTranscriptLine(line, lineOffset); len(parsed) > 0 {
			entry.entries = appendTranscriptTail(entry.entries, parsed, transcriptCacheEntryLimit)
		}
		// 2) Token usage / model — same logic as sessionTranscriptUsageStats
		//    but inlined to avoid a second file scan.
		accumulateTranscriptUsage(&entry.usage, line)
		// 3) Pending user-input tracking — same logic as
		//    pendingCodexUserInput; the map is reduced to a single
		//    latest entry after the scan.
		accumulatePendingCodexUserInput(pending, line, seq)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var latest *codexPendingUserInput
	for _, item := range pending {
		if latest == nil || item.Seq > latest.Seq {
			copy := item
			latest = &copy
		}
	}
	entry.pending = latest
	return entry, nil
}

func appendTranscriptTail(tail, parsed []TranscriptEntry, limit int) []TranscriptEntry {
	if limit <= 0 {
		return append(tail, parsed...)
	}
	tail = append(tail, parsed...)
	if len(tail) <= limit {
		return tail
	}
	over := len(tail) - limit
	copy(tail, tail[over:])
	for i := limit; i < len(tail); i++ {
		tail[i] = TranscriptEntry{}
	}
	return tail[:limit]
}

// accumulateTranscriptUsage processes one jsonl line for token-usage
// stats. Lifted from the original sessionTranscriptUsageStats inner
// loop so the single-pass populator can call it without a duplicate
// open/scan. Errors on unmarshalling are silently skipped — the
// original function had the same forgiving behavior (transcripts
// contain mixed-shape records and Codex/Claude metadata lines).
func accumulateTranscriptUsage(stats *transcriptUsageStats, line []byte) {
	var rec transcriptUsageRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return
	}
	stats.LastTimestamp = laterTimestamp(stats.LastTimestamp, rec.Timestamp)
	// Keep the last REAL model. Claude Code emits trailing "<synthetic>" records
	// (session-limit / interrupt notices) carrying a placeholder model and zero
	// usage; letting those win collapses contextWindowForModel to the 200k default
	// and a 1M-context Opus session reads as a bogus 100% context occupancy.
	if m := strings.TrimSpace(rec.Message.Model); m != "" && !strings.HasPrefix(m, "<") {
		stats.Model = m
	}
	if used := rec.Message.Usage.total(); used > 0 {
		stats.TokensUsed = used // context occupancy = latest turn's full total
	}
	// Dedup: Claude Code appends intermediate AND final usage snapshots for the
	// SAME request (same message.id + requestId, identical counts). Counting
	// every line double-counts work and cost ~2-3x on a long session. Count each
	// request once (first snapshot wins). An empty key (Codex events, or test
	// lines without ids) is never a dup, so it always counts.
	dup := false
	if key := rec.usageDedupKey(); key != "" {
		if _, seen := stats.claudeSeen[key]; seen {
			dup = true
		} else {
			if stats.claudeSeen == nil {
				stats.claudeSeen = map[string]struct{}{}
			}
			stats.claudeSeen[key] = struct{}{}
		}
	}
	if !dup {
		accumulateTranscriptLookupEvents(stats, rec)
		// Session tokens = cumulative input + output + cache CREATION, EXCLUDING
		// cache reads — the basis Claude Code's /stats uses (see processedTokens).
		fresh := rec.Message.Usage.processedTokens()
		stats.TokensSession += fresh
		if day := localDay(rec.Timestamp); day != "" {
			// Per-day tokens, same basis as TokensSession.
			if fresh > 0 {
				if stats.TokensByDay == nil {
					stats.TokensByDay = map[string]int{}
				}
				stats.TokensByDay[day] += fresh
			}
			// Per-day cost is the FULL bill (cache reads + creation included),
			// priced at this turn's own model rate — that's what makes the
			// dollar figure track Claude Code's /cost, since cache reads
			// dominate a long session's bill.
			if cost := rec.Message.Usage.billedCostUSD(modelTokenRate(stats.Model)); cost > 0 {
				if stats.CostByDay == nil {
					stats.CostByDay = map[string]float64{}
				}
				stats.CostByDay[day] += cost
			}
		}
	}
	if rec.Payload == nil {
		return
	}
	var payload struct {
		Type  string `json:"type"`
		Model string `json:"model"` // Codex turn_context records carry the active model here
		Info  struct {
			LastTokenUsage     transcriptTokenUsage `json:"last_token_usage"`
			TotalTokenUsage    transcriptTokenUsage `json:"total_token_usage"`
			ModelContextWindow int                  `json:"model_context_window"`
		} `json:"info"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return
	}
	// Codex emits its model in a turn_context record that precedes the per-turn
	// token_count events, so capture it here before the token_count gate below.
	if m := strings.TrimSpace(payload.Model); m != "" {
		stats.Model = m
	}
	if payload.Type != "token_count" {
		return
	}
	if used := payload.Info.LastTokenUsage.total(); used > 0 {
		stats.TokensUsed = used
	} else if used := payload.Info.TotalTokenUsage.total(); used > 0 {
		stats.TokensUsed = used
	}
	if fresh := payload.Info.TotalTokenUsage.processedTokens(); fresh > 0 {
		firstEvent := stats.lastCodexFreshTotal == 0
		stats.TokensSession = fresh // Codex: running total (no cache-creation)
		delta := fresh
		if !firstEvent {
			delta = fresh - stats.lastCodexFreshTotal
			if delta < 0 {
				delta = fresh
			}
		}
		stats.lastCodexFreshTotal = fresh
		// Derive per-event input/output deltas the same way, so the higher
		// Codex output rate is applied to the right portion. On a running-total
		// reset (negative delta) fall back to the full current component.
		freshIn := payload.Info.TotalTokenUsage.freshInput()
		freshOut := payload.Info.TotalTokenUsage.freshOutput()
		deltaIn, deltaOut := freshIn, freshOut
		if !firstEvent {
			if d := freshIn - stats.lastCodexFreshInput; d >= 0 {
				deltaIn = d
			}
			if d := freshOut - stats.lastCodexFreshOutput; d >= 0 {
				deltaOut = d
			}
		}
		stats.lastCodexFreshInput = freshIn
		stats.lastCodexFreshOutput = freshOut
		// Cached input is billed too (0.1x input — e.g. gpt-5.5's $0.50/MTok),
		// just excluded from the work metric. Track the running cached total so
		// its per-event delta can be priced at the cache-read rate.
		cachedNow := payload.Info.TotalTokenUsage.cacheReadTokens()
		deltaCached := cachedNow
		if !firstEvent {
			if d := cachedNow - stats.lastCodexCached; d >= 0 {
				deltaCached = d
			}
		}
		stats.lastCodexCached = cachedNow
		if delta > 0 {
			if day := localDay(rec.Timestamp); day != "" {
				if stats.TokensByDay == nil {
					stats.TokensByDay = map[string]int{}
				}
				stats.TokensByDay[day] += delta
				rate := modelTokenRate(stats.Model)
				cost := turnCostUSD(deltaIn, deltaOut, rate) + cacheReadCostUSD(deltaCached, rate)
				if cost > 0 {
					if stats.CostByDay == nil {
						stats.CostByDay = map[string]float64{}
					}
					stats.CostByDay[day] += cost
				}
			}
		}
	}
	if payload.Info.ModelContextWindow > 0 {
		stats.TokensMax = payload.Info.ModelContextWindow
	}
}

// accumulatePendingCodexUserInput processes one jsonl line and updates
// the pending-call map. Replicates the order-dependent logic from
// pendingCodexUserInput so the single-pass populator can share the
// scan.
func accumulatePendingCodexUserInput(pending map[string]codexPendingUserInput, line []byte, seq int) {
	rec, ok := codexPayloadRecord(line)
	if !ok {
		return
	}
	switch rec.Type {
	case "message":
		if rec.Role == "user" {
			// A user message resets the pending state — any
			// outstanding elicitation is no longer waiting.
			for k := range pending {
				delete(pending, k)
			}
		}
	case "function_call":
		if !codexRequestUserInputTool(rec.Name) {
			return
		}
		for k := range pending {
			delete(pending, k)
		}
		callID := strings.TrimSpace(rec.CallID)
		if callID == "" {
			callID = fmt.Sprintf("offset-%d", seq)
		}
		question := codexUserInputQuestion(rec.Arguments)
		if question == "" {
			question = "The Codex session is waiting for your input."
		}
		pending[callID] = codexPendingUserInput{
			CallID:    callID,
			Timestamp: rec.Timestamp,
			Question:  question,
			RawJSON:   string(line),
			Seq:       seq,
		}
	case "function_call_output":
		if callID := strings.TrimSpace(rec.CallID); callID != "" {
			delete(pending, callID)
		}
	}
}
