package monitor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func GitHubPollingEnabled() bool {
	return envBoolDefault("FLOW_GH_ENABLED", false) && len(GitHubSelfLogins()) > 0
}

func GitHubSelfLogins() []string {
	return splitList(os.Getenv("FLOW_GH_SELF_LOGINS"))
}

func GitHubRepos() []string {
	return splitList(os.Getenv("FLOW_GH_REPOS"))
}

func GitHubPollInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("FLOW_GH_POLL_INTERVAL"))
	if raw == "" {
		return time.Minute
	}
	d, err := time.ParseDuration(raw)
	if err == nil && d > 0 {
		return d
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return time.Minute
}

func splitList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		v := strings.TrimSpace(part)
		if v == "" {
			continue
		}
		k := strings.ToLower(v)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}

type trackedGitHubPR struct {
	Owner  string
	Repo   string
	Number int
}

func parseGitHubRefTag(tag, prefix string) (trackedGitHubPR, bool) {
	raw := strings.TrimPrefix(strings.TrimSpace(tag), prefix)
	if raw == tag || raw == "" {
		return trackedGitHubPR{}, false
	}
	repo, numText, ok := strings.Cut(raw, "#")
	if !ok {
		return trackedGitHubPR{}, false
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return trackedGitHubPR{}, false
	}
	n, err := strconv.Atoi(numText)
	if err != nil || n <= 0 {
		return trackedGitHubPR{}, false
	}
	return trackedGitHubPR{Owner: owner, Repo: name, Number: n}, true
}

type githubUser struct {
	Login string `json:"login"`
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubMilestone struct {
	Title string `json:"title"`
}

type githubRef struct {
	Name string `json:"ref"`
	SHA  string `json:"sha"`
}

func linkTagForRecord(kind GitHubEventKind, owner, repo string, number int) string {
	if isIssueKind(kind) {
		return fmt.Sprintf("gh-issue:%s/%s#%d", owner, repo, number)
	}
	return fmt.Sprintf("gh-pr:%s/%s#%d", owner, repo, number)
}
