package productdb

// normalize.go holds the flowdb-free twins of the value normalizers the product
// surface needs (session provider, permission mode, priority, harness). They are
// verbatim mirrors of the flowdb originals so a flowdb.X → productdb.X swap is
// behavior-identical; parity is covered by normalize_test.go.

import (
	"fmt"
	"strings"
)

// DefaultPermissionMode is used when callers do not explicitly choose one.
const DefaultPermissionMode = "auto"

// NormalizePermissionMode canonicalizes the task-level agent permission mode.
func NormalizePermissionMode(mode string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "":
		return DefaultPermissionMode, nil
	case "default":
		return "default", nil
	case "auto":
		return "auto", nil
	case "bypass", "bypasspermissions", "dangerously-skip-permissions", "dangerously_skip_permissions":
		return "bypass", nil
	default:
		return "", fmt.Errorf("permission mode must be default|auto|bypass, got %q", mode)
	}
}

// NormalizePriority canonicalizes a task or project priority value.
func NormalizePriority(priority string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(priority)) {
	case "high", "h":
		return "high", nil
	case "", "medium", "med", "m":
		return "medium", nil
	case "low", "l":
		return "low", nil
	default:
		return "", fmt.Errorf("priority must be high|medium|low, got %q", priority)
	}
}

// NormalizeSessionProvider canonicalizes the agent/provider used for a task
// session.
func NormalizeSessionProvider(provider string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(provider)) {
	case "", "claude", "claude-code", "claudecode":
		return "claude", nil
	case "codex", "codex-cli":
		return "codex", nil
	default:
		return "", fmt.Errorf("session provider must be claude|codex, got %q", provider)
	}
}

// NormalizeHarnessName canonicalizes the runtime harness stored on a task.
func NormalizeHarnessName(harness string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(harness)) {
	case "", "claude", "claude-code", "claudecode":
		return "claude", nil
	case "codex", "codex-cli":
		return "codex", nil
	default:
		return "", fmt.Errorf("harness must be claude|codex, got %q", harness)
	}
}
