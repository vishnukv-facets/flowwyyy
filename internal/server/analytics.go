package server

import (
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"flow/internal/flowdb"
)

// bucketUnit is the calendar granularity of an analytics time bucket.
type bucketUnit string

const (
	bucketHour bucketUnit = "hour"
	bucketDay  bucketUnit = "day"
	bucketWeek bucketUnit = "week"
)

// rangeWindow maps a UI range token (1d/7d/15d/30d/6m) to a [from,to) window
// ending at now, plus the bucket granularity that token implies. ok is false
// for an unrecognized token, so the caller can fall back to explicit from/to
// or return a usage error.
func rangeWindow(now time.Time, rng string) (from, to time.Time, unit bucketUnit, ok bool) {
	to = now
	switch rng {
	case "1d":
		return now.Add(-24 * time.Hour), to, bucketHour, true
	case "7d":
		return now.AddDate(0, 0, -7), to, bucketDay, true
	case "15d":
		return now.AddDate(0, 0, -15), to, bucketDay, true
	case "30d":
		return now.AddDate(0, 0, -30), to, bucketDay, true
	case "6m":
		return now.AddDate(0, -6, 0), to, bucketWeek, true
	default:
		return time.Time{}, time.Time{}, "", false
	}
}

// unitForSpan picks a bucket granularity for an arbitrary custom span so the
// point count stays bounded regardless of range: <=2d hourly, <=90d daily,
// otherwise weekly.
func unitForSpan(d time.Duration) bucketUnit {
	switch {
	case d <= 48*time.Hour:
		return bucketHour
	case d <= 90*24*time.Hour:
		return bucketDay
	default:
		return bucketWeek
	}
}

// bucketGrid is a contiguous list of calendar-aligned bucket start times in
// time.Local covering [from,to). The final bucket is Partial when it contains
// "now" (its end is in the future) — that's the live, still-filling window the
// UI highlights and refreshes from the ui-data snapshot.
type bucketGrid struct {
	Unit    bucketUnit
	Starts  []time.Time
	Partial bool
}

// Len reports the number of buckets in the grid.
func (g bucketGrid) Len() int { return len(g.Starts) }

// indexOf returns the index of the bucket containing t, or -1 if t falls
// outside the grid. Buckets are half-open [start, nextStart); the last bucket's
// upper bound is its natural calendar end, so a timestamp at "now" lands in the
// partial bucket rather than being dropped.
func (g bucketGrid) indexOf(t time.Time) int {
	t = t.In(time.Local)
	for i, s := range g.Starts {
		if t.Before(s) {
			return -1 // starts are ascending: t precedes this and all later buckets
		}
		if t.Before(stepBucket(s, g.Unit)) {
			return i
		}
	}
	return -1
}

// bucketsFor builds the calendar-aligned grid for [from,to): from is aligned
// down to its bucket boundary, then buckets step by calendar unit (DST-safe via
// AddDate) up to but not past to.
//
// Weeks are Monday-aligned (ISO week). Note this intentionally differs from the
// Overview heatmap/token grid, which use Sunday alignment; the analytics page
// is a distinct surface and standardizes on Monday-start weeks.
func bucketsFor(from, to time.Time, unit bucketUnit, now time.Time) bucketGrid {
	g := bucketGrid{Unit: unit}
	for s := alignDown(from, unit); s.Before(to); s = stepBucket(s, unit) {
		g.Starts = append(g.Starts, s)
	}
	if n := len(g.Starts); n > 0 {
		g.Partial = stepBucket(g.Starts[n-1], unit).After(now)
	}
	return g
}

// alignDown truncates t down to the start of its bucket in time.Local.
func alignDown(t time.Time, unit bucketUnit) time.Time {
	t = t.In(time.Local)
	switch unit {
	case bucketHour:
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local)
	case bucketWeek:
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
		// Go weekdays: Sunday=0 .. Saturday=6, Monday=1. Days since Monday:
		offset := (int(d.Weekday()) + 6) % 7
		return d.AddDate(0, 0, -offset)
	default: // bucketDay
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	}
}

// stepBucket returns the start of the bucket immediately after the one
// beginning at s, stepping by calendar unit so DST transitions don't drift the
// day/week boundaries.
func stepBucket(s time.Time, unit bucketUnit) time.Time {
	switch unit {
	case bucketHour:
		return s.Add(time.Hour)
	case bucketWeek:
		return s.AddDate(0, 0, 7)
	default: // bucketDay
		return s.AddDate(0, 0, 1)
	}
}

