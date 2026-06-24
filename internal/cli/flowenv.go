package cli

// flowenv.go holds flowwyyy's OWN flag-parsing and flow-root/path/URL helpers —
// the flowdb-free, app-free utilities the product surface (server/product) needs
// so it can stop importing internal/app (Phase-3 decoupling, seam §11.3.1, Tier
// A). They mirror the equivalent unexported helpers in internal/app: in the
// two-binary world the official `flow` binary keeps its copies under internal/,
// and flowwyyy keeps these — the two never share code, which is the whole point
// of the split. All are pure (flag/os/env/filepath); none touch flowdb.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FlagSet creates a named flag.FlagSet that prints errors instead of exiting
// (flag.ContinueOnError) — the flow CLI flag-parsing contract.
func FlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// LeadingHelpArg reports whether args starts with -h/--help.
func LeadingHelpArg(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help")
}

// ParseFlagSet parses args with the standard flow flag handling: a clean help
// request returns (true, 0); any other parse error returns (true, 2); success
// returns (false, 0).
func ParseFlagSet(fs *flag.FlagSet, args []string) (handled bool, rc int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, 0
		}
		return true, 2
	}
	return false, 0
}

// FlowRoot returns the root directory for flow state: $FLOW_ROOT if set, else
// ~/.flow. The path is not guaranteed to exist.
func FlowRoot() (string, error) {
	if r := os.Getenv("FLOW_ROOT"); r != "" {
		return r, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home dir: %w", err)
	}
	return filepath.Join(home, ".flow"), nil
}

// FlowDBPath returns the absolute path to flow.db under the flow root, with a
// clear error if the data directory hasn't been initialized (`flow init`).
func FlowDBPath() (string, error) {
	root, err := FlowRoot()
	if err != nil {
		return "", err
	}
	dbPath := filepath.Join(root, "flow.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", fmt.Errorf("flow is not initialized — run `flow init` to set up %s", root)
	}
	return dbPath, nil
}

// FlowServerURL builds a Mission Control URL for path, honoring $FLOW_UI_URL
// (default http://127.0.0.1:8787).
func FlowServerURL(path string) string {
	endpoint := strings.TrimSpace(os.Getenv("FLOW_UI_URL"))
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8787"
	}
	return strings.TrimRight(endpoint, "/") + path
}

// UISessionToken reads the data-plane session token the running flow server
// minted (<FlowRoot>/.ui-session-token, 0600). Returns "" when the server isn't
// running or the file isn't readable, so callers degrade to an unauthenticated
// request (which the gated route answers with 403).
func UISessionToken() string {
	root, err := FlowRoot()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(root, SessionTokenFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// PreferredUIFlowBinary picks which binary UI-related child processes re-exec:
// the currently-running executable when known, never a bare "flow" PATH lookup
// (which could launch a stale installed build).
func PreferredUIFlowBinary(current string) string {
	if strings.TrimSpace(current) != "" {
		return current
	}
	return "flow"
}
