package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"flow/internal/flowdb"
)

const (
	lookupResume    = "resume"
	lookupReference = "reference"
	lookupCrossTask = "cross_task"
	lookupKB        = "kb"
)

var lookupKindLabels = map[string]string{
	lookupResume:    "Resume",
	lookupReference: "Reference",
	lookupCrossTask: "Cross-task",
	lookupKB:        "Knowledge base",
}

var lookupKindOrder = []string{lookupResume, lookupReference, lookupCrossTask, lookupKB}

type valueConstants struct {
	MinutesPerUnattendedRun float64 `json:"minutes_per_unattended_run"`
	MinutesPerContextSwitch float64 `json:"minutes_per_context_switch"`
	DollarPerHour           float64 `json:"dollar_per_hour"`
}

func defaultValueConstants() valueConstants {
	return valueConstants{
		MinutesPerUnattendedRun: 20,
		MinutesPerContextSwitch: 5,
		DollarPerHour:           100,
	}
}

func normalizeValueConstants(c valueConstants) valueConstants {
	def := defaultValueConstants()
	if c.MinutesPerUnattendedRun <= 0 {
		c.MinutesPerUnattendedRun = def.MinutesPerUnattendedRun
	}
	if c.MinutesPerContextSwitch <= 0 {
		c.MinutesPerContextSwitch = def.MinutesPerContextSwitch
	}
	if c.DollarPerHour <= 0 {
		c.DollarPerHour = def.DollarPerHour
	}
	return c
}

func loadValueConstants(path string) valueConstants {
	def := defaultValueConstants()
	data, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	var c valueConstants
	if err := json.Unmarshal(data, &c); err != nil {
		fmt.Fprintf(os.Stderr, "warning: stats.json is malformed (%v); using defaults\n", err)
		return def
	}
	return normalizeValueConstants(c)
}

type ValueStats struct {
	Constants          valueConstants `json:"constants"`
	LookupsTotal       float64        `json:"lookups_total"`
	ContextTokens      float64        `json:"context_tokens"`
	AutomationRuns     float64        `json:"automation_runs"`
	AutomationHours    float64        `json:"automation_hours"`
	ContextSwitchHours float64        `json:"context_switch_hours"`
	AddressableCount   float64        `json:"addressable_count"`
	TotalHours         float64        `json:"total_hours"`
	TotalDollars       float64        `json:"total_dollars"`
	KPIs               []Kpi          `json:"kpis,omitempty"`
	Series             []Series       `json:"series,omitempty"`
	Breakdowns         []Breakdown    `json:"breakdowns,omitempty"`
}

type transcriptLookupEvent struct {
	Timestamp string
	Tool      string
	Command   string
	FilePath  string
}

type lookupRollup struct {
	Events []lookupValueEvent
}

type lookupValueEvent struct {
	Timestamp    string
	Kind         string
	ContextBytes int64
}

func accumulateTranscriptLookupEvents(stats *transcriptUsageStats, rec transcriptUsageRecord) {
	if len(rec.Message.Content) > 0 {
		var blocks []contentBlock
		if err := json.Unmarshal(rec.Message.Content, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type != "tool_use" || len(b.Input) == 0 {
					continue
				}
				if ev, ok := lookupEventFromToolUse(rec.Timestamp, b.Name, b.Input); ok {
					stats.LookupEvents = append(stats.LookupEvents, ev)
				}
			}
		}
	}
	if len(rec.Payload) == 0 {
		return
	}
	var payload struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Action    struct {
			Command []string `json:"command"`
		} `json:"action"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return
	}
	switch payload.Type {
	case "function_call":
		if ev, ok := lookupEventFromToolUse(rec.Timestamp, payload.Name, payload.Arguments); ok {
			stats.LookupEvents = append(stats.LookupEvents, ev)
		}
	case "local_shell_call":
		cmd := strings.Join(payload.Action.Command, " ")
		if ev, ok := lookupEventFromFields(rec.Timestamp, "local_shell", cmd, ""); ok {
			stats.LookupEvents = append(stats.LookupEvents, ev)
		}
	}
}

func lookupEventFromToolUse(ts, tool string, raw json.RawMessage) (transcriptLookupEvent, bool) {
	var in struct {
		Command  string `json:"command"`
		Cmd      string `json:"cmd"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		var argString string
		if err := json.Unmarshal(raw, &argString); err == nil {
			_ = json.Unmarshal([]byte(argString), &in)
		}
	}
	cmd := in.Command
	if cmd == "" {
		cmd = in.Cmd
	}
	fp := in.FilePath
	if fp == "" {
		fp = in.Path
	}
	return lookupEventFromFields(ts, tool, cmd, fp)
}

