package steering

import (
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func TestAutonomyFromEnv(t *testing.T) {
	t.Run("empty → all off defaults", func(t *testing.T) {
		t.Setenv("FLOW_STEERING_AUTONOMY", "")
		p := AutonomyFromEnv()
		if p.Allow(ActionMakeTask, 1.0) {
			t.Error("empty env should leave make_task off")
		}
	})
	t.Run("bad JSON → safe defaults (never accidentally ON)", func(t *testing.T) {
		t.Setenv("FLOW_STEERING_AUTONOMY", "{not json")
		if AutonomyFromEnv().Allow(ActionForward, 1.0) {
			t.Error("malformed JSON must not enable any action")
		}
	})
	t.Run("override enables an action above threshold", func(t *testing.T) {
		t.Setenv("FLOW_STEERING_AUTONOMY", `{"make_task":{"enabled":true,"threshold":0.7}}`)
		p := AutonomyFromEnv()
		if !p.Allow(ActionMakeTask, 0.75) {
			t.Error("make_task should be allowed at 0.75 (>= 0.7)")
		}
		if p.Allow(ActionMakeTask, 0.6) {
			t.Error("make_task should be denied at 0.6 (< 0.7)")
		}
		// unspecified actions keep the safe default (off)
		if p.Allow(ActionReply, 1.0) {
			t.Error("reply not in override should stay off")
		}
	})
	t.Run("threshold clamped to [0,1]", func(t *testing.T) {
		t.Setenv("FLOW_STEERING_AUTONOMY", `{"forward":{"enabled":true,"threshold":5}}`)
		// threshold clamps to 1.0, so even 0.99 is denied
		if AutonomyFromEnv().Allow(ActionForward, 0.99) {
			t.Error("threshold 5 should clamp to 1.0 → deny 0.99")
		}
	})
	t.Run("unknown action key ignored", func(t *testing.T) {
		t.Setenv("FLOW_STEERING_AUTONOMY", `{"frobnicate":{"enabled":true,"threshold":0}}`)
		_ = AutonomyFromEnv() // must not panic; unknown key ignored
	})
}

func TestDefaultAutonomyIsSurfaceOnly(t *testing.T) {
	p := DefaultAutonomy()
	for _, a := range []Action{ActionMakeTask, ActionForward, ActionCaptureKB, ActionDigestOnly, ActionReply, ActionAFKReply} {
		if p.Allow(a, 1.0) {
			t.Errorf("DefaultAutonomy allowed %q at confidence 1.0; want surface-only (deny)", a)
		}
	}
}

func TestAutonomyFromEnvEnablesAndFailSafesNewSafeActions(t *testing.T) {
	t.Run("capture_kb + dismiss opt in above their thresholds", func(t *testing.T) {
		t.Setenv("FLOW_STEERING_AUTONOMY", `{"capture_kb":{"enabled":true,"threshold":0.75},"digest_only":{"enabled":true,"threshold":0.85}}`)
		p := AutonomyFromEnv()
		if !p.Allow(ActionCaptureKB, 0.75) {
			t.Error("capture_kb should be allowed at 0.75 (>= 0.75)")
		}
		if p.Allow(ActionCaptureKB, 0.74) {
			t.Error("capture_kb should be denied at 0.74 (< 0.75)")
		}
		if !p.Allow(ActionDigestOnly, 0.90) {
			t.Error("dismiss (digest_only) should be allowed at 0.90")
		}
		if p.Allow(ActionDigestOnly, 0.80) {
			t.Error("dismiss (digest_only) should be denied at 0.80 (< 0.85)")
		}
	})
	t.Run("malformed policy never enables capture_kb or dismiss", func(t *testing.T) {
		t.Setenv("FLOW_STEERING_AUTONOMY", "{garbage")
		p := AutonomyFromEnv()
		if p.Allow(ActionCaptureKB, 1.0) || p.Allow(ActionDigestOnly, 1.0) {
			t.Error("malformed JSON must leave the new safe actions off")
		}
	})
}

func TestAutonomyLadderDocumentsPolicyMatrix(t *testing.T) {
	ladder := AutonomyLadder()
	if len(ladder) != 9 {
		t.Fatalf("ladder length = %d, want the nine trust-ladder steps", len(ladder))
	}

	byKey := map[string]AutonomyCapability{}
	for _, step := range ladder {
		byKey[step.Key] = step
		if step.Risk == "" || step.Audit == "" || step.Prerequisites == "" {
			t.Fatalf("step %q missing risk/audit/prerequisites: %+v", step.Key, step)
		}
	}

	if got := byKey["surface"].Risk; got != "none" {
		t.Errorf("surface risk = %q, want none", got)
	}
	if step := byKey["forward"]; !step.Configurable || !step.AutoActable || step.DefaultEnabled || step.DefaultThreshold != 0.85 {
		t.Errorf("forward step = %+v, want configurable auto-actable off-by-default at 0.85", step)
	}
	if step := byKey["make_task"]; !step.Configurable || !step.AutoActable || step.DefaultEnabled || step.DefaultThreshold != 0.80 {
		t.Errorf("make_task step = %+v, want configurable auto-actable off-by-default at 0.80", step)
	}
	if step := byKey["capture_kb"]; !step.Configurable || !step.AutoActable || step.DefaultEnabled || step.DefaultThreshold != 0.75 || step.Action != ActionCaptureKB {
		t.Errorf("capture_kb step = %+v, want configurable auto-actable off-by-default at 0.75", step)
	}
	if step := byKey["dismiss"]; !step.Configurable || !step.AutoActable || step.DefaultEnabled || step.DefaultThreshold != 0.85 || step.Action != ActionDigestOnly {
		t.Errorf("dismiss step = %+v, want configurable auto-actable off-by-default at 0.85 gating digest_only", step)
	}
	if step := byKey["reply"]; step.AutoActable || step.Configurable {
		t.Errorf("reply step = %+v, want never auto-actable/configurable today", step)
	}
	if step := byKey["clear_waiting_on"]; step.DefaultEnabled || step.Configurable {
		t.Errorf("clear_waiting_on step = %+v, want documented but not controlled by FLOW_STEERING_AUTONOMY", step)
	}
}

func TestAutonomyEvaluateExplainsDecision(t *testing.T) {
	p := AutonomyPolicy{
		ActionMakeTask: {Enabled: true, Threshold: 0.80},
		ActionForward:  {Enabled: false, Threshold: 0.85},
		ActionReply:    {Enabled: true, Threshold: 0.10},
	}

	allowed := p.Evaluate(ActionMakeTask, 0.92)
	if !allowed.Allowed || allowed.Decision != "allowed" || allowed.Threshold != 0.80 {
		t.Fatalf("make_task allowed decision = %+v", allowed)
	}
	if !strings.Contains(allowed.Reason, "confidence 0.92 >= threshold 0.80") {
		t.Errorf("allowed reason = %q, want confidence/threshold explanation", allowed.Reason)
	}

	disabled := p.Evaluate(ActionForward, 0.99)
	if disabled.Allowed || disabled.Decision != "disabled" {
		t.Fatalf("forward disabled decision = %+v, want disabled", disabled)
	}

	manualOnly := p.Evaluate(ActionReply, 1.0)
	if manualOnly.Allowed || manualOnly.Decision != "manual_only" {
		t.Fatalf("reply decision = %+v, want manual_only despite env enabling it", manualOnly)
	}

	below := p.Evaluate(ActionMakeTask, 0.50)
	if below.Allowed || below.Decision != "below_threshold" {
		t.Fatalf("below-threshold decision = %+v, want below_threshold", below)
	}
}

func TestAutonomyAllow(t *testing.T) {
	p := AutonomyPolicy{
		ActionForward:  {Enabled: true, Threshold: 0.85},
		ActionAFKReply: {Enabled: false, Threshold: 0.90},
	}
	cases := []struct {
		action     Action
		confidence float64
		want       bool
	}{
		{ActionForward, 0.90, true},
		{ActionForward, 0.85, true},
		{ActionForward, 0.80, false},
		{ActionAFKReply, 0.99, false},
		{ActionReply, 1.0, false},
	}
	for _, c := range cases {
		if got := p.Allow(c.action, c.confidence); got != c.want {
			t.Errorf("Allow(%q, %.2f) = %v, want %v", c.action, c.confidence, got, c.want)
		}
	}
}

func TestAutonomyFnWithFeedbackAdjustsThresholdsWithoutEnablingActions(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	for i := 0; i < 3; i++ {
		if err := flowdb.RecordAttentionFeedback(db, flowdb.AttentionFeedback{
			ID: "approved-forward-" + string(rune('a'+i)), FeedItemID: "fa", Source: "slack",
			Channel: "C_SIGNAL", Author: "U_OK", ThreadType: "channel", ThreadKey: "C_SIGNAL:1",
			SuggestedAction: "forward", FinalAction: "forward", Outcome: "approved",
			Confidence: 0.76, ConfidenceBand: "0.70-0.79", CreatedAt: "2026-06-05T10:00:00Z",
		}); err != nil {
			t.Fatalf("record approval %d: %v", i, err)
		}
		if err := flowdb.RecordAttentionFeedback(db, flowdb.AttentionFeedback{
			ID: "dismiss-reply-" + string(rune('a'+i)), FeedItemID: "fd", Source: "slack",
			Channel: "C_NOISE", Author: "U_NO", ThreadType: "channel", ThreadKey: "C_NOISE:1",
			SuggestedAction: "reply", FinalAction: "dismiss", Outcome: "dismissed",
			Confidence: 0.86, ConfidenceBand: "0.80-0.89", CreatedAt: "2026-06-05T11:00:00Z",
		}); err != nil {
			t.Fatalf("record dismiss %d: %v", i, err)
		}
	}

	base := func() AutonomyPolicy {
		return AutonomyPolicy{
			ActionForward:  {Enabled: true, Threshold: 0.85},
			ActionReply:    {Enabled: true, Threshold: 0.90},
			ActionMakeTask: {Enabled: false, Threshold: 0.80},
		}
	}
	pol := AutonomyFnWithFeedback(db, base)()
	if got := pol[ActionForward].Threshold; got != 0.80 {
		t.Errorf("forward threshold = %.2f, want 0.80 after approvals", got)
	}
	if got := pol[ActionReply].Threshold; got != 0.95 {
		t.Errorf("reply threshold = %.2f, want 0.95 after dismissals", got)
	}
	if pol[ActionMakeTask].Enabled {
		t.Error("feedback overlay must not enable a disabled action")
	}
}
