package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"flow/internal/steering"
)

// steeringRunCap bounds how many recent cascade runs the in-flight store keeps.
// Live triage is bursty but a run is small (a handful of stage events), so a few
// dozen covers "what's running now + what just finished" without unbounded growth.
const steeringRunCap = 60

// steeringRun is one observed event's journey through the cascade — the live,
// CI-style counterpart to the persisted SteeringTrace. RunID equals the trace ID,
// so a finished run can be cross-referenced with its trace.
type steeringRun struct {
	RunID     string                `json:"run_id"`
	ThreadKey string                `json:"thread_key,omitempty"`
	Source    string                `json:"source,omitempty"`
	Stages    []steering.StageEvent `json:"stages"`
	Status    string                `json:"status"` // latest stage status; terminal once Done
	Done      bool                  `json:"done"`
	StartedAt string                `json:"started_at"`
	UpdatedAt string                `json:"updated_at"`

	// Raw origin (folded from the stage events; constant across a run). ChannelType
	// drives the UI's channel/DM/repo glyph; Channel is the resolver input + a
	// fallback when no name resolves. TS/TeamID/URL are resolver inputs only.
	Channel     string `json:"channel,omitempty"`
	ChannelType string `json:"channel_type,omitempty"`
	Author      string `json:"author,omitempty"`
	TS          string `json:"-"`
	TeamID      string `json:"-"`
	URL         string `json:"-"`

	// Resolved at snapshot/serve time (never stored) so the live view shows the
	// same human-readable origin the feed/trace tabs do: "#general" / "DM · Alice"
	// / "owner/repo", a display name, and a deep link — never a raw ID.
	ChannelName string `json:"channel_name,omitempty"`
	AuthorName  string `json:"author_name,omitempty"`
	Permalink   string `json:"permalink,omitempty"`
}

// steeringRunStore is a small ring of recent cascade runs keyed by RunID. It is
// the only place the live stage stream is materialized; the SSE/WS hub carries
// the deltas, this serves a snapshot to tabs that connect mid-flight.
type steeringRunStore struct {
	mu    sync.Mutex
	runs  map[string]*steeringRun
	order []string // RunID insertion order, for ring eviction
	cap   int
}

func newSteeringRunStore() *steeringRunStore {
	return &steeringRunStore{runs: map[string]*steeringRun{}, cap: steeringRunCap}
}

// record folds one stage event into its run and returns the updated run snapshot
// (a copy, safe to marshal without holding the lock). The first event for a
// RunID creates the run; the "verdict" stage marks it done.
func (s *steeringRunStore) record(e steering.StageEvent) steeringRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[e.RunID]
	if run == nil {
		run = &steeringRun{RunID: e.RunID, StartedAt: e.At}
		s.runs[e.RunID] = run
		s.order = append(s.order, e.RunID)
		s.evictLocked()
	}
	if e.ThreadKey != "" {
		run.ThreadKey = e.ThreadKey
	}
	if e.Source != "" {
		run.Source = e.Source
	}
	// Fold origin onto the run (it's constant across a run's stages and present
	// from the first "received" event, so even a Stage 0 drop carries it). Strip
	// it from the per-stage copy below so it isn't repeated on every stage row.
	if e.Channel != "" {
		run.Channel = e.Channel
	}
	if e.ChannelType != "" {
		run.ChannelType = e.ChannelType
	}
	if e.Author != "" {
		run.Author = e.Author
	}
	if e.TS != "" {
		run.TS = e.TS
	}
	if e.TeamID != "" {
		run.TeamID = e.TeamID
	}
	if e.URL != "" {
		run.URL = e.URL
	}
	e.Channel, e.ChannelType, e.Author, e.TS, e.TeamID, e.URL = "", "", "", "", "", ""
	// Streaming delta: a stage re-emitted with accumulated text updates its row in
	// place rather than appending a row per chunk.
	if e.Stream != "" {
		for i := len(run.Stages) - 1; i >= 0; i-- {
			if run.Stages[i].Stage == e.Stage {
				run.Stages[i].Stream = e.Stream
				run.Stages[i].At = e.At
				run.UpdatedAt = e.At
				return run.clone()
			}
		}
	}
	run.Stages = append(run.Stages, e)
	run.Status = e.Status
	run.UpdatedAt = e.At
	if e.Stage == "verdict" {
		run.Done = true
	}
	return run.clone()
}

// evictLocked drops the oldest runs once the ring is over capacity. Caller holds
// the lock.
func (s *steeringRunStore) evictLocked() {
	for len(s.order) > s.cap {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.runs, oldest)
	}
}

