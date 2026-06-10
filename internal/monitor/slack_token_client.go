package monitor

import (
	"strings"
	"sync"

	"github.com/slack-go/slack"
)

// lazySlackClient resolves a Slack token through tokenFn on every use and
// rebuilds the underlying *slack.Client only when the resolved token actually
// changes.
//
// Read-side Slack clients used to bake slack.New(token) at construction, so a
// token rotated while the server runs — operator re-auth, or the Recreate-app
// action that revokes the prior app's tokens — left long-lived holders (the
// steering context fetcher, backfill, the title/name resolver) pinned to a
// now-revoked token. That surfaced as token_revoked on every thread read, which
// in turn starved the Attention Router's classifier of real context and let it
// confabulate matches. Resolving per use lets a rotated token (written into the
// process env by the OAuth callback via os.Setenv) take effect with no restart.
//
// Safe for concurrent use; the cached client is reused until the token string
// differs from the one it was built with.
type lazySlackClient struct {
	tokenFn func() string

	mu    sync.Mutex
	token string
	api   *slack.Client
}

func newLazySlackClient(tokenFn func() string) *lazySlackClient {
	return &lazySlackClient{tokenFn: tokenFn}
}

// callSlackTokenFn resolves a token provider to its current trimmed value, or
// "" when the provider is nil/empty. Used by read clients that pass the token
// to a request function rather than holding a *slack.Client.
func callSlackTokenFn(tokenFn func() string) string {
	if tokenFn == nil {
		return ""
	}
	return strings.TrimSpace(tokenFn())
}

// client returns a *slack.Client bound to the currently-resolved token, or nil
// when no token is configured right now. A nil return is the caller's signal to
// surface ErrNoToken rather than make a doomed API call.
func (l *lazySlackClient) client() *slack.Client {
	if l == nil || l.tokenFn == nil {
		return nil
	}
	tok := strings.TrimSpace(l.tokenFn())
	if tok == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.api == nil || l.token != tok {
		l.token, l.api = tok, slack.New(tok)
	}
	return l.api
}