// deltaPct is the percentage change from prev to curr, or nil when prev is zero
// — the period-over-period delta is undefined against an empty prior window, so
// the UI omits it rather than rendering Inf/NaN.
func deltaPct(curr, prev float64) *float64 {
	if prev == 0 {
		return nil
	}
	d := (curr - prev) / prev * 100
	return &d
}

// ---- wire contract: chart primitives ----

// Series is one time-bucketed chart: one or more lines over the same grid.
type Series struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Unit    string `json:"unit"`
	Stacked bool   `json:"stacked,omitempty"`
	Lines   []Line `json:"lines"`
}

// Line is one named data series; its Points align 1:1 with the bucket grid.
type Line struct {
	Key    string  `json:"key"`
	Label  string  `json:"label,omitempty"`
	Points []Point `json:"points"`
}

// Point is a single (bucket-start, value). T is a date ("2006-01-02") for
// day/week buckets and a full RFC3339 timestamp for hourly buckets.
type Point struct {
	T string  `json:"t"`
	V float64 `json:"v"`
}

// ---- activity domain (tasks) ----

// parseLocal parses an RFC3339 timestamp into time.Local, or the zero time if
// empty/unparseable (matches the localDay convention).
func parseLocal(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}
	}
	return t.In(time.Local)
}

// bucketLabel formats a bucket start for the wire: a date for day/week buckets,
// a full RFC3339 timestamp for hourly buckets.
func bucketLabel(t time.Time, unit bucketUnit) string {
	if unit == bucketHour {
		return t.Format(time.RFC3339)
	}
	return t.Format("2006-01-02")
}

// gridPoints pairs each bucket start with its accumulated value.
func gridPoints(g bucketGrid, vals []float64) []Point {
	pts := make([]Point, g.Len())
	for i, s := range g.Starts {
		pts[i] = Point{T: bucketLabel(s, g.Unit), V: vals[i]}
	}
	return pts
}

// activitySeries buckets task creation and completion into the created-vs-
// completed throughput series. A task counts as "created" in the bucket holding
// its created_at, and "completed" in the bucket holding its status_changed_at
// when status=done. Timestamps outside the window are skipped.
func activitySeries(tasks []*flowdb.Task, g bucketGrid) Series {
	created := make([]float64, g.Len())
	done := make([]float64, g.Len())
	for _, t := range tasks {
		if c := parseLocal(t.CreatedAt); !c.IsZero() {
			if i := g.indexOf(c); i >= 0 {
				created[i]++
			}
		}
		if t.Status == "done" && t.StatusChangedAt.Valid {
			if d := parseLocal(t.StatusChangedAt.String); !d.IsZero() {
				if i := g.indexOf(d); i >= 0 {
					done[i]++
				}
			}
		}
	}
	return Series{
		Key:   "throughput",
		Label: "Created vs completed",
		Unit:  "count",
		Lines: []Line{
			{Key: "created", Label: "Created", Points: gridPoints(g, created)},
			{Key: "done", Label: "Completed", Points: gridPoints(g, done)},
		},
	}
}

// cycleTimeMedianDays is the median days from created_at to status_changed_at
// for tasks completed within the window. ok is false when no task qualifies, so
// the KPI is omitted rather than shown as a misleading 0.
func cycleTimeMedianDays(tasks []*flowdb.Task, g bucketGrid) (float64, bool) {
	var durs []float64
	for _, t := range tasks {
		if t.Status != "done" || !t.StatusChangedAt.Valid {
			continue
		}
		d := parseLocal(t.StatusChangedAt.String)
		c := parseLocal(t.CreatedAt)
		if d.IsZero() || c.IsZero() {
			continue
		}
		if g.indexOf(d) < 0 {
			continue // not completed within the window
		}
		days := d.Sub(c).Hours() / 24
		if days < 0 {
			continue
		}
		durs = append(durs, days)
	}
	if len(durs) == 0 {
		return 0, false
	}
	return median(durs), true
}

// median returns the median of vals (mean of the two middle values for an even
// count). vals is sorted in place.
func median(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sort.Float64s(vals)
	if n%2 == 1 {
		return vals[n/2]
	}
	return (vals[n/2-1] + vals[n/2]) / 2
}

// ---- wire contract: payload envelope ----

// AnalyticsPayload is the response envelope for GET /api/analytics. Adding a
// metric means a new entry in KPIs/Series, never a new route.
type AnalyticsPayload struct {
	Range       string             `json:"range,omitempty"`
	Bucket      bucketUnit         `json:"bucket"`
	From        string             `json:"from"`
	To          string             `json:"to"`
	TZ          string             `json:"tz"`
	GeneratedAt string             `json:"generated_at"`
	Partial     bool               `json:"partial_bucket"`
	KPIs        []Kpi              `json:"kpis"`
	Series      []Series           `json:"series"`
	Value       *ValueStats        `json:"value,omitempty"`
	Breakdowns  []Breakdown        `json:"breakdowns,omitempty"`
	Funnel      *Funnel            `json:"funnel,omitempty"`
	Conversions []SourceConversion `json:"conversions,omitempty"`
}

