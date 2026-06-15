package server

import (
	"database/sql"
	"flow/internal/agents"
	"flow/internal/flowdb"
	"sort"
	"strconv"
	"strings"
	"time"
)

func latestTaskActivity(tv TaskView) string {
	latest := tv.UpdatedAt
	for _, f := range tv.Updates {
		if f.MTime > latest {
			latest = f.MTime
		}
	}
	return latest
}

func laterTimestamp(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	at, aErr := time.Parse(time.RFC3339, a)
	bt, bErr := time.Parse(time.RFC3339, b)
	if aErr == nil && bErr == nil {
		if bt.After(at) {
			return b
		}
		return a
	}
	if b > a {
		return b
	}
	return a
}

// localDay parses an RFC3339 timestamp and returns its local calendar day as
// YYYY-MM-DD, or "" if the timestamp is empty/unparseable. Used to bucket
// per-turn token usage into the days the heatmap and token-series grids use.
func localDay(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return t.In(time.Local).Format("2006-01-02")
}

// buildTokenSeries sums per-day tokens (input+output+cache_creation, cache
// reads excluded — processedTokens; see transcriptUsageStats.TokensByDay)
// across every tracked session into a 12-week
// daily grid — the token-cost-over-time trend. It reuses the cached transcript
// usage, so for sessions already parsed this tick the lookups are warm; done
// sessions never change and stay cached. The window and Sunday alignment match
// buildActivityHeatmap so the two dashboards line up day-for-day.
func (s *Server) buildTokenSeries(tasks []TaskView, now time.Time) ([]uiTokenDay, []uiTopTask, []uiModelCount) {
	now = now.In(time.Local)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekdayOffset := int(today.Weekday())
	thisWeekSunday := today.AddDate(0, 0, -weekdayOffset)
	start := thisWeekSunday.AddDate(0, 0, -77) // 11 prior weeks + this week = 12
	days := make([]uiTokenDay, 84)
	index := make(map[string]int, len(days))
	// perTask[dayIndex][taskLabel] = fresh work tokens that task burned that day,
	// for the activity bar's per-task tooltip breakdown. perTaskCost is the
	// matching estimated dollar cost, so the tooltip can show "$ per task" too.
	perTask := make([]map[string]int, 84)
	perTaskCost := make([]map[string]float64, 84)
	for i := range days {
		date := start.AddDate(0, 0, i).Format("2006-01-02")
		days[i] = uiTokenDay{Date: date}
		index[date] = i
	}
	// One session may back several task rows (e.g. a worktree clone); dedupe by
	// resolved transcript path so a day's tokens aren't double-counted.
	seen := map[string]bool{}
	// topAgg accumulates each task's window-total tokens + cost (across all 84
	// days) for the leaderboard, keyed by slug so names that repeat don't merge.
	topAgg := map[string]*uiTopTask{}
	// modelCounts tallies which model done tasks actually ran on, read from the
	// transcript (entry.usage.Model) rather than the mostly-empty task pin.
	modelCounts := map[string]int{}
	for _, tv := range tasks {
		if tv.SessionID == nil || strings.TrimSpace(*tv.SessionID) == "" {
			continue
		}
		provider := "claude"
		if tv.SessionProvider != nil && *tv.SessionProvider != "" {
			provider = *tv.SessionProvider
		}
		task := &flowdb.Task{
			Slug:            tv.Slug,
			WorkDir:         tv.WorkDir,
			WorktreePath:    nullStringFromPtr(tv.WorktreePath),
			SessionProvider: provider,
			SessionID:       sql.NullString{String: *tv.SessionID, Valid: true},
			SessionPath:     nullStringFromPtr(tv.SessionPath),
		}
		path, err := sessionJSONLPath(s.cfg.DB, task)
		if err != nil || path == "" || seen[path] {
			continue
		}
		seen[path] = true
		entry, err := s.transcripts.get(path)
		if err != nil {
			continue
		}
		// Real model the session ran on (from the transcript), for the
		// Composition card's Model bar — only for closed tasks.
		if tv.Status == "done" {
			if m := strings.TrimSpace(entry.usage.Model); m != "" {
				modelCounts[m]++
			}
		}
		label := strings.TrimSpace(tv.Name)
		if label == "" {
			label = tv.Slug
		}
		var sessTokens int
		var sessCost float64
		for day, tok := range entry.usage.TokensByDay {
			i, ok := index[day]
			if !ok || tok <= 0 {
				continue
			}
			days[i].Tokens += tok
			sessTokens += tok
			if perTask[i] == nil {
				perTask[i] = map[string]int{}
			}
			perTask[i][label] += tok
			if cost := entry.usage.CostByDay[day]; cost > 0 {
				days[i].CostUSD += cost
				sessCost += cost
				if perTaskCost[i] == nil {
					perTaskCost[i] = map[string]float64{}
				}
				perTaskCost[i][label] += cost
			}
		}
		// Roll the session's window totals into the leaderboard accumulator. A
		// slug seen across multiple task rows is already deduped by `seen[path]`,
		// so this adds each transcript's tokens exactly once.
		if sessTokens > 0 || sessCost > 0 {
			agg := topAgg[tv.Slug]
			if agg == nil {
				agg = &uiTopTask{Slug: tv.Slug, Name: label, Provider: provider}
				topAgg[tv.Slug] = agg
			}
			agg.Tokens += sessTokens
			agg.CostUSD += sessCost
		}
	}
	// Finalize each day's per-task breakdown: total contributing tasks plus the
	// top contributors (by tokens) for the tooltip. Cap matches the heatmap's
	// 5-task tooltip list; TaskCount drives the "+N more" affordance.
	const topN = 5
	for i := range days {
		byTask := perTask[i]
		if len(byTask) == 0 {
			continue
		}
		ranked := make([]uiTokenTask, 0, len(byTask))
		for name, tok := range byTask {
			ranked = append(ranked, uiTokenTask{Name: name, Tokens: tok, CostUSD: perTaskCost[i][name]})
		}
		sort.Slice(ranked, func(a, b int) bool {
			if ranked[a].Tokens != ranked[b].Tokens {
				return ranked[a].Tokens > ranked[b].Tokens
			}
			return ranked[a].Name < ranked[b].Name
		})
		days[i].TaskCount = len(ranked)
		if len(ranked) > topN {
			ranked = ranked[:topN]
		}
		days[i].Tasks = ranked
	}
	// Rank the leaderboard by estimated cost (the bill is the decision-driver),
	// then tokens, then name; keep the top few for the "Top tasks by cost" card.
	topTasks := make([]uiTopTask, 0, len(topAgg))
	for _, t := range topAgg {
		topTasks = append(topTasks, *t)
	}
	sort.Slice(topTasks, func(a, b int) bool {
		if topTasks[a].CostUSD != topTasks[b].CostUSD {
			return topTasks[a].CostUSD > topTasks[b].CostUSD
		}
		if topTasks[a].Tokens != topTasks[b].Tokens {
			return topTasks[a].Tokens > topTasks[b].Tokens
		}
		return topTasks[a].Name < topTasks[b].Name
	})
	const maxTop = 8
	if len(topTasks) > maxTop {
		topTasks = topTasks[:maxTop]
	}
	modelMix := make([]uiModelCount, 0, len(modelCounts))
	for m, c := range modelCounts {
		modelMix = append(modelMix, uiModelCount{Model: m, Count: c})
	}
	sort.Slice(modelMix, func(a, b int) bool {
		if modelMix[a].Count != modelMix[b].Count {
			return modelMix[a].Count > modelMix[b].Count
		}
		return modelMix[a].Model < modelMix[b].Model
	})
	return days, topTasks, modelMix
}

