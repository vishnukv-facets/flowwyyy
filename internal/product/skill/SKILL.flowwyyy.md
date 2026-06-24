

---

## Product extensions (flowwyyy)

Attention
  flow attention list [--status new|acted|dismissed|snoozed|all]
                     show Attention Router cards awaiting operator review
  flow attention act <id> <make-task|forward|confirm-handoff|dismiss>
                     perform a simple operator-approved feed action
  flow attention sent <id> [--close-floating <floating-id>]
                     mark an approved send-reply card sent after confirmed post
  flow attention trace [--since 24h] [--disposition dropped|surfaced|error|all] [--limit 50]
                     inspect the steering decision funnel and trace rows
  flow attention feedback [--group source|channel|author|thread-type|suggested-action|confidence-band]
                     report Attention feedback approval/dismiss rates by one dimension
  flow attention calibration
                     per (action × raw confidence band): the observed operator-agreement
                     rate (calibrated confidence) vs the raw band the model emitted
  flow attention handoff accept <correlation-id> --reason "<why>"
                     accept an Attention confirmed handoff from this task's inbox
  flow attention handoff decline <correlation-id> --reason "<why>"
                     decline an Attention confirmed handoff and keep the feed card open

### 4.9b Respond to Attention confirmed handoffs

**When to use:** If this task's inbox contains a "Confirmed handoff request from the attention router" with a `Correlation ID`, the steerer is asking this task to verify whether the source thread belongs here before the card is marked handled.

**Recipe:**

1. Read the handoff context block, then compare it against this task's brief,
   updates, and current transcript context. Do not accept just because the
   steerer guessed this task; the point of confirmed handoff is that the owning
   task has better context than the steerer.
2. If it belongs here, run:
   ```
   flow attention handoff accept <correlation-id> --reason "<why this belongs here>"
   ```
   Accepting marks the Attention card acted and links it to this task. The
   context block already landed in this inbox, so continue from it normally.
3. If it does not belong here, run:
   ```
   flow attention handoff decline <correlation-id> --reason "<why this should route elsewhere>"
   ```
   Declining keeps the Attention card open so the steerer/operator can escalate,
   re-triage, make a new task, or choose another candidate. If you know the
   better owner, name it in the reason.
4. If the context is insufficient, decline with that reason instead of staying
   silent. Pending handoffs time out explicitly and do not mark the card acted.

**Anti-patterns:**

- Do not run ordinary `flow tell` as a substitute for accepting; it bypasses the
  correlation id and leaves the feed card unresolved.
- Do not accept without a reason. The reason is the audit trail the operator
  sees when reviewing why the route was confirmed.
- Do not keep a handoff pending while you investigate unrelated work; accept or
  decline once you have enough task-local evidence.

## 10a. Same-session inbox monitor

Flow routes monitored Slack, GitHub, or future source events into the
active task's `inbox.jsonl`. When a task terminal is live, Flow also runs
a task-local monitor that wakes the same Flow-owned terminal session by
sending a short prompt into the existing Claude or Codex session. The
monitor never performs the work itself and never starts a separate
background solver for the task.

Provider capability note: Flow does not rely on host-native background
monitors. Claude Code may offer native background-session commands such
as `claude agents`, `/bg`, or `--bg`, but Flow still routes
Slack/GitHub/future-source events through `inbox.jsonl` and the
Flow-owned terminal wake path. Codex tasks use that same terminal wake path; do not assume a Codex-native `/bg`, scheduler, or app-server/remote-control integration unless Flow adds a gated backend for it.

When you are woken by this monitor:

1. Read the newest `inbox.jsonl` entries for the current task.
2. Inspect the source link, Slack thread, GitHub PR, or other referenced
   context in the entry.
3. Continue the fix or response in this same agent session.
4. Save normal task updates for durable decisions or outcomes.

For GitHub PR work, make sure the task carries a
`gh-pr:<owner>/<repo>#<number>` tag. `flow done` tries to discover the
current branch PR automatically, and you can add the tag manually with
`flow update task <slug> --tag gh-pr:<owner>/<repo>#<number>`.

