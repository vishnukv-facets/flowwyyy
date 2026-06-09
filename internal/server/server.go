package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"flow/internal/monitor"
	"flow/internal/steering"
)

//go:embed all:static
var staticFS embed.FS

func init() {
	// Register MIME types Go's default table omits on some platforms, so
	// handleStatic and the RPC bridge (which both resolve via
	// mime.TypeByExtension) always serve embedded assets with the right type.
	// Harmless when an extension isn't currently emitted by the UI build.
	_ = mime.AddExtensionType(".wasm", "application/wasm")
	_ = mime.AddExtensionType(".mjs", "text/javascript")
}

func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	// Export persisted settings (config.json) into the process env before any
	// listener reads them, so UI-managed config is authoritative.
	s.applyConfigToEnv()
	// Capture operator config that was set via the environment but never saved,
	// so it survives a restart launched without those exports (otherwise e.g.
	// GitHub polling silently reverts to off).
	s.seedConfigFromEnv()
	// Hydrate GitHub App credentials (PEM, client + webhook secrets) from the OS
	// keyring into the process env. Runs after applyConfigToEnv so the keyring —
	// the authoritative at-rest store — wins over a stale config/shell value,
	// while an absent entry preserves the env fallback.
	loadGitHubSecretsFromKeyring()
	s.terminals = newTerminalHub(s)
	// Restore adhoc floating sessions whose tmux PTYs outlived a prior server
	// process, so the Ask Flow tray survives a flow-server restart.
	s.terminals.loadFloatingFromDisk()
	s.events = newEventHub()
	s.reconcile = newLivenessReconciler(s)
	s.transcripts = newTranscriptCache()
	s.caches = newUICaches()
	s.dbWatcher = newDBWatcher(s)
	s.respawn = newRespawnGate(respawnDebounceWindow)
	s.zrok = &zrokManager{}
	s.inboxMonitors = newInboxMonitorManager(inboxWakeTarget{server: s})
	s.monitorReconcile = newMonitorReconciler(s)
	// Resolves Slack user/channel IDs to display names for the Inbox UI.
	// Nil when no Slack token is configured; all uses are nil-safe.
	s.nameResolver = monitor.NewSlackNameResolver()
	// Resolves a real Slack permalink (chat.getPermalink) from channel+ts so the
	// "Open in Slack" link works for every item — including those captured before
	// the channel/ts/team_id columns existed (channel+ts are recoverable from
	// thread_key). Nil when no token; all uses are nil-safe.
	s.slackPermalinker = monitor.NewSlackPermalinker()
	// Slack Socket Mode listener: only constructed when a DB is available
	// (the dispatcher needs one). Start()/Stop() are no-ops when the env
	// isn't configured for Socket Mode, so wiring is safe to leave in
	// place at all times. The opener attaches new slack-reply tasks to
	// a server-managed PTY so the Claude session streams into the UI
	// instead of an iTerm tab.
	if cfg.DB != nil {
		dispatcher := monitor.NewDispatcher(cfg.DB, &slackTaskOpener{server: s})
		// Attach the steering cascade so untracked messages get triaged into the
		// Attention feed (surface-only in P1). Stage 0 inside the cascade is the
		// real scope gate, so handing it every untracked message is cheap.
		cascade := steering.NewCascade(cfg.DB, steering.WatchConfigFromEnv())
		// Live re-read on settings changes, overlaying the operator's durable
		// "perma drop" mutes (channel/sender/thread) from steering_mutes.
		cascade.ConfigFn = steering.WatchConfigFnWithMutes(cfg.DB)
		cascade.AutonomyFn = steering.AutonomyFnWithFeedback(cfg.DB, steering.AutonomyFromEnv) // live per-action auto-act policy + learned threshold nudges
		// De-ID feed text at ingest: clean Slack <@U…> mention markup to names
		// BEFORE it reaches the classifier/LLM and the trace, so summaries and
		// drafts never parrot raw IDs. nil resolver → no cleaner (identity).
		if s.nameResolver != nil {
			cascade.TextClean = s.nameResolver.CleanText
			cascade.ResolveUserName = s.nameResolver.UserName
		}
		cascade.FetchContext = steering.NewDefaultContextFetcher(cascade.TextClean, s.slackPermalinker)
		dispatcher.Steerer = cascade
		dispatcher.SteererOwnsRouting = steeringAutonomyRoutingEnabled
		s.cascade = cascade
		// Reuse a primed Haiku session across the cheap classifier stages (the
		// heavy framing + task index sent once at session creation, only the
		// per-message payload on each resume). No-op when
		// FLOW_STEERING_SESSION_REUSE=0.
		steering.EnableClassifierSessions()
		slackListener := monitor.NewSlackListener(dispatcher)
		slackListener.SetChangeNotifier(func(kind string) {
			s.publishUIChange(kind)
		})
		s.slackListener = slackListener
		ghDispatcher := monitor.NewGitHubDispatcher(cfg.DB, &slackTaskOpener{server: s})
		// Route every GitHub event through the SAME cascade so it surfaces in
		// the steering trace + attention feed. In surface-only mode this remains
		// additive; when autonomy is enabled, the dispatcher lets the steerer own
		// task routing instead of also running the legacy monitor pipeline.
		if s.cascade != nil {
			ghDispatcher.Steerer = s.cascade
			ghDispatcher.SteererOwnsRouting = steeringAutonomyRoutingEnabled
		}
		s.githubListener = monitor.NewGitHubListener(ghDispatcher)
	}
	return s
}

