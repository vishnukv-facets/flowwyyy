package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
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
	// Mint the data-plane session token before anything can serve. Gates every
	// WS handshake and state-changing /api/* route (audit P0-1).
	s.sessionToken = mintSessionToken()
	// Export persisted settings (config.json) into the process env before any
	// listener reads them, so UI-managed config is authoritative.
	s.applyConfigToEnv()
	// Capture operator config that was set via the environment but never saved,
	// so it survives a restart launched without those exports (otherwise e.g.
	// GitHub polling silently reverts to off).
	s.seedConfigFromEnv()
	// Move any keyring-routed secret still living in config.json plaintext (an
	// install created before secrets were keyring-backed) into the OS keyring and
	// strip the plaintext copy, before the hydration below reads it (audit P2-2).
	s.migrateConfigSecretsToKeyring()
	// Hydrate GitHub App credentials (PEM, client + webhook secrets) from the OS
	// keyring into the process env. Runs after applyConfigToEnv so the keyring —
	// the authoritative at-rest store — wins over a stale config/shell value,
	// while an absent entry preserves the env fallback.
	loadGitHubSecretsFromKeyring()
	// Same for the Slack bot token, operator user token, and OAuth client secret.
	loadSlackSecretsFromKeyring()
	// And the offsite backup token (personal GitHub PAT for the flow-backup repo).
	loadBackupSecretsFromKeyring()
	s.terminals = newTerminalHub(s)
	// Restore adhoc floating sessions whose tmux PTYs outlived a prior server
	// process, so the Ask Flow tray survives a flow-server restart.
	s.terminals.loadFloatingFromDisk()
	s.events = newEventHub()
	// Memoize external-channel verdicts for the outbound send gate; external-ness
	// rarely changes, so a 10-minute TTL keeps conversations.info off the send path.
	s.slackExtCache = newTTLCache[string, slackExtVerdict](10 * time.Minute)
	s.steeringRuns = newSteeringRunStore()
	s.reconcile = newLivenessReconciler(s)
	s.kbDistiller = newKBDistiller(s)
	s.steererCompact = newSteererCompactWorker(s)
	s.kbDreamer = newKBDreamer(s)
	s.kbWatcher = newKBWatcher(s)
	s.transcripts = newTranscriptCache()
	s.caches = newUICaches()
	s.dbWatcher = newDBWatcher(s)
	s.respawn = newRespawnGate(respawnDebounceWindow)
	s.zrok = &zrokManager{}
	s.pairing = newPairingStore()
	s.remoteLimiter = newRateLimiter(10, time.Minute)
	s.inboxMonitors = newInboxMonitorManager(inboxWakeTarget{server: s})
	s.monitorReconcile = newMonitorReconciler(s)
	s.playbookSched = newPlaybookScheduler(s)
	s.ownerSched = newOwnerScheduler(s)
	s.backupSched = newBackupScheduler(s)
	// Resolves Slack user/channel IDs to display names for the Inbox UI.
	// Nil when no Slack token is configured; all uses are nil-safe.
	s.nameResolver = monitor.NewSlackNameResolver()
	// Fall back to the operator's user token for channels the bot can't see (a
	// private channel it was never invited to), so the live/trace/feed views show
	// "#name" instead of a raw C… id for those too.
	s.nameResolver.SetFallbackClient(monitor.NewSlackTitleUserClient())
	// Resolves a real Slack permalink (chat.getPermalink) from channel+ts so the
	// "Open in Slack" link works for every item — including those captured before
	// the channel/ts/team_id columns existed (channel+ts are recoverable from
	// thread_key). Nil when no token; all uses are nil-safe.
	s.slackPermalinker = monitor.NewSlackPermalinker()
	monitor.SetSlackImageFileSaver(s.saveSteererSlackImageAttachment, maxTerminalAttachmentUploadBytes)
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
		// Gate auto-acts on the CALIBRATED confidence (raw model score → empirical
		// P(operator agrees), learned from attention_feedback). Re-loaded per surfaced
		// verdict so it tracks new feedback; an error → nil → raw fallback.
		cascade.CalibratorFn = func() *steering.ConfidenceCalibrator {
			cal, err := steering.LoadConfidenceCalibrator(cfg.DB)
			if err != nil {
				return nil
			}
			return cal
		}
		// De-ID feed text at ingest: clean Slack <@U…> mention markup to names
		// BEFORE it reaches the classifier/LLM and the trace, so summaries and
		// drafts never parrot raw IDs. nil resolver → no cleaner (identity).
		if s.nameResolver != nil {
			cascade.TextClean = s.nameResolver.CleanText
			cascade.ResolveUserName = s.nameResolver.UserName
		}
		cascade.FetchContext = steering.NewDefaultContextFetcher(cascade.TextClean, s.slackPermalinker)
		// Operator-reply learning distills durable facts out of hand-written replies
		// into the KB. Empty FlowRoot ⇒ KBDir stays empty ⇒ capture is skipped.
		if cfg.FlowRoot != "" {
			cascade.KBDir = filepath.Join(cfg.FlowRoot, "kb")
		}
		// Stream live stage progress to Mission Control's inbox (CI-style view).
		cascade.Progress = s.publishSteeringStage
		dispatcher.Steerer = cascade
		// The per-channel session model is the master switch: ON ⇒ the steerer owns
		// routing (triage → attention feed / per-channel sessions); OFF ⇒ the steerer
		// stands down entirely and events route the OLD way (Slack reaction-trigger →
		// task inbox.jsonl, GitHub webhook → legacy task pipeline). No attention/triage
		// when the session model is off.
		dispatcher.SteererOwnsRouting = steering.SteererSessionsEnabled
		// Slack AFK command channel: authorized operator DMs to the bot route into
		// a durable chat agent session (the overview-chat model) instead of the
		// task/steering pipeline. *Server satisfies monitor.ChatCommandSink. Gated
		// OFF by default inside the dispatcher (CommandChannelEnabled).
		dispatcher.ChatSink = s
		s.cascade = cascade
		// Per-channel steerer session model (GAP-1, behind FLOW_STEERING_SESSIONS,
		// default off). *Server implements steering.SteererSessionSink; the cold
		// DeepTriageIncremental path stays the live fallback on any session error.
		cascade.SessionSink = s
		dispatcher.SteererSessionsEnabled = steering.SteererSessionsEnabled
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
			ghDispatcher.SteererOwnsRouting = steering.SteererSessionsEnabled
			ghDispatcher.SessionsEnabled = steering.SteererSessionsEnabled
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
	mux.HandleFunc("/api/analytics", s.handleAnalytics)
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
	mux.HandleFunc("/api/chats", s.handleChats)
	mux.HandleFunc("/api/brain/graph", s.handleBrainGraph)
	mux.HandleFunc("/api/brain/graph/actions", s.handleBrainGraphAction)
	mux.HandleFunc("/api/brain/graph/node/", s.handleBrainGraphNodeDetail)
	mux.HandleFunc("/api/owners", s.handleOwners)
	mux.HandleFunc("/api/owners/", s.handleOwnerRoute)
	mux.HandleFunc("/api/workdirs", s.handleWorkdirs)
	mux.HandleFunc("/api/tags", s.handleTags)
	mux.HandleFunc("/api/persona", s.handlePersona)
	mux.HandleFunc("/api/kb", s.handleKB)
	mux.HandleFunc("/api/kb/dream", s.handleKBDream) // must precede the /api/kb/ subtree
	mux.HandleFunc("/api/kb/prune", s.handleKBPrune) // exact match; precedes the /api/kb/ subtree
	mux.HandleFunc("/api/kb/", s.handleKBFile)
	mux.HandleFunc("/api/backup/status", s.handleBackupStatus)
	mux.HandleFunc("/api/backup/log", s.handleBackupLog)
	mux.HandleFunc("/api/backup/show", s.handleBackupShow)
	mux.HandleFunc("/api/backup/restore", s.handleBackupRestore)
	mux.HandleFunc("/api/backup/now", s.handleBackupNow)
	mux.HandleFunc("/api/backup/token", s.handleBackupToken)
	mux.HandleFunc("/api/memory/sources", s.handleMemorySources)
	mux.HandleFunc("/api/memory", s.handleMemoryWrite)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/ask-flow", s.handleAskFlow)
	mux.HandleFunc("/api/quote", s.handleQuote)
	// Slack OAuth callback. The install flow uses a short-lived localhost TLS
	// listener (loopback mode); in public-ingress mode it also rides the ingress
	// mux (see ingressMux). This local route is the same-process path.
	mux.HandleFunc(slackOAuthCallbackPath, s.handleSlackSetupOAuthCallback)
	mux.HandleFunc("/api/github/webhook", s.handleGitHubWebhook)
	mux.HandleFunc("/api/github/webhook/status", s.handleGitHubWebhookStatus)
	mux.HandleFunc("/api/ingress/status", s.handleIngressStatus)
	mux.HandleFunc("/api/attention", s.handleAttention)
	mux.HandleFunc("/api/attention/trace", s.handleAttentionTrace)
	mux.HandleFunc("/api/attention/decision", s.handleAttentionDecision)
	mux.HandleFunc("/api/steering/runs", s.handleSteeringRuns)
	mux.HandleFunc("/api/slack/send", s.handleSlackSend)
	mux.HandleFunc("/api/slack/react", s.handleSlackReact)
	mux.HandleFunc("/api/slack/pending", s.handleSlackPendingList)
	mux.HandleFunc("/api/slack/pending/decide", s.handleSlackPendingDecide)
	mux.HandleFunc("/api/slack/channels", s.handleSlackChannels)
	mux.HandleFunc("/api/slack/setup/status", s.handleSlackSetupStatus)
	mux.HandleFunc("/api/slack/setup/create-app", s.handleSlackSetupCreateApp)
	mux.HandleFunc("/api/slack/setup/app-token", s.handleSlackSetupAppToken)
	mux.HandleFunc("/api/slack/setup/oauth/start", s.handleSlackSetupOAuthStart)
	mux.HandleFunc("/api/slack/setup/oauth/cancel", s.handleSlackSetupOAuthCancel)
	mux.HandleFunc("/api/slack/setup/reset", s.handleSlackSetupReset)
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
	mux.HandleFunc("/api/remote/pair-code", s.handleRemotePairCode)
	mux.HandleFunc("/api/remote/devices", s.handleRemoteDevices)
	mux.HandleFunc("/api/remote/devices/revoke", s.handleRemoteDeviceRevoke)
	mux.HandleFunc("/api/remote/status", s.handleRemoteStatus)
	mux.HandleFunc("/api/remote/enable", s.handleRemoteEnable)
	mux.HandleFunc("/api/remote/disable", s.handleRemoteDisable)
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
	// Gate the direct-HTTP /api/* data plane (cross-origin reject + session
	// token on state-changing routes). The WS handlers and apiHandler() do
	// their own auth, so this only affects direct HTTP. See session_token.go.
	return s.dataPlaneAuth(mux)
}

