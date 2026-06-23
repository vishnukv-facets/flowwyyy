package flowclient

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

var executablePath = os.Executable

func Resolve() (string, error) {
	if b := os.Getenv("FLOW_BIN"); b != "" {
		if isFile(b) {
			return b, nil
		}
	}
	if exe, err := executablePath(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "flow")
		if isFile(cand) {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("flow"); err == nil {
		return p, nil
	}
	return "", errors.New("flow binary not found: set $FLOW_BIN, install flow on PATH, or place it beside flowwyyy")
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