A task spawned from a GitHub **issue** keeps its `gh-issue:` tag AND
auto-gains a `gh-pr:` tag for each open PR you open to resolve it, as long
as the PR references the issue (e.g. `Fixes #<number>`) and is authored by
one of the configured self logins — the monitor links them via the issue's
GitHub cross-references each poll. This works even when the PR is opened
from a sub-branch that differs from the task's worktree branch (the common
"split one issue into several PRs" case), which the branch-based linker
alone cannot catch. One issue task can therefore carry several `gh-pr:`
tags. If a PR still isn't linked (e.g. it doesn't reference the issue), add
the tag manually with the command above.

## 10b. Slack-reply tasks (reaction-trigger pipeline)

`flow ui serve` hosts a Slack Socket Mode listener that watches every
conversation the user can see. When the user adds a designated reaction
(`:claude:` by default — configurable via `FLOW_SLACK_TRIGGER_EMOJI`) to
any message, the listener creates a flow task tagged `slack-reply` and
`slack-thread:<channel>:<thread_ts>`, then opens an iTerm tab on it.
Subsequent messages and reactions in the same thread route into the
same task's `inbox.jsonl`, so this conversation (the spawned Claude
session) is the single hub for everything that happens in the Slack
thread.

If your task carries the `slack-reply` tag (`flow show task` lists tags
under the `tags:` line), follow this bootstrap:

1. **Read your brief.** It's a snapshot of what triggered you —
   channel, thread_ts, item author, the reactor, and the conventions
   for replying. Treat its "Slack context" block as authoritative;
   don't re-derive thread_ts from the inbox.
2. **Read the `## Operator identity` block in your brief.** It lists
   the Slack user IDs that belong to the operator (the human running
   this flow installation). Hold these IDs in working memory before
   processing the inbox — every classification decision below depends
   on them. If the block is empty (operator hadn't configured
   `FLOW_SLACK_SELF_USER_IDS` when this task was spawned), ask the
   operator IN THIS SESSION for their Slack user ID before posting
   any reply; you cannot reliably tell their messages from external
   participants' otherwise. Do not invoke `slack.auth.test` to
   "discover" their ID — the User Token auth ID is the operator by
   construction, but the operator may have multiple workspace IDs and
   the env var is the source of truth.
3. **Catch up on the inbox.** Read every line of
   `~/.flow/tasks/<your-slug>/inbox.jsonl` in order. Each line is a JSON
   object `{enqueued_at, event}` where `event` is an `InboundEvent`
   (Kind = `message` | `app_mention` | `reaction_added`; full schema in
   `internal/monitor/inbound_event.go`). The events arrived while this
   session was closed — process them before posting fresh replies.

   **Classify each event by `event.user_id`:**
   - If `user_id` matches an operator ID from step 2: this is a
     **coordination signal** from the human you work with. Read it,
     let it adjust your plan, but do NOT post a Slack reply at the
     operator and do NOT treat it as an external follow-up that
     needs investigation. The operator's Slack messages in the
     thread are often context-setting ("ignore the last message",
     "this is for X"), not instructions to action.
   - If `user_id` is anyone else: external participant — the normal
     reply rules apply.
   - If `user_id` is empty (rare; bot/system events): treat as
     external for safety.
4. **Use the same-session monitor.** Flow wakes the same Flow-owned
   terminal session when new actionable Slack events arrive. If you are
   diagnosing monitor behavior manually, the equivalent file tail is
   `tail -F ~/.flow/tasks/<your-slug>/inbox.jsonl`. Apply the same
   operator-vs-external classification (step 3) to every event the
   live tail surfaces — the classification rule is not just a
   catch-up-time concern.
5. **Pull richer Slack context if needed.** The inbox event payload is
   compact. To see the full thread (older messages, files, deep links),
   use the Slack MCP tools — primarily
   `mcp__claude_ai_Slack__slack_read_thread` against the channel +
   thread_ts in your brief.
6. **Reply.** Post into the originating thread (`channel` + `thread_ts`
   from your brief). Posts go as the user (User Token), not a bot, so
   write in their voice and avoid claims you can't back up. For Slack
   channel/thread replies, use Flow's Slack sender rather than the direct
   Slack MCP send tool: write the final text to a temp file, then run
   `flow slack send --channel <channel> --thread-ts <thread_ts> --as user --text-file <path>`.
   This posts as the operator through the user token and is the Slack Connect-safe
   path when direct MCP sends are restricted. If it fails with `missing_scope`,
   the Slack app/user token needs reinstall with `chat:write`; do not mark sent.

   **NEVER sign the message. Do not add a manual `Sent using ...` footer —
   or ANY attribution line.** Your message body MUST end with your actual
   last sentence of content. Do not append (or any variant of): a
   `Sent using @…` line, "Sent via Codex", "Sent via ChatGPT", "— Codex",
   "Generated by …", or any "Sent / Posted / Generated using / via / by …"
   signature. This applies to EVERY reply — threads and DMs, first message
   and every follow-up.

   Why: Slack/ChatGPT app attribution is added automatically OUTSIDE the
   message body by the Slack client/tooling. If you also write one inside
   the body, the reply ends with TWO signatures in the thread, which looks
   broken. The platform handles attribution — you never do.

   Save a progress note after each meaningful exchange so the thread's
   history is captured in flow even if the inbox file rotates.