func lookupEventFromFields(ts, tool, cmd, filePath string) (transcriptLookupEvent, bool) {
	cmd = strings.TrimSpace(cmd)
	filePath = strings.TrimSpace(filePath)
	if cmd == "" && filePath == "" {
		return transcriptLookupEvent{}, false
	}
	if !possibleLookup(tool, cmd, filePath) {
		return transcriptLookupEvent{}, false
	}
	return transcriptLookupEvent{Timestamp: ts, Tool: tool, Command: cmd, FilePath: filePath}, true
}

func possibleLookup(tool, cmd, filePath string) bool {
	switch {
	case strings.Contains(cmd, "flow show "),
		strings.Contains(cmd, "flow transcript"),
		strings.Contains(filepath.ToSlash(cmd), "/.flow/kb/"),
		strings.Contains(filepath.ToSlash(cmd), "/.flow/tasks/"),
		strings.Contains(filepath.ToSlash(cmd), "/.flow/projects/"):
		return true
	case strings.Contains(filepath.ToSlash(filePath), "/.flow/kb/"),
		strings.Contains(filepath.ToSlash(filePath), "/.flow/tasks/"),
		strings.Contains(filepath.ToSlash(filePath), "/.flow/projects/"):
		return true
	}
	lowerTool := strings.ToLower(tool)
	if strings.Contains(lowerTool, "read") {
		return filePath != "" || mentionsFlowContextPath(cmd)
	}
	if strings.Contains(lowerTool, "bash") || strings.Contains(lowerTool, "shell") {
		return mentionsFlowContextPath(cmd)
	}
	return false
}

func mentionsFlowContextPath(s string) bool {
	slash := filepath.ToSlash(s)
	return strings.Contains(slash, "/kb/") ||
		strings.Contains(slash, "/tasks/") ||
		strings.Contains(slash, "/projects/")
}

func lookupRollupForTask(events []transcriptLookupEvent, ownSlug, flowRoot string) lookupRollup {
	var out lookupRollup
	for _, ev := range events {
		if parseLocal(ev.Timestamp).IsZero() {
			continue
		}
		kind, contextBytes, ok := classifyLookupEvent(ev, ownSlug, flowRoot)
		if !ok {
			continue
		}
		out.Events = append(out.Events, lookupValueEvent{Timestamp: ev.Timestamp, Kind: kind, ContextBytes: contextBytes})
	}
	return out
}

func classifyLookupEvent(ev transcriptLookupEvent, ownSlug, flowRoot string) (string, int64, bool) {
	cmd := strings.TrimSpace(ev.Command)
	root := strings.TrimSpace(flowRoot)
	ownBrief := filepath.Join(root, "tasks", ownSlug, "brief.md")
	ownUpdates := filepath.Join(root, "tasks", ownSlug, "updates")
	if strings.Contains(cmd, "flow show task") {
		return lookupResume, fileSize(ownBrief) + dirSize(ownUpdates, ".md"), true
	}
	if strings.Contains(cmd, "flow transcript") {
		return lookupCrossTask, fileSize(ownBrief), true
	}
	if strings.Contains(cmd, "flow show ") {
		return lookupReference, fileSize(ownBrief), true
	}

	p := strings.TrimSpace(ev.FilePath)
	if p == "" {
		p = pathMentionedInCommand(cmd, root)
	}
	if p == "" {
		return "", 0, false
	}
	if pathUnder(p, filepath.Join(root, "kb")) || strings.Contains(filepath.ToSlash(p), "/.flow/kb/") {
		if ev.FilePath != "" {
			return lookupKB, fileSize(p), true
		}
		return lookupKB, computeAvgKBFileBytes(filepath.Join(root, "kb")), true
	}
	if ownSlug != "" && pathUnder(p, filepath.Join(root, "tasks", ownSlug)) {
		return "", 0, false
	}
	if pathUnder(p, filepath.Join(root, "tasks")) || pathUnder(p, filepath.Join(root, "projects")) ||
		strings.Contains(filepath.ToSlash(p), "/.flow/tasks/") || strings.Contains(filepath.ToSlash(p), "/.flow/projects/") {
		if ev.FilePath != "" {
			return lookupCrossTask, fileSize(p), true
		}
		return lookupCrossTask, fileSize(ownBrief), true
	}
	return "", 0, false
}

func pathMentionedInCommand(cmd, root string) string {
	if root == "" || cmd == "" {
		return ""
	}
	slashRoot := filepath.ToSlash(root)
	for _, marker := range []string{"/kb/", "/tasks/", "/projects/"} {
		if idx := strings.Index(filepath.ToSlash(cmd), slashRoot+marker); idx >= 0 {
			rest := filepath.ToSlash(cmd)[idx:]
			end := len(rest)
			for i, r := range rest {
				if r == '"' || r == '\'' || r == ' ' || r == '\t' || r == '\n' {
					end = i
					break
				}
			}
			return filepath.FromSlash(rest[:end])
		}
	}
	return ""
}