// SourceConversion is one connector's funnel from observed events to created
// tasks over the window — the "how much Slack/GitHub turns into work" story.
type SourceConversion struct {
	Source   string `json:"source"` // slack | github
	Observed int    `json:"observed"`
	Surfaced int    `json:"surfaced"`
	Tasks    int    `json:"tasks"` // events the steerer routed to make_task
}

// analyticsInputs bundles the raw data the payload is assembled from. Grouping
// them keeps buildAnalyticsPayload's signature stable as domains are added and
// avoids a long run of same-typed slice arguments at the call sites.
type analyticsInputs struct {
	tasks          []*flowdb.Task
	usages         []taskTokenUsage
	runs           []*flowdb.BrainRun
	traces         []flowdb.SteeringTraceLite
	tags           map[string][]string // task slug -> tags (for task-origin breakdown)
	projectNames   map[string]string   // project slug -> display name
	valueConstants valueConstants
}

// Kpi is one headline number with a period-over-period delta. DeltaPct is nil
// (omitted by the UI) when the prior window was empty — see deltaPct.
type Kpi struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Value    float64  `json:"value"`
	Unit     string   `json:"unit"`
	DeltaPct *float64 `json:"delta_pct"`
}

// analyticsQuery is the resolved request: an explicit [From,To) window, the
// bucket granularity, and an optional provider filter.
type analyticsQuery struct {
	Range    string
	From     time.Time
	To       time.Time
	Unit     bucketUnit
	Provider string
}

// parseAnalyticsRange resolves the query string into an analyticsQuery. A
// `range` token wins; otherwise from/to define a custom window (bucket chosen by
// span); with neither, it defaults to 7d. `to` is clamped to now.
func parseAnalyticsRange(v url.Values, now time.Time) (analyticsQuery, error) {
	q := analyticsQuery{Provider: v.Get("provider")}
	if r := v.Get("range"); r != "" {
		from, to, unit, ok := rangeWindow(now, r)
		if !ok {
			return q, fmt.Errorf("unknown range %q", r)
		}
		q.Range, q.From, q.To, q.Unit = r, from, to, unit
		return q, nil
	}
	if fromS, toS := v.Get("from"), v.Get("to"); fromS != "" || toS != "" {
		from, to := parseLocal(fromS), parseLocal(toS)
		if from.IsZero() || to.IsZero() {
			return q, fmt.Errorf("from and to must both be RFC3339 timestamps")
		}
		if !from.Before(to) {
			return q, fmt.Errorf("from must be before to")
		}
		if to.After(now) {
			to = now
		}
		q.From, q.To, q.Unit = from, to, unitForSpan(to.Sub(from))
		return q, nil
	}
	from, to, unit, _ := rangeWindow(now, "7d")
	q.Range, q.From, q.To, q.Unit = "7d", from, to, unit
	return q, nil
}

