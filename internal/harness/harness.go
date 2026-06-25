// Package harness defines the agent runtime abstraction used by flow.
package harness

import "fmt"

// Name is the short identifier persisted on tasks.harness.
type Name string

const (
	NameClaude Name = "claude"
	NameCodex  Name = "codex"
)

// Harness is the minimal runtime identity every supported agent adapter
// exposes. Launch command construction still lives in the app package because
// Flow Manager already has provider-specific command builders there.
type Harness interface {
	Name() Name
	Provider() string
	Binary() string
}

// BackgroundAgent is one entry from a harness-owned background-agent registry.
type BackgroundAgent struct {
	ShortID   string
	SessionID string
	Name      string
	Cwd       string
	PID       int
	Status    string
	State     string
}

// BackgroundLauncher is an optional harness capability used by FLOW_TERM=bg.
type BackgroundLauncher interface {
	SpawnBackground(workDir, name, prompt string, opts LaunchOpts) (BackgroundAgent, error)
	ResumeBackground(workDir, sessionID string, opts LaunchOpts) (BackgroundAgent, error)
	BackgroundAgents() ([]BackgroundAgent, error)
}

// LaunchOpts carries command-line choices shared by background launches.
type LaunchOpts struct {
	PermissionMode string
	Model          string
	Effort         string
	Inject         string
}

const InjectionMarker = "[via flow do --with]"

// NormalizeName canonicalizes a persisted harness name.
func NormalizeName(name string) (Name, error) {
	switch name {
	case "", "claude", "claude-code", "claudecode":
		return NameClaude, nil
	case "codex", "codex-cli":
		return NameCodex, nil
	default:
		return "", fmt.Errorf("harness must be claude|codex, got %q", name)
	}
}