7. **Acknowledge with a 👍 reaction when a reply isn't warranted.** When an
   ingested message just needs acknowledgement — a closing, an FYI, or a short
   "ok / thanks / got it / will let you know / sounds good" — react to THAT
   message with a thumbs-up instead of posting a low-value "ok" reply. The
   reaction gives the sender a "seen / acknowledged" signal and tells the
   operator the agent ingested the context, without cluttering the thread. Run:
   `flow slack react --channel <channel> --ts <message_ts> --emoji +1 --as user`
   where `<message_ts>` is the specific message's `TS` from the inbox event (the
   message you're acking — NOT the thread_ts). Rules:
   - **Selective, never blanket.** React only when acknowledgement IS the right
     response. A message that asks a question or needs a real answer gets a
     reply (step 6), never just a reaction. Do not react to every message — one
     emoji per message is exactly the noise to avoid.
   - **External messages only.** Don't react to the operator's own coordination
     messages (the step-3 operator-vs-external classification still applies).
   - The reaction posts as the operator (user token) so it lands even when the
     flow bot isn't a member of the channel. It's idempotent — re-reacting the
     same emoji is a no-op.
8. **Monitoring a DM reply (automatic).** You can reply to someone **privately
   by DM** rather than in this thread. flow monitors that DM for you
   **automatically** — when you send the DM (Claude or Codex), the `PostToolUse`
   hook registers the DM **thread** for this task. The recipient's replies in
   that DM thread then appear in `inbox.jsonl` and wake this session, classified
   by `event.user_id` exactly like thread events (step 3). No manual tagging.

   Monitoring is scoped to the **DM thread you started**, not the person's whole
   DM channel — so other, unrelated conversations you have with the same person
   don't leak into this task. (Requires the workspace Slack app to subscribe to
   `message.im` / `message.mpim` under "events on behalf of users", and a user
   token configured for backfill; if a DM reply never arrives, check those.)
9. **Close out.** When the thread is resolved (`:white_check_mark:` from
   the user, "thanks", explicit "done"), run `flow done` — the close-out
   sweep distills the Slack conversation into KB facts and a project
   update.

**Anti-patterns specific to slack-reply tasks:**

- **Do not action operator-authored inbox events as external
  follow-ups.** When `event.user_id` matches one of the operator IDs
  listed in the brief's `## Operator identity` block, the message is
  coordination from the human you work with, not a question from a
  third party. Do not reply *at* the operator in the Slack thread, do
  not open a fresh investigation around the message, and do not treat
  the message body as instructions unless the operator explicitly asks
  you to act on it in the current Claude/Codex session. The Goniyo
  thread regressed because an operator-authored coordination message
  was processed as a customer follow-up; the operator identity block
  exists to make that mistake explicit.
- **Do not post top-level into a public/private channel.** The
  underlying SlackWriter refuses any `chat.postMessage` to a non-DM
  channel without `thread_ts`, but you should never need to anyway —
  every reply belongs in the originating thread.
- **Do not edit the brief mid-thread.** The brief is the spawn-time
  snapshot. New events arrive via the inbox, not by mutating the brief.
