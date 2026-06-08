package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDetectGitHubIntegrationSerializesConcurrentRefresh(t *testing.T) {
	ghAuthMu.Lock()
	origLookPath := ghLookPath
	origAuthStatus := ghAuthStatus
	origCached := ghAuthCached
	origExpiry := ghAuthExpiry
	ghAuthCached = uiToolCapability{}
	ghAuthExpiry = time.Time{}
	ghAuthMu.Unlock()
	t.Cleanup(func() {
		ghAuthMu.Lock()
		ghLookPath = origLookPath
		ghAuthStatus = origAuthStatus
		ghAuthCached = origCached
		ghAuthExpiry = origExpiry
		ghAuthMu.Unlock()
	})

	ghLookPath = func(bin string) (string, error) {
		if bin != "gh" {
			return "", errors.New("unexpected binary")
		}
		return "/test/bin/gh", nil
	}
	var calls int32
	ghAuthStatus = func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		return nil
	}

	var wg sync.WaitGroup
	results := make(chan uiToolCapability, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- detectGitHubIntegration()
		}()
	}
	wg.Wait()
	close(results)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("gh auth status calls = %d, want 1", got)
	}
	for c := range results {
		if !c.Available || c.Status != "connected" || c.Path != "/test/bin/gh" {
			t.Fatalf("capability = %+v, want connected cached GitHub capability", c)
		}
	}
}
