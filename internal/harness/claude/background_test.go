package claude

import (
	"testing"

	"flow/internal/harness"
)

func TestParseBackgroundBanner(t *testing.T) {
	short, err := parseBackgroundBanner("backgrounded · 1a2b3c4d · flow task\nattach with claude attach 1a2b3c4d")
	if err != nil {
		t.Fatalf("parseBackgroundBanner: %v", err)
	}
	if short != "1a2b3c4d" {
		t.Fatalf("short id = %q, want 1a2b3c4d", short)
	}
}

func TestParseBackgroundAgentsFiltersBackgroundKind(t *testing.T) {
	agents, err := parseBackgroundAgents([]byte(`[
		{"kind":"background","id":"1a2b3c4d","sessionId":"11111111-1111-4111-8111-111111111111","name":"one","cwd":"/repo","pid":123,"status":"busy","state":"working"},
		{"kind":"interactive","id":"aaaaaaaa","sessionId":"22222222-2222-4222-8222-222222222222","name":"two","cwd":"/repo","pid":456,"status":"busy","state":"working"}
	]`))
	if err != nil {
		t.Fatalf("parseBackgroundAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents len = %d, want 1", len(agents))
	}
	if agents[0].ShortID != "1a2b3c4d" || agents[0].SessionID == "" || agents[0].PID != 123 {
		t.Fatalf("unexpected agent: %+v", agents[0])
	}
}

func TestSpawnBackgroundCapturesSessionID(t *testing.T) {
	old := BGCommandRunner
	t.Cleanup(func() { BGCommandRunner = old })
	BGCommandRunner = func(workDir string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "agents" {
			return []byte(`[{"kind":"background","id":"1a2b3c4d","sessionId":"11111111-1111-4111-8111-111111111111","name":"task","cwd":"/repo","pid":123,"status":"busy","state":"working"}]`), nil
		}
		if workDir != "/repo" {
			t.Fatalf("workDir = %q, want /repo", workDir)
		}
		if !containsArgPair(args, "--effort", "xhigh") {
			t.Fatalf("spawn args missing --effort xhigh: %#v", args)
		}
		return []byte("backgrounded · 1a2b3c4d · task\n"), nil
	}

	agent, err := New().(harness.BackgroundLauncher).SpawnBackground("/repo", "task", "prompt", harness.LaunchOpts{PermissionMode: "bypass", Effort: "xhigh"})
	if err != nil {
		t.Fatalf("SpawnBackground: %v", err)
	}
	if agent.SessionID != "11111111-1111-4111-8111-111111111111" || agent.ShortID != "1a2b3c4d" {
		t.Fatalf("unexpected captured agent: %+v", agent)
	}
}

func containsArgPair(args []string, key, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == val {
			return true
		}
	}
	return false
}
