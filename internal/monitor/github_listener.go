package monitor

import (
	"context"
	"sync"
	"time"
)

// GitHubListener owns two things in the webhook-first world: it dispatches
// normalized webhook events (Dispatch, called by the server's webhook
// receiver), and it runs a lightweight "linker-tick" that tags in-progress
// tasks with the PRs opened on their branch / cross-referencing their issue.
// The tick exists because GitHub emits no webhook event for a self-authored PR
// being opened, so cross-ref linking can't be event-driven. The expensive
// gh-api search-poller it replaced is gone.
type GitHubListener struct {
	dispatcher *GitHubDispatcher

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}

	linkInterval time.Duration
	logFn        func(string, ...any)
}

func NewGitHubListener(d *GitHubDispatcher) *GitHubListener {
	if d == nil {
		return nil
	}
	return &GitHubListener{
		dispatcher:   d,
		linkInterval: GitHubPollInterval(),
		logFn:        NewStderrLogger("[github listener] "),
	}
}

// Start schedules the PR-linker tick. It runs only when GitHub ingress is not
// off AND an App is connected+installed (the tick uses the App's installation
// token to resolve cross-references); otherwise it is a no-op and the webhook
// receiver alone handles live events.
func (l *GitHubListener) Start() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return nil
	}
	if GitHubTransport() == GitHubTransportOff {
		l.logFn("github ingress off")
		return nil
	}
	if _, ok := gitHubAppCredentials(); !ok {
		l.logFn("no GitHub App connected/installed; webhook receiver idle, PR-linker tick not scheduled")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.done = make(chan struct{})
	l.running = true
	go func() {
		defer close(l.done)
		l.linkLoop(ctx)
	}()
	return nil
}

func (l *GitHubListener) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return
	}
	cancel := l.cancel
	done := l.done
	l.running = false
	l.cancel = nil
	l.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			l.logFn("stop: timeout waiting for linker goroutine to exit")
		}
	}
}

// Dispatch routes one already-normalized webhook event through the dispatcher
// (task creation, inbox append, attention routing, reopen/mark-done) with no
// GitHub API call. Unchanged from before.
func (l *GitHubListener) Dispatch(ctx context.Context, ev GitHubEvent) error {
	if l == nil || l.dispatcher == nil {
		return nil
	}
	return l.dispatcher.Dispatch(ctx, ev)
}

func (l *GitHubListener) linkLoop(ctx context.Context) {
	l.linkPass(ctx)
	interval := l.linkInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.linkPass(ctx)
		}
	}
}

func (l *GitHubListener) linkPass(ctx context.Context) {
	if l.dispatcher == nil {
		return
	}
	db := l.dispatcher.DB
	linkInProgressTaskPRs(ctx, db)
	linkInProgressIssuePRs(ctx, db, GitHubSelfLogins())
}