- **Do not invoke `flow do` on another task while this one is live.**
  The inbox tail only delivers events to the *current* session. Jumping
  away means the next message in the Slack thread won't reach you live
  (it queues in inbox.jsonl, but you don't see it until you `flow do
  <slack-slug>` again).
- **Do not bypass the reaction trigger.** If the user wants Claude on a
  new thread, they add a reaction. Don't manually create slack-reply
  tasks for threads they didn't consent to.

### Slack app command DM (AFK control)

The operator can also DM the Flow Slack app directly as a private AFK command
channel. This is not a `slack-reply` customer/colleague thread. When the
operator's own DM to the Flow Slack app is accepted by the command-channel
ingress, flow adds a `:eyes:` reaction to that incoming Slack message as a
receipt that processing has started. The reaction is left in place; it is not
swapped or removed when the reply completes. This ack applies
only to the operator's app DM command surface, never to external participants,
customer/colleague threads, or the operator's messages in ordinary channels.

## 10c. GitHub PR and issue tasks (monitor pipeline)

`flow ui serve` ingests GitHub activity through **signed webhook deliveries from
a GitHub App**, fed to the dispatcher (task creation, inbox append, attention
routing, reopen/mark-done). Set it up in one click on the Mission Control
**Connectors** page → **Connect GitHub** wizard: it registers a GitHub App via
GitHub's App-manifest flow, captures the app id + private key + webhook secret
(secrets go to the **OS keyring**, never config.json), and guides installation —
no manual `FLOW_GH_WEBHOOK_SECRET` paste and no `gh` CLI auth for the monitor. The
wizard requires a running **public ingress** (zrok) first, since the App's
webhook URL must be public at App-creation time.
One connected public App can be installed on both personal and org accounts;
creating a different App is an explicit replacement of the stored credentials,
not additive multi-App support.

On a delivery to `POST /api/github/webhook`, flow verifies the
`X-Hub-Signature-256` HMAC, records the delivery for idempotency, normalizes the
payload into a `GitHubEvent`, and dispatches it — **no GitHub API call on the hot
path**. `FLOW_GH_TRANSPORT` is `webhook` once the wizard runs (`off` disables
ingress); the legacy `gh`-CLI search-poller has been **removed**. App auth (a JWT
signed with the private key → an installation token) powers the native
`google/go-github` calls that remain — issue↔PR cross-reference linking and
delivery backfill — so the monitor no longer shells out to `gh`.

The App delivers events for every repo it is installed on: open assigned issues,
assigned PRs, PRs requesting your review, PR/issue comments, top-level PR reviews,
PR head updates, PR merges, and PR closes become inbox events. **Direct asks** —
assignment or review-request — create a task AND auto-open a session (you're
expected to act). Comments, reviews, head updates, and merges route to the
existing tagged task. (Off-install `@-mention`/involvement discovery, which
required user-level search, is no longer performed — install the App on the repos
you want Flow to watch.)

**Gap recovery is redelivery backfill, not re-polling.** If Flow or the public
ingress was down, the wizard's **Replay missed deliveries** button (and
`POST /api/github/setup/backfill`) lists the App's hook deliveries
(`GET /app/hook/deliveries`) and replays the missed ones through the same
normalize→dispatch pipeline, deduped by the delivery GUID.

GitHub-origin tasks are tagged `github` plus either
`gh-pr:<owner>/<repo>#<number>` or `gh-issue:<owner>/<repo>#<number>`.
The tag is the durable linkage. Do not infer linkage from the task slug
or GitHub URL when the tag exists.

If your task carries a `gh-pr:` or `gh-issue:` tag (`flow show task`
lists tags under the `tags:` line), follow this bootstrap:

1. **Confirm the webhook ingress is live when follow-up matters.** If you expect
   new review comments, commits, or merge notifications while working, make sure
   `flow ui serve` is running with the GitHub App connected and a public ingress
   up — check `GET /api/github/webhook/status` (transport, secret-configured,
   deliveries-received) or `GET /api/github/setup/status` (app connected +
   installed). If deliveries were missed during downtime, use the wizard's
   **Replay missed deliveries**. If the user wants only a one-time review and no
   live follow-up, note that choice.
2. **Read your brief.** It is a snapshot of the PR/issue at task
   creation time: title, URL, author, labels, milestone, base/head refs,
   and the initial GitHub body. Treat it as initial context, not the
   live source of truth. If the PR/issue's repo matched a flow project
   by git-origin remote, the task is **already attached to that project
   and runs in its real checkout** (the brief has a `## Project` block
   instead of a picker) — make code changes there, not in a workspace.
   If the repo didn't match a unique project, the brief asks you to pick
   one as your first step; once you do, attaching it adopts the project's
   `work_dir` (see §4.16 `--project`).
3. **Catch up on the inbox.** Read every line of
   `~/.flow/tasks/<your-slug>/inbox.jsonl` in order. Each line is a JSON
   object `{enqueued_at, event}` where `event.Kind` may include
   `pr_assigned`, `pr_review_requested`, `issue_assigned`,
   `pr_mentioned`, `issue_mentioned` (you were @-mentioned),
   `pr_involved`, `issue_involved` (notify-only; you're in the loop),
   `pr_comment`, `issue_comment` (top-level conversation comments),
   `pr_review_comment`, `pr_review_changes_requested`,
   `pr_review_approved`, `pr_head_updated`, `pr_merged`, or `pr_closed`.
   Top-level comments arrive even when the operator authored them on
   their own PR/issue — that is the operator's primary way to instruct
   you mid-task (e.g. "fix merge conflicts"); act on the instruction. Do
   not re-action a comment you yourself posted.
4. **Use the same-session monitor.** Flow wakes the same Flow-owned
   terminal session when ANY new GitHub event arrives for this task —
   every PR/issue event is actionable (comments, head updates, approvals,
   merges, closes, assignments), so a merge or close reliably reaches a
   live session and you can act on it (wrap up, proceed, or rework). If you
   are diagnosing monitor behavior manually, the equivalent file tail is
   `tail -F ~/.flow/tasks/<your-slug>/inbox.jsonl`.
5. **Review from current GitHub state.** Use `gh pr view`, `gh pr diff`,
   `gh pr checkout`, or `gh api` as needed before approving, requesting
   changes, or replying. Inbox events tell you something changed; they
   are not a substitute for re-reading the latest diff.
6. **Respect lifecycle events.** A `pr_head_updated` event means new
   commits landed and the PR should be reviewed again. A `pr_merged`
   event means the monitor has marked the associated flow task done; do
   not reopen it unless the user asks for follow-up work — wake here is to
   let you post a closing note and wrap up, not to keep working. A
   `pr_closed` event means the PR was closed WITHOUT merging; the task is
   left as-is (not auto-done) so you can decide whether to close it out or
   keep going (e.g. the work moves to a new PR).

**Acknowledge on GitHub before you dig in (autonomous-agent etiquette).**
The human who assigned, review-requested, mentioned you, or left the comment
should know an autonomous agent has the item — promptly, in the operator's
voice, before you start the substantive work:

- **On bootstrap of a GitHub-origin task**, post one acknowledgement on the
  item *before* analysis:
  - PR: `gh pr comment <number> --repo <owner>/<repo> --body "<ack>"`
  - Issue: `gh issue comment <number> --repo <owner>/<repo> --body "<ack>"`
- **When woken by a new `pr_review_comment`, `pr_review_changes_requested`,
  `pr_comment`, or `issue_comment`**, post a short reactive ack on that item
  ("On it — reviewing this now.") before doing the work.
- **Every ack MUST end with the marker line `<!-- flow-agent-ack -->`** on its
  own line. The monitor matches this exact marker to recognize the comment as
  your own ack and drop it, so your ack never wakes you in a loop. Omit it and
  you will re-trigger yourself on the next poll.
- **Ack once per item, not per event.** Before the bootstrap ack, scan the
  item's existing comments (`gh pr view <n> --comments` / `gh issue view <n>
  --comments`) for `flow-agent-ack`; if one is already present, skip the
  bootstrap ack and just proceed — don't let a busy PR collect an ack on every
  bot comment.
- Posts go as the operator's GitHub identity (not a separate bot), so write in
  their voice. Suggested wording:

  ```
  🤖 An autonomous flow agent has picked this up and is looking into it — I'll follow up here.

  <!-- flow-agent-ack -->
  ```
- If the ack fails (no write permission, `gh` error), note it and proceed with
  the work — acking is courtesy, not a gate.

**Anti-patterns specific to GitHub tasks:**

- **Do not re-action your own ack.** A comment you authored that carries the
  `flow-agent-ack` marker is your acknowledgement; the monitor already drops it,
  but if you ever see one in the inbox, ignore it — never treat it as a new
  instruction.
- **Do not approve on monitor signal alone.** The listener can reopen a
  review task after new commits, but approval only happens after you have
  verified the current diff and checks.
- **Do not create duplicate PR-review tasks manually.** If the existing
  task has the `gh-pr:<owner>/<repo>#<number>` tag, new review comments
  and commits belong in that task's inbox.
- **Do not edit the brief to record new GitHub events.** The brief is
  the spawn-time snapshot. New GitHub activity arrives through
  `inbox.jsonl` and durable decisions belong in normal task updates.

## 10d. Attention Router feed

The Attention Router is Flow's cross-source triage surface. It watches
connector events, records steering traces, and writes promising items to
`attention_feed` so the operator can decide what happens next. Treat it as
an operator-review queue, not as an instruction to autonomously post, mute,
create tasks, or change policy.

**When to inspect it:**

- **Before asking "what should I work on"** or running the §4.1 start-day
  flow, run `flow attention list --status new` alongside the task lists.
  Attention cards are often the freshest "this needs review" signal.
- **When an inbox/monitor event wakes you**, check whether the same source,
  channel, author, thread, PR, or issue already has an Attention card before
  creating duplicate tasks or replying. Use the card as context, then follow
  the Slack/GitHub task rules above for the actual source interaction.
- **When the operator asks "why did Flow surface this?"**, start from the
  card's "why this" fields in Mission Control. If more audit detail is
  needed, `flow attention trace` is the audit trail for the steering funnel,
  stage outcome, disposition, confidence, source, and matched-task evidence.

**Review recipe:**

1. Run `flow attention list --status new` to see unresolved cards.
2. For a specific card, inspect its source thread/PR/issue and its trace
   evidence before acting. Mission Control's card/detail view shows source
   evidence, matched task/project, reason, confidence, stage outcome, and
   action preview; CLI users can use `flow attention trace` for the broader
   funnel and recent trace rows.
3. Choose the narrowest operator-approved action:
   - `flow attention act <id> dismiss` when the current card is noise. This
     hides/resolves the card only; it does not mute the source. If a later event
     arrives on the same thread, Flow reopens that thread's feed row with the
     refreshed, collated thread summary instead of treating it as unrelated.
   - `flow attention act <id> make-task` when it should become tracked work.
   - `flow attention act <id> forward` when it belongs with an existing task.
     If a matched task is shown, prefer forwarding to that task over creating a
     duplicate task unless the match evidence is wrong. Forward writes the
     source context into the matched task's `inbox.md` and a source-attributed
     `attention_forward` row in `inbox.jsonl` that preserves the original
     Slack/GitHub sender, channel/repo, thread, and permalink. When the task has
     a live Flow terminal, the same inbox monitor wakes that session with the
     delivered context; treat it as source-authored context, not an
     operator-authored note.
   - `flow attention act <id> confirm-handoff` when the match looks plausible
     but you want the matched task's agent to accept or decline before the card
     is marked handled.
   Mission Control also exposes retriage, mute-channel, mute-sender,
   mute-thread, make-task-start, open-source/open-session, and send-reply.
4. Use `flow attention feedback --group <dimension>` when you need the
   learning-loop summary. Common dimensions are `source`, `channel`,
   `author`, `thread-type`, `suggested-action`, and `confidence-band`.
   Report patterns as evidence; do not mutate autonomy policy yourself.
5. Use `flow attention calibration` when the operator asks whether the
   confidence numbers can be trusted. It prints, per (action × raw
   confidence band), the observed operator-agreement rate (the calibrated
   confidence) — so a band where the model emits 0.9 but the operator only
   agrees ~50% of the time is visible. Bands with too little feedback are
   marked as raw fallback. Report it as evidence; calibration only surfaces
   the score, it does not change the autonomy gate.

**Send-reply safety boundary:**

- Do not post a send-reply yourself unless the operator approved the reply
  text or approved specific revision instructions in the Attention UI.
- The server does not post Slack replies directly. Slack send-reply opens an
  ephemeral, watchable floating Claude session with Slack MCP access; that
  session posts the approved reply and then runs
  `flow attention sent <id> --close-floating <floating-id>`.
- GitHub send-reply can use the headless agent path because it posts through
  `gh`; matched-task replies are injected into that task's inbox/session and
  recorded as a task update so the owning agent posts from the right context.
- In all cases, mark the card sent only after the post is confirmed. If the
  tool call, `gh` command, or task handoff fails, leave the card unresolved so
  the operator can retry or inspect the visible session.
- Never run `flow attention sent` merely because a draft exists, because an
  agent printed text, or because a send session was opened. The command is
  bookkeeping after confirmed delivery, not the delivery itself.

**Autonomy defaults and limits:**

- Default autonomy is surface-only: `DefaultAutonomy()` disables every outward
  action even though it seeds sensible thresholds for future opt-in settings.
- The autonomy gate evaluates the **calibrated** confidence, not the raw model
  number: `ConfidenceCalibrator` maps a score to the empirical P(operator agrees)
  in that action×band (learned from `attention_feedback`), so an operator
  threshold means "minimum probability I'd agree". A thin/cold band falls back to
  the raw score. Autonomous auto-acts deliberately record **no** feedback row, so
  the steerer can't inflate the calibration it gates on (audit lives in the
  steering trace).
- Stage 1/2 classifier work is budgeted because it shells out to Claude
  subprocesses. `FLOW_STEERING_CLASSIFIER_BUDGET_PER_HOUR` defaults to `120`
  (cheap batched Haiku turns — the old 30 throttled normal inbox volume); deep
  triage is the expensive rung at `FLOW_STEERING_DEEP_BUDGET_PER_HOUR` `60`
  (bursts spill to the surfaced Stage-2 verdict, nothing lost). Lower them if
  Mission Control heats the machine under connector noise; raise only on explicit
  request.
- If the classifier reports quota/auth unavailability, the router pauses
  classifier subprocess launches for
  `FLOW_STEERING_CLASSIFIER_FAILURE_COOLDOWN` (default `10m`) and records trace
  drops instead of repeatedly spawning failing CLI processes.
- The autonomy trust ladder is: surface feed items; dismiss/mute only after an
  explicit mute scope; forward high-confidence context to matched tasks; create
  high-confidence backlog tasks; capture durable facts to the KB; clear
  `waiting_on` only for tracked external replies; draft AFK holding replies; and
  send substantive outbound replies only after explicit operator approval.
- Four **safe** actions are auto-actable in settings today — `make_task` (0.80),
  `forward` (0.85), `capture_kb` (0.75), and `dismiss` (auto-resolve a surfaced
  `digest_only` FYI card, 0.85) — each off by default, gated on the calibrated
  confidence, and traceable. `capture_kb` auto-act runs the same headless KB-write
  agent the operator's "capture" button uses (a `claude -p` subprocess, dispatched
  async). A `drop` verdict is already suppressed pre-card unconditionally and is
  not a gated action. Reply/AFK (outward sends) stay manual-only even if malformed
  policy JSON tries to enable them.