// snapshot returns the recent runs, newest-first, so a tab that connects
// mid-flight sees what's running and what just finished.
func (s *steeringRunStore) snapshot() []steeringRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]steeringRun, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		if run := s.runs[s.order[i]]; run != nil {
			out = append(out, run.clone())
		}
	}
	return out
}

func (r *steeringRun) clone() steeringRun {
	cp := *r
	cp.Stages = append([]steering.StageEvent(nil), r.Stages...)
	return cp
}

// publishSteeringStage is the cascade's Progress hook: it folds the event into
// the in-flight store and fans the delta out to subscribers so open tabs render
// the stage progressing live. Safe before the hub exists (no-op on the publish).
func (s *Server) publishSteeringStage(e steering.StageEvent) {
	if s.steeringRuns == nil {
		return
	}
	run := s.steeringRuns.record(e)
	if s.events == nil {
		return
	}
	data, err := json.Marshal(run)
	if err != nil {
		return
	}
	s.events.publish(eventEnvelope{
		Type:     "steering_stage",
		TaskSlug: run.ThreadKey, // lets clients filter by thread without parsing Data
		Data:     json.RawMessage(data),
	})
}

// handleSteeringRuns serves the recent + in-flight cascade runs so the inbox UI
// can render the live stage view on load, before the next WS delta arrives.
func (s *Server) handleSteeringRuns(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	runs := []steeringRun{}
	if s.steeringRuns != nil {
		runs = s.steeringRuns.snapshot()
	}
	s.enrichSteeringRuns(r.Context(), runs)
	writeJSON(w, map[string]any{"runs": runs})
}

// enrichSteeringRuns resolves each run's raw origin into the human-readable
// channel name / author / permalink the feed and trace tabs already show, so the
// live view never renders a raw Slack ID (or "untracked event" when the repo is
// known). It warms the Slack name cache once for the whole snapshot so per-run
// resolution is all cache hits (mirrors the trace handler). Mutates runs in place.
func (s *Server) enrichSteeringRuns(ctx context.Context, runs []steeringRun) {
	if len(runs) == 0 {
		return
	}
	var users, chans []string
	for _, run := range runs {
		if run.Source == "github" {
			continue // already-human fields; no resolver needed
		}
		if ch := steeringRunChannel(run); ch != "" {
			chans = append(chans, ch)
		}
		if run.Author != "" {
			users = append(users, run.Author)
		}
	}
	s.warmSlackNames(ctx, users, chans)
	for i := range runs {
		s.resolveSteeringRunOrigin(ctx, &runs[i])
	}
}

// steeringRunChannel returns the Slack channel id for a run, deriving it from the
// thread_key ("<channel>:<ts>") when the raw Channel field is empty.
func steeringRunChannel(run steeringRun) string {
	if run.Channel != "" {
		return run.Channel
	}
	ch, _ := splitThreadKey(run.ThreadKey)
	return ch
}

// resolveSteeringRunOrigin fills a run's ChannelName/AuthorName/Permalink from
// its raw origin, mirroring steeringTraceView so the live, trace, and feed tabs
// stay visually consistent. GitHub fields are already human; Slack IDs go through
// the name resolver with a DM fallback, and the cheap slack:// deep link (not a
// per-row network getPermalink) keeps the live snapshot fast.
func (s *Server) resolveSteeringRunOrigin(ctx context.Context, run *steeringRun) {
	if run.Source == "github" {
		run.ChannelName = run.Channel // owner/repo
		run.AuthorName = run.Author   // login
		run.Permalink = run.URL
		return
	}
	ch, ts := run.Channel, run.TS
	if ch == "" || ts == "" {
		tkChan, tkTS := splitThreadKey(run.ThreadKey)
		if ch == "" {
			ch = tkChan
		}
		if ts == "" {
			ts = tkTS
		}
	}
	if run.Channel == "" {
		run.Channel = ch // so the UI's channel_name || channel fallback shows something
	}
	if s.nameResolver != nil {
		run.ChannelName = s.nameResolver.ChannelName(ctx, ch)
		run.AuthorName = s.nameResolver.UserName(ctx, run.Author)
	}
	// DMs have no channel name — label by the person instead.
	if run.ChannelName == "" && (run.ChannelType == "im" || run.ChannelType == "mpim") {
		if run.AuthorName != "" {
			run.ChannelName = "DM · " + run.AuthorName
		} else {
			run.ChannelName = "Direct message"
		}
	}
	run.Permalink = connectorPermalink(run.Source, run.TeamID, ch, ts, run.URL)
}