func buildActivityHeatmap(tasks []TaskView, now time.Time) []uiActivityDay {
	now = now.In(time.Local)
	// Align the grid to a Sunday-start so the row labels (Mon/Wed/Fri)
	// are correct regardless of what day "today" is. End on the current
	// week's Saturday — future days in this week get count=0.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekdayOffset := int(today.Weekday()) // Sun=0..Sat=6
	thisWeekSunday := today.AddDate(0, 0, -weekdayOffset)
	start := thisWeekSunday.AddDate(0, 0, -77) // 11 prior full weeks + this week = 12 weeks
	days := make([]uiActivityDay, 84)
	index := make(map[string]int, len(days))
	seenTasks := make(map[string]map[string]bool)
	// Count is the number of DISTINCT tasks we actually WORKED ON each day — not
	// tasks that merely exist. We deliberately do NOT count created_at/updated_at:
	// the attention router auto-creates dozens of triage cards (Slack mentions, PR
	// events) whose created_at all land on one day, and updated_at gets bumped by
	// background machinery (linking a PR, tag/waiting changes) — both inflate the
	// number with tasks nobody touched. Real work signals only: a session
	// started/resumed, a progress note (update file) written, or live right now.
	distinct := make(map[string]map[string]bool)
	for i := range days {
		date := start.AddDate(0, 0, i).Format("2006-01-02")
		days[i] = uiActivityDay{Date: date}
		index[date] = i
	}
	// distinct counting is keyed by slug (stable, collision-free); the displayed
	// task list uses the human name so the tooltip never leaks a raw slug
	// (e.g. slack-<channel>-<ts>) — see the "no raw Slack IDs in UI" rule.
	add := func(ts time.Time, slug, name string) {
		date := ts.In(time.Local).Format("2006-01-02")
		if _, ok := index[date]; !ok {
			return
		}
		if distinct[date] == nil {
			distinct[date] = map[string]bool{}
		}
		distinct[date][slug] = true
		if seenTasks[date] == nil {
			seenTasks[date] = map[string]bool{}
		}
		if !seenTasks[date][slug] && len(days[index[date]].Tasks) < 5 {
			label := name
			if label == "" {
				label = slug
			}
			days[index[date]].Tasks = append(days[index[date]].Tasks, label)
			seenTasks[date][slug] = true
		}
	}
	addString := func(ts string, slug, name string) {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			add(t, slug, name)
		}
	}
	for _, task := range tasks {
		if task.SessionStarted != nil {
			addString(*task.SessionStarted, task.Slug, task.Name)
		}
		if task.SessionLastResumed != nil {
			addString(*task.SessionLastResumed, task.Slug, task.Name)
		}
		for _, update := range task.Updates {
			if t, ok := activityTimeForFile(update); ok {
				add(t, task.Slug, task.Name)
			}
		}
		// Only a genuinely live session counts as activity "now". Merely being
		// in-progress (open but idle, last touched days ago) must NOT light up
		// today — that falsely inflates today's count with stale tasks. Real
		// created/updated/session/update timestamps already place those tasks on
		// the days they were actually touched.
		if task.Live {
			add(now, task.Slug, task.Name)
		}
	}
	for i := range days {
		days[i].Count = len(distinct[days[i].Date])
	}
	return days
}