func (s *Server) ListenAndServe(addr string) int {
	// Persist the minted token (0600) so trusted local CLIs (flow wait, slack
	// send, attention sent) can authenticate to the data plane. Written before
	// serving so the file matches the in-memory token by the time we accept
	// connections.
	s.writeSessionTokenFile()
	s.syncPowerAssertion()
	defer s.stopPowerAssertion()
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Start the optional zrok public share when auto-start is enabled. The
	// share serves the composite publicIngressHandler: GitHub webhook + OAuth
	// callbacks always, plus the device-token-gated remote app mux when remote
	// access is enabled. It never serves the full localhost Mission Control
	// handler (no shared session token, no unauthenticated operator routes).
	if s.zrok != nil {
		s.zrok.handler = s.publicIngressHandler()
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
	// Start the KB distiller: periodically capture durable knowledge from idle
	// live sessions (in-progress tasks + chats) into kb/*.md, mid-flight. Gated
	// off cleanly on shutdown via the deferred stop().
	if s.kbDistiller != nil {
		s.kbDistiller.start()
		defer s.kbDistiller.stop()
	}
	// Per-channel steerer session idle-sweep: tear down PTYs of steerer sessions
	// whose transcript has been quiet past the TTL (the chat row + session_id
	// survive; the next event resumes them). Only runs when FLOW_STEERING_SESSIONS
	// is on, so it is a no-op by default.
	if steering.SteererSessionsEnabled() {
		sweepCtx, sweepCancel := context.WithCancel(context.Background())
		defer sweepCancel()
		go s.runSteererIdleSweep(sweepCtx)
		if s.steererCompact != nil {
			s.steererCompact.start()
			defer s.steererCompact.stop()
		}
	}
	// Start the KB dreamer: periodic hygiene pass that flags stale KB entries
	// into each file's "Pending removal" section and auto-prunes ones left
	// flagged past the max age.
	if s.kbDreamer != nil {
		s.kbDreamer.start()
		defer s.kbDreamer.stop()
	}
	// Start the KB file watcher: pushes a live "kb" invalidation over SSE when
	// any kb/*.md changes (agent capture, dreamer prune, UI edit), so the
	// Knowledge screen updates without polling.
	if s.kbWatcher != nil {
		s.kbWatcher.start()
		defer s.kbWatcher.stop()
	}
	// Start the persistent-monitor reconciler: restore background monitors for
	// Slack/GitHub/branch-linked tasks on boot, recreate any that die, and
	// tear down monitors for finished tasks.
	if s.monitorReconcile != nil {
		s.monitorReconcile.start()
		defer s.monitorReconcile.stop()
	}
	// Drive scheduled playbook runs: each tick shells out to
	// `flow playbook tick-due`, which fires any due playbook as an autonomous
	// run. Stops cleanly on shutdown.
	if s.playbookSched != nil {
		s.playbookSched.start()
		defer s.playbookSched.stop()
	}
	// Owner twin: fires due owner ticks via `flow owner tick-due`, with a boot
	// tick so owners that came due while the server was down (or the laptop
	// asleep) catch up on startup.
	if s.ownerSched != nil {
		s.ownerSched.start()
		defer s.ownerSched.stop()
	}
	// Backup safety net: checkpoint curated markdown on boot (catches anything an
	// out-of-process agent or manual edit wrote while the server was down), then
	// run scheduled backups (checkpoint + db snapshot + offsite push) on cadence.
	// Boot backup runs ASYNC so it never blocks the server from listening (a
	// full checkpoint over a large ~/.flow can take a moment, and the offsite
	// push is network-bound). Best-effort.
	go func() {
		s.backupCheckpoint("ui serve boot")
		if err := s.maybeBackupPush(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "flow backup: boot push: %v\n", err)
		}
	}()
	if s.backupSched != nil {
		s.backupSched.start()
		defer s.backupSched.stop()
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
		// Channel-thread reconcile uses the bot token, falling back to the
		// operator's user token for origin channels the bot isn't a member of —
		// the same fix as the steerer sweep. Without it, a slack-reply task whose
		// origin channel the bot can't read (private channel, or never invited)
		// retries every interval forever (channel_not_found / not_in_channel) and
		// never recovers, even though the operator's token could read it.
		if rc := monitor.NewSlackChannelRepliesClient(); rc != nil {
			backfill := monitor.NewSlackBackfill(s.cfg.DB, rc, 0)
			if s.cascade != nil {
				backfill.Observer = s.cascade
				backfill.SteererOwnsRouting = steering.SteererSessionsEnabled
				backfill.SteererSessionsEnabled = steering.SteererSessionsEnabled
			}
			// DM channels the agent registered (slack-dm: tags) are reconciled
			// via conversations.history on the user token — the bot can't read
			// the operator's DMs. No-op when no user token is configured.
			if dh := monitor.NewSlackUserRepliesClient(); dh != nil {
				backfill.SetDMRepliesClient(dh)
			}
			backfill.SetLogger(monitor.NewStderrLogger(""))
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
		// Channel sweep uses the bot token, falling back to the operator's user
		// token for channels the bot isn't a member of — the operator is a member
		// of every channel worth watching, so the user token recovers them even
		// when the bot was never invited (the engg-infra case). DMs stay user-only.
		ch := monitor.NewSlackChannelHistoryClient()
		dm := monitor.NewSlackUserHistoryClient()
		ims := monitor.NewSlackUserIMLister()
		if ch != nil || (dm != nil && ims != nil) {
			sbf := steering.NewSteeringBackfill(s.cfg.DB, s.cascade.ObserveBatch, ch, dm, ims, steering.WatchConfigFromEnv, 0, 0, 0)
			// Follow active threads so replies that landed in a watched channel /
			// DM while the socket was down (e.g. the laptop slept) are recovered —
			// a top-level history sweep alone never returns thread replies. Channel
			// threads use the bot token (user-token fallback for non-member
			// channels); DM threads use the user token.
			if rc := monitor.NewSlackChannelRepliesClient(); rc != nil {
				sbf.SetRepliesClient(rc)
			}
			if drc := monitor.NewSlackUserRepliesClient(); drc != nil {
				sbf.SetDMRepliesClient(drc)
			}
			sbf.SetLogger(monitor.NewStderrLogger("[steering backfill] "))
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
			n, err := steering.BackfillFeedTaskThreadTags(s.cfg.DB, monitor.NewStderrLogger("[thread-tag backfill] "))
			if err != nil {
				fmt.Fprintf(os.Stderr, "[thread-tag backfill] %v\n", err)
			} else if n > 0 {
				fmt.Fprintf(os.Stderr, "[thread-tag backfill] linked %d existing task(s) to their source thread\n", n)
			}
			// Then re-check open make_task cards: any whose thread now resolves to
			// an existing (incl. archived) task flips to forward instead of nagging
			// to create a duplicate.
			m, rerr := steering.ReconcileOpenFeedMatches(s.cfg.DB, monitor.NewStderrLogger("[feed reconcile] "))
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "[feed reconcile] %v\n", rerr)
			} else if m > 0 {
				fmt.Fprintf(os.Stderr, "[feed reconcile] flipped %d open card(s) to forward an existing task\n", m)
			}
			// Clear any 'drop'-verdict cards that an earlier bug surfaced — they're
			// cascade-classified noise and shouldn't sit in the active feed.
			d, derr := steering.DismissSurfacedDropCards(s.cfg.DB, monitor.NewStderrLogger("[feed reconcile] "))
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
	// Resume delivery of wakes that were buffered (in the pending_wakes table)
	// when a session was blocked on the operator's input, in case the server
	// restarted while they were withheld. flushWakes withholds anything still
	// awaiting input and routes the rest to whatever session is live.
	if s.terminals != nil {
		go s.terminals.resumeBufferedWakes()
	}
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
	// Hand the same-origin SPA its session token (window.__FLOW_TOKEN__) so it
	// can authenticate its /ws/* sockets. Unreadable cross-origin; only the
	// served HTML document carries it.
	if path == "index.html" {
		data = injectSessionToken(data, s.sessionToken)
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// remoteForbiddenRPCPath reports whether an /api path is a localhost-only
// operator action that a remote (device-token) client must NOT reach over the
// /ws/rpc bridge. The remote surface can drive sessions but can never pair new
// devices, toggle remote access, or read/revoke the device list.
//
// The denylist is intentionally NARROW — only /api/remote/ device-management
// paths are blocked. Everything else over the remote /ws/rpc channel is
// operator-equivalent by design: this is a single-operator tool and the device
// token is the trust boundary. Widening the denylist should be a deliberate
// decision with a threat-model rationale, not a defensive reflex.
func remoteForbiddenRPCPath(path string) bool {
	return strings.HasPrefix(path, "/api/remote/")
}

// handleRemoteStatic serves the embedded PWA shell for the REMOTE surface. It is
// identical to handleStatic EXCEPT it never injects the shared session token —
// the phone authenticates with its own device token from localStorage. It marks
// the page remote so the client uses the device-token transport.
func (s *Server) handleRemoteStatic(w http.ResponseWriter, r *http.Request) {
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
	if path == "index.html" {
		data = injectRemoteFlag(data)
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// injectRemoteFlag inserts window.__FLOW_REMOTE__ = true before </head> so the
// PWA knows to authenticate with its stored device token, not __FLOW_TOKEN__.
func injectRemoteFlag(html []byte) []byte {
	tag := []byte("<script>window.__FLOW_REMOTE__=true;</script></head>")
	return []byte(strings.Replace(string(html), "</head>", string(tag), 1))
}

// remoteAppMux is the device-token-gated app surface served over zrok when
// remote access is enabled. Only /api/remote/pair is reachable without a device
// token (rate-limited; how a device gets its first token). All data flows over
// the device-gated /ws/rpc — no general /api/* is exposed on this mux.
func (s *Server) remoteAppMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/remote/pair", s.handleRemotePair)
	mux.Handle("/ws/terminal", s.remoteAuth(http.HandlerFunc(s.handleTerminalWebSocket)))
	mux.Handle("/ws/floating-terminal", s.remoteAuth(http.HandlerFunc(s.handleFloatingTerminalWebSocket)))
	mux.Handle("/ws/rpc", s.remoteAuth(http.HandlerFunc(s.handleRPCWebSocket)))
	mux.Handle("/ws/events", s.remoteAuth(http.HandlerFunc(s.handleEventWebSocket)))
	mux.HandleFunc("/", s.handleRemoteStatic)
	return mux
}

// publicIngressHandler is what the zrok share serves. The GitHub webhook + OAuth
// mux is always served unchanged; the remote app is served only when remote
// access is enabled, otherwise app paths 404.
func (s *Server) publicIngressHandler() http.Handler {
	ingress := s.ingressMux()
	app := s.remoteAppMux()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/github/webhook", githubSetupCallbackPath, slackOAuthCallbackPath:
			ingress.ServeHTTP(w, r)
			return
		}
		if !remoteAccessEnabled() {
			http.NotFound(w, r)
			return
		}
		app.ServeHTTP(w, r)
	})
}