// buildAnalyticsPayload assembles the payload from the task list, transcript
// usage, and brain-run ledger. It is pure (no DB, no clock) so it can be
// table-tested; the handler supplies the inputs + now.
func buildAnalyticsPayload(in analyticsInputs, q analyticsQuery, now time.Time) AnalyticsPayload {
	tasks, usages, runs, traces := in.tasks, in.usages, in.runs, in.traces
	g := bucketsFor(q.From, q.To, q.Unit, now)
	span := q.To.Sub(q.From)
	prevFrom, prevTo := q.From.Add(-span), q.From
	prevG := bucketsFor(prevFrom, prevTo, q.Unit, prevTo)

	var doneCur, donePrev float64
	for _, t := range tasks {
		if t.Status != "done" || !t.StatusChangedAt.Valid {
			continue
		}
		d := parseLocal(t.StatusChangedAt.String)
		if d.IsZero() {
			continue
		}
		switch {
		case g.indexOf(d) >= 0:
			doneCur++
		case !d.Before(prevFrom) && d.Before(prevTo):
			donePrev++
		}
	}

	tok := aggregateTokens(usages, g)
	prevTok := aggregateTokens(usages, prevG)
	value := computeValueStats(usages, runs, g, in.valueConstants)

	kpis := []Kpi{
		{Key: "tasks_done", Label: "Completed", Value: doneCur, Unit: "count", DeltaPct: deltaPct(doneCur, donePrev)},
	}
	if med, ok := cycleTimeMedianDays(tasks, g); ok {
		kpis = append(kpis, Kpi{Key: "cycle_time_median", Label: "Median cycle", Value: med, Unit: "days"})
	}
	kpis = append(kpis,
		Kpi{Key: "tokens", Label: "Tokens", Value: tok.tokenTotal, Unit: "tokens", DeltaPct: deltaPct(tok.tokenTotal, prevTok.tokenTotal)},
		Kpi{Key: "cost_usd", Label: "Cost", Value: tok.costTotal, Unit: "usd", DeltaPct: deltaPct(tok.costTotal, prevTok.costTotal)},
	)

	series := []Series{activitySeries(tasks, g)}
	// Token time-series only for day/week grids — hourly has no per-hour data
	// (see the 1d token decision); the token/cost KPIs above still cover it.
	if q.Unit != bucketHour && len(tok.tokensByProvider) > 0 {
		series = append(series,
			stackedSeries("tokens", "Tokens", "tokens", tok.tokensByProvider, g),
			stackedSeries("cost", "Token cost", "usd", tok.costByProvider, g),
		)
	}

	// Autonomy domain: only surfaced when there are runs in the fetched set, so
	// operators who never use `do --auto`/owners don't get an empty chart.
	if len(runs) > 0 {
		auto := computeAutonomy(runs, g)
		prevAuto := computeAutonomy(runs, prevG)
		kpis = append(kpis, Kpi{Key: "runs", Label: "Autonomous runs", Value: auto.started, Unit: "count", DeltaPct: deltaPct(auto.started, prevAuto.started)})
		if auto.hasFinished {
			kpis = append(kpis, Kpi{Key: "run_success", Label: "Run success", Value: auto.successPct, Unit: "pct"})
		}
		if auto.p50ok {
			kpis = append(kpis, Kpi{Key: "run_p50", Label: "Median run", Value: auto.p50Minutes, Unit: "min"})
		}
		series = append(series, autonomySeries(runs, g))
	}

	// Steering funnel domain: only when the attention router has trace rows in
	// the window — operators without connectors see no empty funnel.
	var funnel *Funnel
	if len(traces) > 0 {
		f := steeringFunnel(traces, g)
		funnel = &f
		var observedPrev float64
		for _, r := range traces {
			if prevG.indexOf(parseLocal(r.CreatedAt)) >= 0 {
				observedPrev++
			}
		}
		kpis = append(kpis, Kpi{Key: "events_observed", Label: "Events observed", Value: float64(f.Observed), Unit: "count", DeltaPct: deltaPct(float64(f.Observed), observedPrev)})
		if f.Observed > 0 {
			kpis = append(kpis, Kpi{Key: "surface_rate", Label: "Surface rate", Value: float64(f.Surfaced) / float64(f.Observed) * 100, Unit: "pct"})
		}
		vol, latency := steeringSeries(traces, g)
		series = append(series, vol, latency)
	}

	var breakdowns []Breakdown
	if len(tok.modelTokens) > 0 {
		breakdowns = append(breakdowns, modelBreakdown(tok))
	}
	// Project effort (tokens by project) — only when some project had token spend.
	if pe := projectEffortBreakdown(usages, in.projectNames, g); len(pe.Segments) > 0 {
		breakdowns = append(breakdowns, pe)
	}
	// Tasks by origin — only when at least one task was created in the window.
	if ts := taskSourceBreakdown(tasks, in.tags, g); segmentsSum(ts.Segments) > 0 {
		breakdowns = append(breakdowns, ts)
	}

	// Connector → task conversions (Slack/GitHub).
	conversions := sourceConversions(traces, g)

	return AnalyticsPayload{
		Range:       q.Range,
		Bucket:      q.Unit,
		From:        q.From.Format(time.RFC3339),
		To:          q.To.Format(time.RFC3339),
		TZ:          now.Location().String(),
		GeneratedAt: now.Format(time.RFC3339),
		Partial:     g.Partial,
		KPIs:        kpis,
		Series:      series,
		Value:       value,
		Breakdowns:  breakdowns,
		Funnel:      funnel,
		Conversions: conversions,
	}
}

// segmentsSum totals a breakdown's segment values (used to drop all-zero breakdowns).
func segmentsSum(segs []Segment) float64 {
	var s float64
	for _, seg := range segs {
		s += seg.Value
	}
	return s
}

