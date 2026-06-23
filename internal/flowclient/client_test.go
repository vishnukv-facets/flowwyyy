package flowclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeFlow(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flow")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCapturesStdoutStderrAndExitCode(t *testing.T) {
	bin := fakeFlow(t, "echo out; echo err >&2; exit 7\n")
	stdout, stderr, code, err := Client{Bin: bin}.Run(context.Background(), "demo")
	if err == nil {
		t.Fatal("Run err = nil, want exit error")
	}
	if stdout != "out\n" || stderr != "err\n" || code != 7 {
		t.Fatalf("Run() = stdout %q stderr %q code %d", stdout, stderr, code)
	}
}

func TestTypedHelpersBuildExpectedArguments(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args.log")
	bin := fakeFlow(t, "printf '%s\\n' \"$@\" > '"+log+"'\n")

	if _, err := (Client{Bin: bin}).RunPlaybook(context.Background(), "demo", "--auto"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(raw)), "run\nplaybook\ndemo\n--auto"; got != want {
		t.Fatalf("RunPlaybook args = %q, want %q", got, want)
	}

	if _, err := (Client{Bin: bin}).Done(context.Background(), "task-slug"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(log)
	if got, want := strings.TrimSpace(string(raw)), "done\ntask-slug"; got != want {
		t.Fatalf("Done args = %q, want %q", got, want)
	}
}
