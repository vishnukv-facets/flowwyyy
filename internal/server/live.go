package server

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var psRunner = func() ([]byte, error) {
	return exec.Command("ps", "-axo", "pid,command").Output()
}

var claudeSessionArgRe = regexp.MustCompile(
	`(?:--session-id|--resume)[ =]([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12})`,
)

func liveAgentSessions() (map[string]bool, error) {
	out, err := psRunner()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	live := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "claude") {
			matches := claudeSessionArgRe.FindAllStringSubmatch(line, -1)
			for _, match := range matches {
				if len(match) >= 2 {
					live[strings.ToLower(match[1])] = true
				}
			}
		}
		if strings.Contains(lower, "codex") {
			// Resumed sessions (`codex resume <id>`) carry the session id.
			if strings.Contains(lower, "resume") {
				for _, id := range anySessionUUIDs(line) {
					live[strings.ToLower(id)] = true
				}
			}
			// FRESH sessions (launched with a prompt) generate their id
			// internally, so it never appears on the command line — but the
			// working dir (-C) always does, for both fresh and resumed. Index
			// it so liveness can match by workdir when the id is absent.
			for _, dir := range codexWorkdirsInLine(line) {
				live[codexDirLiveKeyPrefix+dir] = true
			}
		}
	}
	return live, nil
}

// codexWorkdirRe captures the working directory codex is launched with
// (`-C <dir>`). A fresh codex session's session id is internal and never on the
// command line, so workdir matching is what lets liveness detect it as alive.
var codexWorkdirRe = regexp.MustCompile(`(?:^|\s)(?:-C|--cd)[ =](\S+)`)

const codexDirLiveKeyPrefix = "codexdir:"

func codexWorkdirsInLine(line string) []string {
	var out []string
	for _, m := range codexWorkdirRe.FindAllStringSubmatch(line, -1) {
		if len(m) >= 2 {
			out = append(out, filepath.Clean(m[1]))
		}
	}
	return out
}

// codexDirLiveKey is the map key used to record/look up a live codex working
// directory in the liveAgentSessions() map.
func codexDirLiveKey(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	return codexDirLiveKeyPrefix + filepath.Clean(dir)
}

// cachedLiveAgentSessions wraps liveAgentSessions with a per-server TTL cache.
// SSE-driven endpoints (events.go ticks at 2s) would otherwise fork `ps` on
// every tick — and historically multiple call sites within one buildUIData
// each forked their own `ps`. The cache collapses concurrent and back-to-back
// calls into one fork per 1.5s window.
func (s *Server) cachedLiveAgentSessions() (map[string]bool, error) {
	if s == nil || s.caches == nil {
		return liveAgentSessions()
	}
	return s.caches.live.load(time.Now(), liveAgentSessions)
}

func anySessionUUIDs(line string) []string {
	re := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	return re.FindAllString(line, -1)
}
