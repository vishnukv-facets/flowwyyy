package server

import (
	"context"
	"flow/internal/flowdb"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func terminalEnv(flowRoot, commandPath string) []string {
	env := os.Environ()
	env = setEnvValue(env, "TERM", "xterm-256color")
	env = setEnvValue(env, "COLORTERM", "truecolor")
	env = setEnvValue(env, "FORCE_COLOR", "1")
	env = setEnvValue(env, "TERM_PROGRAM", "flow-ui")
	env = setEnvValue(env, "CLAUDE_CODE_NO_FLICKER", "0")
	env = setEnvValue(env, "CLAUDE_CODE_DISABLE_MOUSE", "1")
	env = appendEnvDefault(env, "LANG", "en_US.UTF-8")
	env = appendEnvDefault(env, "LC_CTYPE", "en_US.UTF-8")
	// Mark this PTY as flow-spawned for the agent-event hook (see
	// internal/app/hook.go injectHookMetadata). Lets the server tell
	// apart flow-managed sessions from ambient agents in the same repo.
	env = setEnvValue(env, "FLOW_HOOK_OWNED", "1")
	if root := strings.TrimSpace(flowRoot); root != "" {
		env = setEnvValue(env, "FLOW_ROOT", root)
	} else if root := os.Getenv("FLOW_ROOT"); root != "" {
		env = setEnvValue(env, "FLOW_ROOT", root)
	}
	// Make `gh` work inside sandboxed agent sessions. Codex's workspace-write
	// sandbox can't read the macOS Keychain where `gh` stores its token, so
	// `gh` fails to authenticate even with network enabled. Resolve the token
	// here (in the server, outside any sandbox) and pass it via GH_TOKEN, which
	// gh prefers over keychain/config. No-op if a token is already in the env
	// or can't be resolved (e.g. gh not logged in).
	if envValueLocal(env, "GH_TOKEN") == "" && envValueLocal(env, "GITHUB_TOKEN") == "" {
		if tok := ghAuthToken(); tok != "" {
			env = setEnvValue(env, "GH_TOKEN", tok)
		}
	}
	return prependCommandDirToPath(env, commandPath)
}

// ghAuthToken resolves the gh CLI token from the server's environment (outside
// any agent sandbox). Overridable in tests. Returns "" when gh is unavailable
// or not logged in.
var ghAuthToken = func() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "-h", "github.com").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func terminalEnvWithHook(flowRoot, commandPath, hookURL string) []string {
	env := terminalEnv(flowRoot, commandPath)
	if hookURL = strings.TrimSpace(hookURL); hookURL != "" {
		env = setEnvValue(env, "FLOW_HOOK_URL", hookURL)
	}
	return env
}

func terminalEnvMap(flowRoot, commandPath, hookURL, slug, provider, permissionMode string, freeAgent bool) map[string]string {
	env := terminalEnvWithHook(flowRoot, commandPath, hookURL)
	out := map[string]string{
		"TERM":                      "xterm-256color",
		"COLORTERM":                 "truecolor",
		"FORCE_COLOR":               "1",
		"TERM_PROGRAM":              "flow-ui",
		"CLAUDE_CODE_NO_FLICKER":    "0",
		"CLAUDE_CODE_DISABLE_MOUSE": "1",
		"LANG":                      "en_US.UTF-8",
		"LC_CTYPE":                  "en_US.UTF-8",
		"FLOW_HOOK_OWNED":           "1",
		"FLOW_SESSION_PROVIDER":     provider,
		"FLOW_PERMISSION_MODE":      normalizedTerminalPermissionMode(permissionMode),
	}
	if freeAgent {
		out["FLOW_FREE_AGENT"] = "1"
	} else {
		out["FLOW_TASK"] = slug
	}
	for _, key := range []string{"PATH", "FLOW_ROOT", "FLOW_HOOK_URL", "GH_TOKEN"} {
		if value := envValueLocal(env, key); value != "" {
			out[key] = value
		}
	}
	return out
}

func normalizedTerminalPermissionMode(mode string) string {
	normalized, err := flowdb.NormalizePermissionMode(mode)
	if err != nil {
		return flowdb.DefaultPermissionMode
	}
	return normalized
}

func setEnvValue(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func appendEnvDefault(env []string, key, value string) []string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return env
		}
	}
	return append(env, prefix+value)
}

func prependCommandDirToPath(env []string, commandPath string) []string {
	commandPath = strings.TrimSpace(commandPath)
	if commandPath == "" {
		return env
	}
	dir := filepath.Dir(commandPath)
	if dir == "." || dir == "" {
		return env
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	current := envValueLocal(env, "PATH")
	if current == "" {
		return setEnvValue(env, "PATH", dir)
	}
	for _, part := range filepath.SplitList(current) {
		if part == dir {
			return env
		}
	}
	return setEnvValue(env, "PATH", dir+string(os.PathListSeparator)+current)
}

func envValueLocal(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}
