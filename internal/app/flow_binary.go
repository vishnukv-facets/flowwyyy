package app

import "strings"

// preferredUIFlowBinary picks which binary UI-related child processes re-exec.
// It must be the binary currently running (os.Executable()), NOT a bare "flow"
// PATH lookup: resolving "flow" via $PATH can launch a stale installed build.
func preferredUIFlowBinary(current string) string {
	if strings.TrimSpace(current) != "" {
		return current
	}
	return "flow"
}