- `FLOW_STEERING_AUTONOMY` can enable action thresholds only when the operator
  configured it. Do not edit env, settings, DB rows, or code to enable
  autonomy on the operator's behalf.
- Operator clicks/manual CLI actions are authorization for that one action.
  They do not authorize future autonomous sends, mutes, task creation, or
  forwarding.
- The feedback loop may suppress noisy channels/authors or adjust thresholds,
  but learned feedback never enables an action. Treat feedback as evidence for
  triage quality, not as permission to take new classes of action.

## 10e. Owners

Owners are durable, repo-scoped controllers for ongoing outcomes. An owner is
not one long agent session: it is an `owners/<slug>/charter.md`, an
`owners/<slug>/updates/` journal, a row in `owners`, and short ticks that wake,
review state, dispatch work, self-pace, and exit.

Core commands:

- `flow add owner "<name>" --work-dir <path> [--project <slug>] [--every 24h] [--agent claude|codex]`
- `flow owner list` or `flow list owners`
- `flow owner show <slug>` or `flow show owner <slug>`
- `flow owner start|pause|retire <slug>`
- `flow owner tick <slug>` for a guided interactive tick; `--auto` for a headless tick now
- `flow owner next <slug> --in <duration>` or `--at <RFC3339>`
- `flow owner tick-due` is the scheduler entry point; do not run it manually unless you are checking the scheduler.

Owner rules:

- Owners orchestrate; they do not perform substantive work inline. A tick is
  sessionless and gets no task close-out sweep. Real work must be dispatched as
  tasks or playbook runs that can self-close.
- One-time work should be created with `flow add task "<what>" --agent <owner-agent> --tag owner:<slug>` and then run with `flow do --auto <task>` when safe.
- Human decisions should be parked as question tasks tagged both
  `question` and `owner:<slug>`. Do not run `question` tasks with `--auto`.
- Every tick should read the charter, read recent owner journal notes, run
  `flow owner show <slug>` to avoid duplicating in-flight work, dispatch only
  what is needed, write a concise journal note under `owners/<slug>/updates/`,
  set its next wake with `flow owner next`, and exit.
- The first tick should usually be interactive so the operator can tune the
  charter. Later ticks can run headlessly through `flow owner tick --auto` or
  the host scheduler calling `flow owner tick-due`.