// handleAnalytics serves GET /api/analytics. It runs off the SSE hot path — the
// aggregation here never executes inside buildUIData.
func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	q, err := parseAnalyticsRange(r.URL.Query(), now)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	// Fetch back through the previous comparison window (by updated_at, which a
	// completion bumps) so KPI deltas have prior-period data. Regular tasks only —
	// playbook runs are automated and excluded from manual throughput.
	since := q.From.Add(-q.To.Sub(q.From)).Format(time.RFC3339)
	tasks, err := flowdb.ListTasks(s.cfg.DB, flowdb.TaskFilter{
		Kind:            "regular",
		Since:           since,
		IncludeArchived: true,
	})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	// Autonomous-run ledger + steering trace over the same comparison span for
	// the autonomy and funnel domains. Read failures degrade gracefully — the
	// page drops those charts rather than erroring the whole request.
	runs, err := flowdb.ListBrainRunsSince(s.cfg.DB, since)
	if err != nil {
		runs = nil
	}
	traces, err := flowdb.ListSteeringTraceLite(s.cfg.DB, since)
	if err != nil {
		traces = nil
	}
	// Tags (task origin) + project display names — both degrade to empty maps,
	// which the pure functions handle (origin → manual; project label → slug).
	slugs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		slugs = append(slugs, t.Slug)
	}
	tags, err := flowdb.GetTaskTagsBatch(s.cfg.DB, slugs)
	if err != nil {
		tags = nil
	}
	projectNames := map[string]string{}
	if projects, perr := flowdb.ListProjects(s.cfg.DB, flowdb.ProjectFilter{IncludeArchived: true}); perr == nil {
		for _, p := range projects {
			projectNames[p.Slug] = p.Name
		}
	}
	writeJSON(w, buildAnalyticsPayload(analyticsInputs{
		tasks:          tasks,
		usages:         s.resolveTokenUsage(tasks),
		runs:           runs,
		traces:         traces,
		tags:           tags,
		projectNames:   projectNames,
		valueConstants: loadValueConstants(filepath.Join(s.cfg.FlowRoot, "stats.json")),
	}, q, now))
}

// resolveTokenUsage lifts per-task transcript usage out of the cache, deduped by
// resolved session path (mirrors buildTokenSeries). Tasks without a session are
// skipped; a transcript read error is tolerated (that task contributes nothing).
func (s *Server) resolveTokenUsage(tasks []*flowdb.Task) []taskTokenUsage {
	var usages []taskTokenUsage
	seen := map[string]bool{}
	for _, t := range tasks {
		if !t.SessionID.Valid || strings.TrimSpace(t.SessionID.String) == "" {
			continue
		}
		path, err := sessionJSONLPath(s.cfg.DB, t)
		if err != nil || path == "" || seen[path] {
			continue
		}
		seen[path] = true
		entry, err := s.transcripts.get(path)
		if err != nil {
			continue
		}
		provider := t.SessionProvider
		if provider == "" {
			provider = "claude"
		}
		roll := lookupRollupForTask(entry.usage.LookupEvents, t.Slug, s.cfg.FlowRoot)
		usages = append(usages, taskTokenUsage{
			Provider:           provider,
			Model:              strings.TrimSpace(entry.usage.Model),
			ProjectSlug:        t.ProjectSlug.String,
			TokensByDay:        entry.usage.TokensByDay,
			CostByDay:          entry.usage.CostByDay,
			LookupsByDay:       roll.LookupsByDay,
			ContextTokensByDay: roll.ContextTokensByDay,
		})
	}
	return usages
}

// ---- tokens & cost domain (transcript usage) ----

// Breakdown is a non-time composition (e.g. model mix) over the window.
type Breakdown struct {
	Key      string    `json:"key"`
	Label    string    `json:"label"`
	Segments []Segment `json:"segments"`
}

// Segment is one slice of a Breakdown.
type Segment struct {
	Key   string  `json:"key"`
	Label string  `json:"label,omitempty"`
	Value float64 `json:"value"`
}

// taskTokenUsage is the per-task transcript usage the token domain needs, lifted
// out of the transcript cache so the bucketing below stays pure and testable.
type taskTokenUsage struct {
	Provider           string             // claude | codex (task.SessionProvider)
	Model              string             // session model (transcriptUsageStats.Model)
	ProjectSlug        string             // owning project ("" = floating) — for project effort
	TokensByDay        map[string]int     // YYYY-MM-DD -> fresh tokens
	CostByDay          map[string]float64 // YYYY-MM-DD -> billed USD
	LookupsByDay       map[string]map[string]int
	ContextTokensByDay map[string]int64
}

// tokenAgg is the accumulated token/cost view for a window: per-provider
// per-bucket arrays (for stacked series), model totals (for the mix breakdown),
// and window totals (for KPIs). Per-bucket arrays are grid-length.
type tokenAgg struct {
	tokensByProvider map[string][]float64
	costByProvider   map[string][]float64
	modelTokens      map[string]float64
	tokenTotal       float64
	costTotal        float64
}

