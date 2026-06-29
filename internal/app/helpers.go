package app

import (
	"database/sql"
	"errors"
	"flag"
	"flow/internal/flowdb"
	"os"
	"strings"
)

// flagSet creates a named flag.FlagSet that prints errors instead of exiting.
func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func leadingHelpArg(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help")
}

func parseFlagSet(fs *flag.FlagSet, args []string) (handled bool, rc int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, 0
		}
		return true, 2
	}
	return false, 0
}

type currentAgentSession struct {
	Provider string
	ID       string
	EnvVar   string
}

// currentSessionForProvider returns the current agent session handle.
// Claude Code exposes CLAUDE_CODE_SESSION_ID; Codex exposes CODEX_THREAD_ID.
// If a provider was requested explicitly, prefer that provider's env var so
// `flow do --here --agent codex` validates against the Codex thread even if
// another host variable is present in a nested shell.
func currentSessionForProvider(provider string) currentAgentSession {
	if provider == sessionProviderCodex {
		if session := currentCodexSession(); session.ID != "" {
			return session
		}
		return currentClaudeSession()
	}
	if provider == sessionProviderClaude {
		if session := currentClaudeSession(); session.ID != "" {
			return session
		}
		return currentCodexSession()
	}
	switch strings.TrimSpace(strings.ToLower(os.Getenv("FLOW_SESSION_PROVIDER"))) {
	case sessionProviderCodex:
		if session := currentCodexSession(); session.ID != "" {
			return session
		}
	case sessionProviderClaude:
		if session := currentClaudeSession(); session.ID != "" {
			return session
		}
	}
	if session := currentClaudeSession(); session.ID != "" {
		return session
	}
	return currentCodexSession()
}

func currentClaudeSession() currentAgentSession {
	if sid := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID")); sid != "" {
		return currentAgentSession{Provider: sessionProviderClaude, ID: sid, EnvVar: "CLAUDE_CODE_SESSION_ID"}
	}
	return currentAgentSession{}
}

func currentCodexSession() currentAgentSession {
	if sid := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")); sid != "" {
		return currentAgentSession{Provider: sessionProviderCodex, ID: sid, EnvVar: "CODEX_THREAD_ID"}
	}
	if sid := strings.TrimSpace(os.Getenv("CODEX_SESSION_ID")); sid != "" {
		return currentAgentSession{Provider: sessionProviderCodex, ID: sid, EnvVar: "CODEX_SESSION_ID"}
	}
	return currentAgentSession{}
}

// currentSessionID returns this process's current Claude/Codex session id,
// or "" if neither host exposes one.
func currentSessionID() string {
	return currentSessionForProvider("").ID
}

// currentSessionTask returns the task bound to this Claude/Codex session
// via tasks.session_id. Returns sql.ErrNoRows if the current session
// is unbound (dispatch session) or the env var is missing.
func currentSessionTask(db *sql.DB) (*flowdb.Task, error) {
	return flowdb.TaskBySessionID(db, currentSessionID())
}

// isNoBindingErr is a small predicate for the dispatch-session case.
// Callers use it to differentiate "no current binding" from real
// scan errors when reverse-looking-up by $CLAUDE_CODE_SESSION_ID.
func isNoBindingErr(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
