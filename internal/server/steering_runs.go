package server

import (
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
	writeJSON(w, map[string]any{"runs": runs})
}