func pathUnder(path, dir string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(dir) == "" {
		return false
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	d, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(d, p)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")
}

func computeValueStats(usages []taskTokenUsage, runs []*flowdb.BrainRun, g bucketGrid, c valueConstants) *ValueStats {
	c = normalizeValueConstants(c)
	byKind := map[string]float64{}
	linesByKind := map[string][]float64{}
	var contextTokens float64
	var lookupsTotal float64
	var contextBytes int64
	for _, u := range usages {
		for _, ev := range u.Lookups {
			if ev.Kind == "" {
				continue
			}
			i := g.indexOf(parseLocal(ev.Timestamp))
			if i < 0 {
				continue
			}
			byKind[ev.Kind]++
			lookupsTotal++
			ensureLine(linesByKind, ev.Kind, g.Len())[i]++
			contextBytes += ev.ContextBytes
		}
	}
	contextTokens = float64(contextBytes / 4)
	var automationRuns float64
	for _, r := range runs {
		if s := brainRunStarted(r); !s.IsZero() && g.indexOf(s) >= 0 {
			automationRuns++
		}
	}
	if lookupsTotal == 0 && contextTokens == 0 && automationRuns == 0 {
		return nil
	}
	contextSwitches := byKind[lookupResume] + byKind[lookupReference]
	automationHours := automationRuns * c.MinutesPerUnattendedRun / 60
	contextSwitchHours := contextSwitches * c.MinutesPerContextSwitch / 60
	totalHours := automationHours + contextSwitchHours
	st := &ValueStats{
		Constants:          c,
		LookupsTotal:       lookupsTotal,
		ContextTokens:      contextTokens,
		AutomationRuns:     automationRuns,
		AutomationHours:    automationHours,
		ContextSwitchHours: contextSwitchHours,
		AddressableCount:   byKind[lookupReference] + byKind[lookupCrossTask],
		TotalHours:         totalHours,
		TotalDollars:       totalHours * c.DollarPerHour,
	}
	st.KPIs = []Kpi{
		{Key: "value_lookups", Label: "Context recalls", Value: st.LookupsTotal, Unit: "count"},
		{Key: "value_context_tokens", Label: "Context re-established", Value: st.ContextTokens, Unit: "tokens"},
	}
	if st.TotalHours > 0 {
		st.KPIs = append(st.KPIs,
			Kpi{Key: "value_hours_saved", Label: "Hours saved", Value: st.TotalHours, Unit: "hours"},
			Kpi{Key: "value_dollars_saved", Label: "Saved", Value: st.TotalDollars, Unit: "usd"},
		)
	}
	if len(linesByKind) > 0 {
		st.Series = []Series{lookupSeries(linesByKind, g)}
	}
	if len(byKind) > 0 {
		st.Breakdowns = []Breakdown{lookupBreakdown(byKind)}
	}
	return st
}

func lookupSeries(linesByKind map[string][]float64, g bucketGrid) Series {
	lines := make([]Line, 0, len(linesByKind))
	for _, kind := range lookupKindOrder {
		if vals := linesByKind[kind]; segmentsSumPoints(vals) > 0 {
			lines = append(lines, Line{Key: kind, Label: lookupKindLabels[kind], Points: gridPoints(g, vals)})
		}
	}
	for kind, vals := range linesByKind {
		if lookupKindLabels[kind] != "" || segmentsSumPoints(vals) == 0 {
			continue
		}
		lines = append(lines, Line{Key: kind, Label: kind, Points: gridPoints(g, vals)})
	}
	return Series{Key: "value_lookups", Label: "Context recalls", Unit: "count", Lines: lines}
}

func lookupBreakdown(byKind map[string]float64) Breakdown {
	segs := make([]Segment, 0, len(byKind))
	for kind, v := range byKind {
		if v <= 0 {
			continue
		}
		label := lookupKindLabels[kind]
		if label == "" {
			label = kind
		}
		segs = append(segs, Segment{Key: kind, Label: label, Value: v})
	}
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].Value != segs[j].Value {
			return segs[i].Value > segs[j].Value
		}
		return segs[i].Key < segs[j].Key
	})
	return Breakdown{Key: "lookup_kind", Label: "Recalls by kind", Segments: segs}
}

func segmentsSumPoints(vals []float64) float64 {
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum
}

func computeAvgKBFileBytes(kbDir string) int64 {
	entries, err := os.ReadDir(kbDir)
	if err != nil {
		return 0
	}
	var total int64
	var count int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		total += fi.Size()
		count++
	}
	if count == 0 {
		return 0
	}
	return total / count
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func dirSize(dir, ext string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		total += fi.Size()
	}
	return total
}