// parseDayLocal parses a YYYY-MM-DD day key into local midnight, or the zero
// time if unparseable.
func parseDayLocal(day string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ensureLine returns m[key], allocating a zeroed grid-length slice on first use.
func ensureLine(m map[string][]float64, key string, n int) []float64 {
	if m[key] == nil {
		m[key] = make([]float64, n)
	}
	return m[key]
}

// aggregateTokens buckets each task's per-day tokens/cost into the grid, split
// by provider, and tallies model + window totals. A day whose midnight falls
// outside the grid is skipped, so the totals reflect only the window. Works for
// any grid; the caller emits the time-series only for day/week grids (hourly has
// no per-hour data — see the 1d token decision).
func aggregateTokens(usages []taskTokenUsage, g bucketGrid) tokenAgg {
	agg := tokenAgg{
		tokensByProvider: map[string][]float64{},
		costByProvider:   map[string][]float64{},
		modelTokens:      map[string]float64{},
	}
	for _, u := range usages {
		prov := u.Provider
		if prov == "" {
			prov = "unknown"
		}
		model := u.Model
		if model == "" {
			model = "unknown"
		}
		for day, tok := range u.TokensByDay {
			i := g.indexOf(parseDayLocal(day))
			if i < 0 {
				continue
			}
			ensureLine(agg.tokensByProvider, prov, g.Len())[i] += float64(tok)
			agg.modelTokens[model] += float64(tok)
			agg.tokenTotal += float64(tok)
		}
		for day, cost := range u.CostByDay {
			i := g.indexOf(parseDayLocal(day))
			if i < 0 {
				continue
			}
			ensureLine(agg.costByProvider, prov, g.Len())[i] += cost
			agg.costTotal += cost
		}
	}
	return agg
}

// stackedSeries builds a stacked Series from per-provider per-bucket arrays, with
// provider lines in stable (sorted) order.
func stackedSeries(key, label, unit string, byProvider map[string][]float64, g bucketGrid) Series {
	provs := make([]string, 0, len(byProvider))
	for p := range byProvider {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	lines := make([]Line, 0, len(provs))
	for _, p := range provs {
		lines = append(lines, Line{Key: p, Label: p, Points: gridPoints(g, byProvider[p])})
	}
	return Series{Key: key, Label: label, Unit: unit, Stacked: true, Lines: lines}
}

// modelBreakdown turns model token totals into a composition, largest first.
func modelBreakdown(agg tokenAgg) Breakdown {
	segs := make([]Segment, 0, len(agg.modelTokens))
	for m, v := range agg.modelTokens {
		segs = append(segs, Segment{Key: m, Label: m, Value: v})
	}
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].Value != segs[j].Value {
			return segs[i].Value > segs[j].Value
		}
		return segs[i].Key < segs[j].Key
	})
	return Breakdown{Key: "model_mix", Label: "Model mix", Segments: segs}
}

// ---- autonomy domain (brain_runs) ----

// brainRunStarted is the run's start time (started_at, falling back to
// created_at), or the zero time if neither parses.
func brainRunStarted(r *flowdb.BrainRun) time.Time {
	if r.StartedAt.Valid {
		if t := parseLocal(r.StartedAt.String); !t.IsZero() {
			return t
		}
	}
	return parseLocal(r.CreatedAt)
}