// registerAPIRoutes wires every /api/* data-plane route onto mux. It is
// shared by the public HTTP Handler and the in-process apiHandler the
// WebSocket-RPC bridge dispatches through, so the route table has exactly
// one source of truth.
func (s *Server) registerAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/ui-data.js", s.handleUIDataJS)
	mux.HandleFunc("/api/ui-data", s.handleUIDataJSON)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/events", s.handleUIEvents)
	mux.HandleFunc("/api/actions", s.handleAction)
	mux.HandleFunc("/api/hooks/agent", s.handleAgentHook)
	mux.HandleFunc("/api/overview", s.handleOverview)
	mux.HandleFunc("/api/work-events", s.handleWorkEvents)
	mux.HandleFunc("/api/fs/entries", s.handleFSEntries)
	mux.HandleFunc("/api/fs/mkdir", s.handleFSMkdir)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/", s.handleTaskRoute)
	mux.HandleFunc("/api/inbox", s.handleInbox)
	mux.HandleFunc("/api/inbox/conversation", s.handleInboxConversation)
	mux.HandleFunc("/api/inbox/notify", s.handleInboxNotify)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/projects/", s.handleProjectRoute)
	mux.HandleFunc("/api/playbooks", s.handlePlaybooks)
	mux.HandleFunc("/api/playbooks/", s.handlePlaybookRoute)
	mux.HandleFunc("/api/workdirs", s.handleWorkdirs)
	mux.HandleFunc("/api/tags", s.handleTags)
	mux.HandleFunc("/api/kb", s.handleKB)
	mux.HandleFunc("/api/kb/", s.handleKBFile)
	mux.HandleFunc("/api/memory/sources", s.handleMemorySources)
	mux.HandleFunc("/api/memory", s.handleMemoryWrite)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/ask-flow", s.handleAskFlow)
	mux.HandleFunc("/api/quote", s.handleQuote)
	// Slack OAuth callback. The install flow uses a short-lived localhost TLS
	// listener; this local route is only a same-process fallback and is not
	// registered on the public ingress mux.
	mux.HandleFunc(slackOAuthCallbackPath, s.handleSlackSetupOAuthCallbackMain)
	mux.HandleFunc("/api/github/webhook", s.handleGitHubWebhook)
	mux.HandleFunc("/api/github/webhook/status", s.handleGitHubWebhookStatus)
	mux.HandleFunc("/api/ingress/status", s.handleIngressStatus)
	mux.HandleFunc("/api/attention", s.handleAttention)
	mux.HandleFunc("/api/attention/trace", s.handleAttentionTrace)
	mux.HandleFunc("/api/attention/decision", s.handleAttentionDecision)
	mux.HandleFunc("/api/slack/channels", s.handleSlackChannels)
	mux.HandleFunc("/api/slack/setup/status", s.handleSlackSetupStatus)
	mux.HandleFunc("/api/slack/setup/create-app", s.handleSlackSetupCreateApp)
	mux.HandleFunc("/api/slack/setup/app-token", s.handleSlackSetupAppToken)
	mux.HandleFunc("/api/slack/setup/oauth/start", s.handleSlackSetupOAuthStart)
	mux.HandleFunc("/api/slack/setup/oauth/cancel", s.handleSlackSetupOAuthCancel)
	mux.HandleFunc("/api/github/auth/status", s.handleGitHubAuthStatus)
	mux.HandleFunc("/api/github/auth/switch", s.handleGitHubAuthSwitch)
	mux.HandleFunc("/api/github/setup/status", s.handleGitHubSetupStatus)
	mux.HandleFunc("/api/github/setup/create-app", s.handleGitHubSetupCreateApp)
	// The manifest conversion callback is also registered on the public ingress
	// mux (ingressMux) — GitHub redirects the operator's browser to it on the
	// public URL. This local route is the same-process fallback.
	mux.HandleFunc(githubSetupCallbackPath, s.handleGitHubSetupCallback)
	mux.HandleFunc("/api/github/setup/backfill", s.handleGitHubSetupBackfill)
	mux.HandleFunc("/api/github/setup/disconnect", s.handleGitHubSetupDisconnect)
	mux.HandleFunc("/api/github/setup/orgs", s.handleGitHubSetupOrgs)
	mux.HandleFunc("/api/github/setup/installations", s.handleGitHubSetupInstallations)
}

