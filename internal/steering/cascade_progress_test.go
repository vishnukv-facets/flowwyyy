package steering

import (
	"context"
	"flow/internal/productdb"
	"strings"
	"testing"
)

// captureStages wires c.Progress to append into a slice, so a test can assert on
// the live stage events the cascade emits as it runs.
func captureStages(c *Cascade) *[]StageEvent {
	var events []StageEvent
	c.Progress = func(e StageEvent) { events = append(events, e) }
	return &events
}

func stageSeq(events []StageEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Stage+"/"+e.Status)
	}
	return out
}

func TestObserveEmitsStageProgressForSurfacedRun(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	events := captureStages(c)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:11.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.72,"summary":"do it"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"make_task","confidence":0.9,"summary":"deep","draft":""}`, nil
	})

	if err := c.Observe(context.Background(), msg("C1", "11.1", "U_OTHER", "please do this")); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	got := stageSeq(*events)
	want := []string{"received/running", "stage0/passed", "stage1/running", "stage2/running", "stage3/running", "verdict/surfaced"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stage sequence = %v\nwant %v", got, want)
	}

	// Every stage event shares the run id, and that id is the trace id — a live
	// run and its persisted trace are the same object.
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(*traces))
	}
	runID := (*traces)[0].ID
	for _, e := range *events {
		if e.RunID != runID {
			t.Fatalf("stage %s RunID = %q, want %q", e.Stage, e.RunID, runID)
		}
		if e.At == "" {
			t.Fatalf("stage %s has empty At timestamp", e.Stage)
		}
	}
	// Terminal event carries the final decision for the UI.
	last := (*events)[len(*events)-1]
	if !strings.Contains(last.Detail, "make_task") {
		t.Errorf("verdict detail = %q, want it to name the final action", last.Detail)
	}
}

func TestObserveEmitsStageProgressForStage0Drop(t *testing.T) {
	c, _ := cascadeFixture(t)
	events := captureStages(c)
	// Self-authored (U_ME) → Stage 0 drops before any model call.
	if err := c.Observe(context.Background(), msg("C1", "10.1", "U_ME", "note")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	got := stageSeq(*events)
	want := []string{"received/running", "verdict/dropped"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stage sequence = %v\nwant %v (a Stage0 drop must not emit stage1/2/3)", got, want)
	}
	if d := (*events)[len(*events)-1].Detail; !strings.Contains(d, "self-authored") {
		t.Errorf("verdict detail = %q, want the drop reason", d)
	}
}

func TestStageEmitIsNoOpWithoutHook(t *testing.T) {
	c, _ := cascadeFixture(t)
	// No Progress hook set: stage() must be a safe no-op (and tolerate a nil tr).
	c.stage(nil, c.now(), "received", "running", "x")
	c.stage(&productdb.SteeringTrace{ID: "id1"}, c.now(), "stage0", "passed", "x")
}