// brainRunFinished returns (finishedAt, true) for a finished run, or
// (zero, false) for one still in flight. finished_at is set in exactly one
// place at terminal time (auto.go), so its presence is the authoritative
// "is this run done?" signal.
func brainRunFinished(r *flowdb.BrainRun) (time.Time, bool) {
	if !r.FinishedAt.Valid {
		return time.Time{}, false
	}
	t := parseLocal(r.FinishedAt.String)
	if t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

// "completed" is the single success status — the worker called `flow done` on
// itself. Every other terminal status ("dead", "error") is a failed run.
func brainRunSucceeded(status string) bool { return status == "completed" }

// autonomySeries buckets autonomous-run volume: runs started per bucket (by
// start time) and runs that finished successfully per bucket (by finish time).
// In-flight runs count toward "started" but never "completed".
func autonomySeries(runs []*flowdb.BrainRun, g bucketGrid) Series {
	started := make([]float64, g.Len())
	completed := make([]float64, g.Len())
	for _, r := range runs {
		if s := brainRunStarted(r); !s.IsZero() {
			if i := g.indexOf(s); i >= 0 {
				started[i]++
			}
		}
		if f, ok := brainRunFinished(r); ok && brainRunSucceeded(r.Status) {
			if i := g.indexOf(f); i >= 0 {
				completed[i]++
			}
		}
	}
	return Series{
		Key:   "runs",
		Label: "Autonomous runs",
		Unit:  "count",
		Lines: []Line{
			{Key: "started", Label: "Started", Points: gridPoints(g, started)},
			{Key: "completed", Label: "Completed", Points: gridPoints(g, completed)},
		},
	}
}

// autonomyStats are the window-scoped autonomy KPIs. successPct/p50Minutes are
// computed over runs that FINISHED within the grid; in-flight runs are excluded
// (their outcome and duration aren't known yet). hasFinished/p50ok flag whether
// the corresponding KPI has any data, so the caller omits it rather than
// rendering a misleading 0.
type autonomyStats struct {
	started     float64
	finished    float64
	successPct  float64
	p50Minutes  float64
	hasFinished bool
	p50ok       bool
}

// computeAutonomy tallies run volume (started within the grid) and outcome
// stats (success rate + p50 duration over finished runs).
func computeAutonomy(runs []*flowdb.BrainRun, g bucketGrid) autonomyStats {
	var st autonomyStats
	var succeeded float64
	var durs []float64
	for _, r := range runs {
		if s := brainRunStarted(r); !s.IsZero() && g.indexOf(s) >= 0 {
			st.started++
		}
		f, ok := brainRunFinished(r)
		if !ok || g.indexOf(f) < 0 {
			continue // in-flight, or finished outside the window
		}
		st.finished++
		if brainRunSucceeded(r.Status) {
			succeeded++
		}
		if s := brainRunStarted(r); !s.IsZero() {
			if d := f.Sub(s).Minutes(); d >= 0 {
				durs = append(durs, d)
			}
		}
	}
	if st.finished > 0 {
		st.successPct = succeeded / st.finished * 100
		st.hasFinished = true
	}
	if len(durs) > 0 {
		st.p50Minutes = median(durs)
		st.p50ok = true
	}
	return st
}

// ---- steering funnel domain (attention router) ----

// Funnel is the window-total attention-router funnel: how many observed events
// surfaced vs errored vs dropped (broken down by the stage they died at). The
// UI renders this as a funnel bar; Dropped segments are largest-first.
type Funnel struct {
	Observed int       `json:"observed"`
	Surfaced int       `json:"surfaced"`
	Errors   int       `json:"errors"`
	Dropped  []Segment `json:"dropped,omitempty"`
}

// steeringFunnel tallies the window-total funnel over lite trace rows whose
// created_at falls within the grid.
func steeringFunnel(rows []flowdb.SteeringTraceLite, g bucketGrid) Funnel {
	var f Funnel
	dropped := map[string]float64{}
	for _, r := range rows {
		if g.indexOf(parseLocal(r.CreatedAt)) < 0 {
			continue
		}
		f.Observed++
		switch r.Disposition {
		case "surfaced":
			f.Surfaced++
		case "error":
			f.Errors++
		case "dropped":
			stage := r.StageReached
			if stage == "" {
				stage = "unknown"
			}
			dropped[stage]++
		}
	}
	for stage, n := range dropped {
		f.Dropped = append(f.Dropped, Segment{Key: stage, Label: stage, Value: n})
	}
	sort.Slice(f.Dropped, func(i, j int) bool {
		if f.Dropped[i].Value != f.Dropped[j].Value {
			return f.Dropped[i].Value > f.Dropped[j].Value
		}
		return f.Dropped[i].Key < f.Dropped[j].Key
	})
	return f
}

// steeringSeries builds the per-bucket funnel volume (observed vs surfaced) and
// the p50 triage-latency trend. Latency is a true per-bucket median over the
// raw rows in that bucket; empty buckets are 0.
func steeringSeries(rows []flowdb.SteeringTraceLite, g bucketGrid) (volume Series, latency Series) {
	observed := make([]float64, g.Len())
	surfaced := make([]float64, g.Len())
	lat := make([][]float64, g.Len())
	for _, r := range rows {
		i := g.indexOf(parseLocal(r.CreatedAt))
		if i < 0 {
			continue
		}
		observed[i]++
		if r.Disposition == "surfaced" {
			surfaced[i]++
		}
		if r.LatencyMS > 0 {
			lat[i] = append(lat[i], float64(r.LatencyMS))
		}
	}
	p50 := make([]float64, g.Len())
	for i := range lat {
		if len(lat[i]) > 0 {
			p50[i] = median(lat[i])
		}
	}
	volume = Series{
		Key:   "steering",
		Label: "Attention funnel",
		Unit:  "count",
		Lines: []Line{
			{Key: "observed", Label: "Observed", Points: gridPoints(g, observed)},
			{Key: "surfaced", Label: "Surfaced", Points: gridPoints(g, surfaced)},
		},
	}
	latency = Series{
		Key:   "steering_latency",
		Label: "Triage latency (p50)",
		Unit:  "ms",
		Lines: []Line{
			{Key: "p50", Label: "p50 ms", Points: gridPoints(g, p50)},
		},
	}
	return volume, latency
}

// ---- activity heatmap (weekday × hour) ----------------------------------

// ---- project effort (tokens by project) ---------------------------------

// projectEffortBreakdown sums in-window tokens per owning project — the honest
// "where is effort going" proxy (no time-tracking exists). Tasks with no project
// fall into "(none)". Largest first; names maps slug → display name.
func projectEffortBreakdown(usages []taskTokenUsage, names map[string]string, g bucketGrid) Breakdown {
	byProject := map[string]float64{}
	for _, u := range usages {
		var sum float64
		for day, tok := range u.TokensByDay {
			if g.indexOf(parseDayLocal(day)) >= 0 {
				sum += float64(tok)
			}
		}
		if sum == 0 {
			continue
		}
		byProject[u.ProjectSlug] += sum
	}
	segs := make([]Segment, 0, len(byProject))
	for slug, v := range byProject {
		label := names[slug]
		if label == "" {
			label = slug
		}
		if label == "" {
			label = "(no project)"
		}
		segs = append(segs, Segment{Key: orNoProject(slug), Label: label, Value: v})
	}
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].Value != segs[j].Value {
			return segs[i].Value > segs[j].Value
		}
		return segs[i].Key < segs[j].Key
	})
	return Breakdown{Key: "project_effort", Label: "Project effort (tokens)", Segments: segs}
}