// apiHandler lazily builds and caches the data-plane mux used by the
// WebSocket-RPC bridge. It carries only /api/* routes — never the static
// or websocket routes — so RPC frames can't reach the upgrade handlers.
func (s *Server) apiHandler() http.Handler {
	s.apiOnce.Do(func() {
		mux := http.NewServeMux()
		s.registerAPIRoutes(mux)
		s.apiMux = mux
	})
	return s.apiMux
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	mux.HandleFunc("/ws/terminal", s.handleTerminalWebSocket)
	mux.HandleFunc("/ws/floating-terminal", s.handleFloatingTerminalWebSocket)
	mux.HandleFunc("/ws/events", s.handleEventWebSocket)
	mux.HandleFunc("/ws/rpc", s.handleRPCWebSocket)
	mux.HandleFunc("/ws", s.handleWebSocketPlaceholder)
	mux.HandleFunc("/", s.handleStatic)
	return mux
}

func (s *Server) ListenAndServe(addr string) int {
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Start the optional zrok public share when auto-start is enabled. The
	// share serves only the restricted ingress mux (connector callbacks), never
	// the full Mission Control handler.
	if s.zrok != nil {
		s.zrok.handler = s.ingressMux()
		// Provision + persist the webhook secret and reserved share name on
		// first enable so the share can start and its URL stays stable across
		// restarts (no-op once both are set — see ensureZrokIngressCredentials).
		s.ensureZrokIngressCredentials()
		s.zrok.start()
		defer s.zrok.stop()
	}
	// Start the liveness reconciler so dead Claude/Codex sessions get
	// detected even when their Stop/SessionEnd hook never fires. Stops
	// cleanly on shutdown via the deferred s.reconcile.stop().
	if s.reconcile != nil {
		s.reconcile.start()
		defer s.reconcile.stop()
	}
	// Start the persistent-monitor reconciler: restore background monitors for
	// Slack/GitHub/branch-linked tasks on boot, recreate any that die, and
	// tear down monitors for finished tasks.
	if s.monitorReconcile != nil {
		s.monitorReconcile.start()
		defer s.monitorReconcile.stop()
	}
	// Watch SQLite data_version so writes from external processes
	// (notably the flow CLI) trigger an SSE refresh within ~1s without
	// each CLI command needing to notify us out-of-band.
	if s.dbWatcher != nil {
		s.dbWatcher.start()
		defer s.dbWatcher.stopWatching()
	}
	// Start the Slack Socket Mode listener when configured. The listener
	// is responsible for receiving reaction_added + message events and
	// routing them into flow tasks via monitor.Dispatcher. Start() is a
	// no-op when env config is incomplete; Stop() is safe to call
	// unconditionally on shutdown.
	if s.slackListener != nil {
		if err := s.slackListener.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: slack listener start: %v\n", err)
		}
		defer s.slackListener.Stop()
	}
	// Durable Slack backfill. The live Socket Mode listener above loses any
	// events that arrive while its socket is down — every restart, any network
	// blip — because Socket Mode never replays them. This goroutine reconciles
	// each monitored thread against the Slack Web API on boot and on an
	// interval, appending missed replies to inbox.jsonl so the Inbox and the
	// same-session monitor catch up regardless of socket gaps. It runs off the
	// Web API (independent of the socket) and is a no-op when no Slack token is
	// configured.
	if s.cfg.DB != nil {
		if rc := monitor.NewSlackRepliesClient(); rc != nil {
			backfill := monitor.NewSlackBackfill(s.cfg.DB, rc, 0)
			if s.cascade != nil {
				backfill.Observer = s.cascade
				backfill.SteererOwnsRouting = steeringAutonomyRoutingEnabled
			}
			// DM channels the agent registered (slack-dm: tags) are reconciled
			// via conversations.history on the user token — the bot can't read
			// the operator's DMs. No-op when no user token is configured.
			if dh := monitor.NewSlackUserRepliesClient(); dh != nil {
				backfill.SetDMRepliesClient(dh)
			}
			backfill.SetLogger(func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, format+"\n", args...)
			})
			bfCtx, bfCancel := context.WithCancel(context.Background())
			defer bfCancel()
			go backfill.Run(bfCtx)
		}
	}
	// Steerer backfill. The reaction backfill above only reconciles already-
	// tracked threads; the steerer's continuous path (untracked firehose) has
	// no catch-up of its own, so anything that arrives in a watched channel or
	// DM while the socket is down — including before the steerer ever ran — is
	// lost. This sweeps watched channels (bot token) + DMs (user token) via
	// conversations.history since a per-channel watermark and replays them
	// through the SAME cascade (ObserveBatch, origin=backfill). No-op when no
	// Slack history client is configured or FLOW_STEERING_BACKFILL=0.
	if s.cfg.DB != nil && s.cascade != nil && steeringBackfillEnabled() {
		ch := monitor.NewSlackHistoryClient()
		dm := monitor.NewSlackUserHistoryClient()
		ims := monitor.NewSlackUserIMLister()
		if ch != nil || (dm != nil && ims != nil) {
			sbf := steering.NewSteeringBackfill(s.cfg.DB, s.cascade.ObserveBatch, ch, dm, ims, steering.WatchConfigFromEnv, 0, 0, 0)
			sbf.SetLogger(func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, "[steering backfill] "+format+"\n", args...)
			})
			sbfCtx, sbfCancel := context.WithCancel(context.Background())
			defer sbfCancel()
			go sbf.Run(sbfCtx)
		}
	}
	// One-shot tag backfill: re-link steerer-created tasks (make_task / send_reply)
	// to their source thread. Tasks spawned before source-thread tagging carry no
	// slack-thread:/gh- linkage, so replies on those threads — in-thread or
	// forwarded into a DM — can't route home. This derives the linkage purely from
	// stored feed rows (no network), so existing data starts routing too.
	if s.cfg.DB != nil {
		go func() {
			n, err := steering.BackfillFeedTaskThreadTags(s.cfg.DB, func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, "[thread-tag backfill] "+format+"\n", args...)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "[thread-tag backfill] %v\n", err)
			} else if n > 0 {
				fmt.Fprintf(os.Stderr, "[thread-tag backfill] linked %d existing task(s) to their source thread\n", n)
			}
			// Then re-check open make_task cards: any whose thread now resolves to
			// an existing (incl. archived) task flips to forward instead of nagging
			// to create a duplicate.
			m, rerr := steering.ReconcileOpenFeedMatches(s.cfg.DB, func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, "[feed reconcile] "+format+"\n", args...)
			})
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "[feed reconcile] %v\n", rerr)
			} else if m > 0 {
				fmt.Fprintf(os.Stderr, "[feed reconcile] flipped %d open card(s) to forward an existing task\n", m)
			}
			// Clear any 'drop'-verdict cards that an earlier bug surfaced — they're
			// cascade-classified noise and shouldn't sit in the active feed.
			d, derr := steering.DismissSurfacedDropCards(s.cfg.DB, func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, "[feed reconcile] "+format+"\n", args...)
			})
			if derr != nil {
				fmt.Fprintf(os.Stderr, "[feed reconcile] %v\n", derr)
			} else if d > 0 {
				fmt.Fprintf(os.Stderr, "[feed reconcile] dismissed %d stale drop-verdict card(s)\n", d)
			}
		}()
	}
	// Start the GitHub polling listener when explicitly enabled. Like
	// Slack, Start() is a no-op when env config is incomplete.
	if s.githubListener != nil {
		if err := s.githubListener.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: github listener start: %v\n", err)
		}
		defer s.githubListener.Stop()
	}
	// One-shot async backfill of tasks.session_path for pre-existing
	// Codex sessions captured before the column was added. Skipped if
	// the DB is unset (tests, healthchecks). Errors are swallowed
	// inside; we never want a slow ~/.codex to block startup.
	go s.backfillSessionPaths()
	// Warm the search index once at boot so the first ⌘K query hits a fresh
	// FTS index instead of triggering an inline rebuild. Runs in the
	// background (the rebuild walks the whole flow root) and routes the timer
	// through syncSearchThrottled so it shares the in-flight guard.
	go s.warmSearchIndex()
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error: serve: %v\n", err)
		return 1
	case <-sigCh:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "error: shutdown: %v\n", err)
			return 1
		}
		return 0
	}
}

// steeringBackfillEnabled reports whether the steerer catch-up sweep should
// run. Defaults on; set FLOW_STEERING_BACKFILL=0 to disable while leaving the
// rest of the steerer wired.
func steeringBackfillEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_STEERING_BACKFILL"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func steeringAutonomyRoutingEnabled() bool {
	pol := steering.AutonomyFromEnv()
	for _, action := range []steering.Action{steering.ActionMakeTask, steering.ActionForward} {
		if p, ok := pol[action]; ok && p.Enabled {
			return true
		}
	}
	return false
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
	if path == "." || path == "" {
		path = "index.html"
	}
	data, err := staticFS.ReadFile("static/" + path)
	if err != nil {
		data, err = staticFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, "static assets unavailable", http.StatusInternalServerError)
			return
		}
		path = "index.html"
	}
	if ctype := mime.TypeByExtension(filepath.Ext(path)); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

func embeddedStaticExists(name string) bool {
	_, err := fs.Stat(staticFS, "static/"+name)
	return err == nil
}