// buildUIStats derives the Mission Control analytics strip: activity-day streaks
// from the heatmap, plus per-provider context-token and session totals across
// every tracked session (live + done + chats, deduped by slug). Chats are
// flow-launched sessions too, so their token/cost burn folds into the same
// per-provider panel rather than being silently excluded.
func buildUIStats(live, done, chats []uiAgent, heatmap []uiActivityDay, now time.Time) uiStats {
	var st uiStats
	today := now.In(time.Local).Format("2006-01-02")
	// Restrict to days up to and including today. The heatmap grid runs to the
	// end of the current week, so the trailing future days (count 0) would
	// otherwise read as a broken streak.
	onByDay := make([]bool, 0, len(heatmap))
	for _, d := range heatmap {
		if d.Date > today {
			continue
		}
		on := d.Count > 0
		onByDay = append(onByDay, on)
		if on {
			st.ActiveDays++
		}
	}
	// Longest run of consecutive active days anywhere in the window.
	run := 0
	for _, on := range onByDay {
		if on {
			run++
			if run > st.LongestStreak {
				st.LongestStreak = run
			}
		} else {
			run = 0
		}
	}
	// Current streak: count back from today. A not-yet-active today doesn't break
	// the streak (you may simply not have started yet) — skip it once, then count
	// consecutive active days backwards.
	for i := len(onByDay) - 1; i >= 0; i-- {
		if onByDay[i] {
			st.CurrentStreak++
		} else if i == len(onByDay)-1 {
			continue // grace: today hasn't been touched yet
		} else {
			break
		}
	}
	// Token + session totals per provider, deduped by slug so a session present
	// in both the live and done lists isn't counted twice.
	seen := make(map[string]bool)
	tally := func(list []uiAgent) {
		for _, a := range list {
			if a.Slug == "" || seen[a.Slug] {
				continue
			}
			seen[a.Slug] = true
			// Sum TokensSession (cumulative "work done" per session, cache-excluded)
			// so this panel equals the SUM of the per-session "tok" pills and the
			// two finally correlate. (TokensUsed is just the current context-window
			// occupancy shown on each card's X/max bar — a different axis.)
			st.TokensTotal += a.TokensSession
			st.CostTotal += a.CostSession
			st.SessionsTotal++
			// Every session is exactly one of codex / claude: the builder defaults
			// Provider to "claude" when unset and codex is always explicit, so the
			// else-branch correctly owns claude and any unexpected value.
			if strings.EqualFold(strings.TrimSpace(a.Provider), agents.ProviderCodex) {
				st.TokensCodex += a.TokensSession
				st.CostCodex += a.CostSession
				st.SessionsCodex++
			} else {
				st.TokensClaude += a.TokensSession
				st.CostClaude += a.CostSession
				st.SessionsClaude++
			}
		}
	}
	tally(live)
	tally(done)
	tally(chats)
	return st
}