func orNoProject(slug string) string {
	if slug == "" {
		return "(none)"
	}
	return slug
}

// ---- task origin (tasks by connector source) ----------------------------

// taskSource classifies a task by its connector-provenance tags, mirroring the
// Overview's doneBySource: Slack wins when a task carries both (it's the trigger).
func taskSource(tags []string) string {
	slack, github := false, false
	for _, g := range tags {
		switch {
		case g == "slack-reply" || strings.HasPrefix(g, "slack-thread:"):
			slack = true
		case g == "github" || strings.HasPrefix(g, "gh-pr:") || strings.HasPrefix(g, "gh-issue:"):
			github = true
		}
	}
	switch {
	case slack:
		return "slack"
	case github:
		return "github"
	default:
		return "manual"
	}
}

// taskSourceBreakdown counts tasks CREATED within the window by their origin.
// The slack/github/manual segments are always present (stable legend) even at 0.
func taskSourceBreakdown(tasks []*flowdb.Task, tags map[string][]string, g bucketGrid) Breakdown {
	counts := map[string]float64{"slack": 0, "github": 0, "manual": 0}
	for _, t := range tasks {
		c := parseLocal(t.CreatedAt)
		if c.IsZero() || g.indexOf(c) < 0 {
			continue
		}
		counts[taskSource(tags[t.Slug])]++
	}
	return Breakdown{
		Key:   "task_source",
		Label: "Tasks by origin",
		Segments: []Segment{
			{Key: "slack", Label: "Slack", Value: counts["slack"]},
			{Key: "github", Label: "GitHub", Value: counts["github"]},
			{Key: "manual", Label: "Manual", Value: counts["manual"]},
		},
	}
}

// ---- connector → task conversion -----------------------------------------

// sourceConversions builds a per-connector funnel (observed → surfaced → made a
// task) over the window from steering traces. "Tasks" counts events the steerer
// routed to make_task. Most-active source first; only slack/github are tracked.
func sourceConversions(traces []flowdb.SteeringTraceLite, g bucketGrid) []SourceConversion {
	type agg struct{ observed, surfaced, tasks int }
	by := map[string]*agg{}
	for _, tr := range traces {
		if tr.Source != "slack" && tr.Source != "github" {
			continue
		}
		if g.indexOf(parseLocal(tr.CreatedAt)) < 0 {
			continue
		}
		a := by[tr.Source]
		if a == nil {
			a = &agg{}
			by[tr.Source] = a
		}
		a.observed++
		if tr.Disposition == "surfaced" {
			a.surfaced++
		}
		if tr.FinalAction == "make_task" {
			a.tasks++
		}
	}
	out := make([]SourceConversion, 0, len(by))
	for src, a := range by {
		out = append(out, SourceConversion{Source: src, Observed: a.observed, Surfaced: a.surfaced, Tasks: a.tasks})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Observed != out[j].Observed {
			return out[i].Observed > out[j].Observed
		}
		return out[i].Source < out[j].Source
	})
	return out
}
