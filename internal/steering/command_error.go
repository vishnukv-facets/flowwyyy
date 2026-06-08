package steering

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func commandError(prefix string, err error, stdout []byte) error {
	detail := commandErrorDetail(err, stdout)
	if detail == "" {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return fmt.Errorf("%s: %w (%s)", prefix, err, detail)
}

func commandErrorDetail(err error, stdout []byte) string {
	parts := []string{}
	if out := strings.TrimSpace(string(stdout)); out != "" {
		parts = append(parts, "stdout: "+out)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			parts = append(parts, "stderr: "+stderr)
		}
	}
	return strings.Join(parts, "; ")
}