func activityTimeForFile(file FileRef) (time.Time, bool) {
	if len(file.Filename) >= len("2006-01-02") {
		if t, err := time.ParseInLocation("2006-01-02", file.Filename[:10], time.Local); err == nil {
			return t.Add(12 * time.Hour), true
		}
	}
	if t, err := time.Parse(time.RFC3339, file.MTime); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func minutesSince(ts string) int {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	min := int(time.Since(t) / time.Minute)
	if min < 0 {
		return 0
	}
	return min
}

func secondsSince(ts string) int {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	sec := int(time.Since(t) / time.Second)
	if sec < 0 {
		return 0
	}
	return sec
}

func formatActivity(seconds int) string {
	switch {
	case seconds < 60:
		return strconv.Itoa(seconds) + "s ago"
	case seconds < 3600:
		return strconv.Itoa(seconds/60) + "m ago"
	case seconds < 86400:
		return strconv.Itoa(seconds/3600) + "h ago"
	default:
		return strconv.Itoa(seconds/86400) + "d ago"
	}
}

// toolCallActivitySeries returns a 60-cell activity series for the agent
// tile, where each cell counts transcript entries observed in the
// corresponding minute of the last hour. Cell 0 is 59 minutes ago;
// cell 59 is the current minute. Anything older than 60 minutes is
// dropped. Every timestamped entry counts (tool calls, assistant/user
// turns, tool results) so the strip is meaningful for both Claude and
// Codex — Codex transcripts rarely surface as discrete tool_use events.
func toolCallActivitySeries(transcript []uiTranscript, now time.Time) []int {
	out := make([]int, 60)
	if len(transcript) == 0 {
		return out
	}
	// Anchor the window to the session's most recent activity rather than
	// wall-clock now. A session that crashed or went idle days ago still shows
	// the bars from its final active hour instead of an empty strip — the tile
	// chart is meaningful regardless of how long ago the session ran.
	anchor := time.Time{}
	for _, e := range transcript {
		if strings.TrimSpace(e.Time) == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, e.Time); err == nil && t.After(anchor) {
			anchor = t
		}
	}
	if anchor.IsZero() || anchor.After(now) {
		anchor = now
	}
	cutoff := anchor.Add(-time.Duration(len(out)) * time.Minute)
	for _, e := range transcript {
		if strings.TrimSpace(e.Time) == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.Time)
		if err != nil {
			continue
		}
		if t.Before(cutoff) || t.After(anchor.Add(time.Minute)) {
			continue
		}
		minutesAgo := int(anchor.Sub(t) / time.Minute)
		if minutesAgo < 0 {
			minutesAgo = 0
		}
		if minutesAgo >= len(out) {
			continue
		}
		out[len(out)-1-minutesAgo]++
	}
	return out
}

func lastSevenFromThirty(in []int) []int {
	if len(in) >= 7 {
		return append([]int(nil), in[len(in)-7:]...)
	}
	out := make([]int, 7)
	copy(out[7-len(in):], in)
	return out
}
