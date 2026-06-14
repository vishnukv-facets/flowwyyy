package server

import (
	"fmt"
	"os/exec"
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
		if strings.Contains(lower, "codex") && strings.Contains(lower, "resume") {
			for _, id := range anySessionUUIDs(line) {
				live[strings.ToLower(id)] = true
			}
		}
	}
	return live, nil
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
