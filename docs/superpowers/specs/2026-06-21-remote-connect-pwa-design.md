# Remote Connect — Mobile PWA for live Claude/Codex sessions

- **Date:** 2026-06-21
- **Status:** Design — awaiting review
- **Author:** Vishnu KV (with Claude)
- **Scope:** New feature in `flowwyyy`. Self-contained; no Praxis dependency for v1.

## Goal

Let the operator reach Mission Control — and drive their live Claude/Codex
sessions — from a phone, away from the laptop. Tapping into a running session
from the phone gives the same interactive terminal the laptop tab has, backed
by the same underlying agent process.

A concrete user story: *"I'm away from my desk. My phone shows my running
sessions. I open the one that's mid-task, read what it's doing, and type a
reply into the same terminal my laptop is attached to."*

## Why a PWA (not a native app)

The entire Mission Control UI is already a Vite + React + TypeScript bundle that
is `go:embed`-ed into the `flow` binary (`internal/server/server.go:21`,
`internal/server/ui/`). **All** data flows over a single WebSocket-RPC socket
(`/ws/rpc`, `internal/server/rpc_bridge.go`) and the live terminal over
`/ws/terminal` (`internal/server/terminal_bridge.go`). A native iOS/Android app
would mean reimplementing the WS-RPC client and an xterm-equivalent terminal in
Swift/Kotlin/React Native. A PWA reuses the existing bundle verbatim.

The technical substrate is already favorable:

- **Sessions are tmux-backed** (`flow-<slug>`). tmux is inherently multi-client,
  so a phone attaching to the same session is the substrate's native behavior —
  we are adding a second door to a room that already supports multiple
  occupants, not building session-sharing from scratch.
- **The UI is transport-agnostic.** The same bundle, the same RPC envelope, the
  same terminal protocol serve localhost and a remote phone identically.

## Decisions locked during brainstorming

1. **Client:** PWA (installable web app), not a native app.
2. **Reachability + auth:** Standalone and hardened — reuse the existing zrok
   ingress, expose the app behind a new authenticated public surface, and add a
   per-device credential class. **No Praxis dependency** in v1.
3. **Mobile scope:** Full Mission Control, made responsive down to phone widths
   — not a terminal-only slice. Shipped in phases (see Phasing).

## Non-goals (v1)

- **Web push notifications** to the phone (attention cards, "agent needs input").
  A compelling follow-on, but it adds VAPID/web-push infrastructure. Out of scope.
- **Praxis SSO / Praxis relay.** Deferred to a possible v2 (see "Future: Praxis").
- **Native app.**
- **Offline data.** The app is entirely live-WS-driven; the service worker caches
  only the static shell for installability. There is no meaningful offline mode
  for a remote terminal.

## Architecture overview

```
  Phone (installed PWA)              laptop (flow ui serve)
 ┌──────────────────┐  zrok overlay  ┌────────────────────────────────────┐
 │  Mission Control │ ─────────────► │  remote-access mux (NEW, opt-in)    │
 │   React bundle   │  TLS, public   │   ├─ static PWA bundle (existing)   │
 │  + device token  │     URL        │   ├─ /ws/rpc       (existing)       │
 │  (paired)        │ ◄───────────── │   ├─ /ws/terminal → tmux flow-<x>   │──┐
 └──────────────────┘                │   └─ /ws/events    (existing)       │  │ same
                                     │                                     │  │ tmux
                                     │  webhook mux (UNCHANGED, minimal)   │  │ session
                                     │   └─ /api/github/webhook + OAuth    │  │
                                     └────────────────────────────────────┘  │
                                              laptop tab also attaches ───────┘
```

The remote path reuses the existing data plane wholesale. The only genuinely new
server code is (a) a bounded public surface, and (b) a device-token + pairing
layer. Everything else is CSS/layout in the frontend.

## Component 1 — Remote-access ingress

**Today:** `ingressMux()` (`internal/server/ingress.go:525`) is deliberately tiny
— only `/api/github/webhook` and OAuth callbacks are public. The full UI/API is
never exposed; the main server binds to `127.0.0.1:8787` by default
(`internal/app/serve.go`).

**Change:** Add a **distinct remote-access handler** served over zrok that serves
the full *authenticated* app surface (static PWA bundle, `/ws/rpc`,
`/ws/terminal`, `/ws/events`). Keep the existing webhook mux exactly as-is.

Rationale for keeping them separate:
- The webhook share is a pinned/reserved URL registered with GitHub — do not
  entangle it with a general app surface.
- "Expose the whole app" deserves one clearly-bounded, auditable surface with its
  own lifecycle and its own auth gate.

**Opt-in, off by default.** A "Remote access" toggle in Settings brings the share
up. When off, nothing of the app is public and the webhook ingress is untouched.
`flow ui serve` must never silently expose a shell.

Open implementation choice (resolve in plan): a **separate reserved zrok share**
vs. a path/host split on the existing share. Leaning separate share for blast-radius
clarity.

## Component 2 — Device authentication & pairing

**Today:** A single 256-bit shared token minted at boot, stored at
`~/.flow/.ui-session-token`, presented as `?token=` on WS URLs or the
`X-Flow-Session-Token` header (`internal/server/session_token.go:50-96`). Good for
localhost; too weak to expose a live shell publicly (not revocable per client,
shared by every consumer).

**Change:** Add a **per-device credential class** layered on top of the existing
token (which keeps working for localhost):

- New `remote_devices` table: one row per paired device — its own token (hashed
  at rest), a label ("Vishnu's iPhone"), `created_at`, **`expires_at`**,
  `last_seen_at`, `revoked_at`, revocable independently.
- **Device tokens expire 12 hours after pairing (locked decision).** A token
  minted via QR pairing is valid for exactly 12h; after that the phone must
  re-pair (scan a fresh QR from the laptop). This bounds the exposure of a lost
  or stolen phone to at most 12h even before the operator revokes, and fits the
  AFK use case (a daily re-pair from the laptop is acceptable). The alternative —
  never-expiring tokens — was rejected.
- **Pairing flow:** laptop Settings → "Add device" → server mints a short-lived
  (≈5 min), single-use **pairing code** and shows a QR encoding the public URL +
  code. The phone scans it, redeems the code, and receives its own **device
  token** (valid 12h), stored in the PWA (localStorage). The laptop's localhost
  token is never shared to the phone. Two distinct lifetimes: the pairing *code*
  is a ~5-minute window to scan; the device *token* it yields is valid 12h.
- Remote `/ws/*` connections require a **device** token (distinct from the
  localhost token). The existing exact-host `Origin` check
  (`checkLocalWSOrigin`, `session_token.go:103`) continues to apply and is
  naturally satisfied because the PWA is served from, and connects back to, the
  same zrok host.
- **Revocation:** revoke a single device (lost phone) without disturbing the
  laptop session or other devices. This is the core capability a shared token
  cannot provide and the main reason for the new class.

A visible "paired devices" list in Settings (laptop only) shows each device's
label, created/expiry/last-seen, and a revoke button. The label is auto-derived
from the device's user-agent into a friendly type (iPhone / iPad / Android phone
/ Mac / …) so the operator can tell at a glance which device is paired. Revoke is
reachable **only** from the laptop (localhost-gated, and blocked on the remote RPC
denylist) — a paired phone can never list or revoke devices.

## Component 3 — The PWA shell

- `manifest.json`: `display: standalone`, app name, icons, theme color.
- A **service worker** that caches the static shell for installability and fast
  cold-start. Data remains live over WS — no offline data caching.
- "Add to Home Screen" on iOS and Android.
- Existing `index.html:6` already has `viewport-fit=cover` for notch/safe-area —
  good starting point.

## Component 4 — Responsive Mission Control

The UI is desktop-first today: the sidebar only collapses to icons at 1080px and
never hides; the smallest breakpoint is 720px; lists are dense tables; graphs and
diffs assume width (`internal/server/ui/src/styles/app.css`). Making it work down
to ~375px is the bulk of the effort. Decomposed by surface:

- **Shell / nav** (`components/Shell.tsx`): always-present sidebar → bottom tab bar
  or slide-in drawer at phone widths.
- **Lists** (tasks, sessions, inbox): dense tables → stacked cards below ~640px.
- **SessionDetail** (`/session/:slug`): tabbed panels stack vertically; the
  terminal goes full-bleed.
- **Terminal input** *(the hard spot)*: `components/Terminal.tsx` + xterm on a
  mobile soft keyboard needs an **accessory key row** (Esc, Tab, Ctrl, arrows,
  `|`, `/`, etc.). iOS Safari's soft keyboard exposes none of those keys natively
  and interacts awkwardly with IME — this needs real care and device testing.
- **Graph / diff / analytics:** wrap in horizontal-scroll containers or provide
  simplified mobile views.

## Phasing

Ship the valuable core first, then breadth.

- **P1 — core remote-control path:** remote-access ingress + device pairing +
  responsive shell/nav + session list + the terminal view (with the accessory key
  row). This delivers the headline user story end to end.
- **P2 — full breadth:** make the remaining screens (projects, KB, analytics,
  connectors, graph, etc.) responsive.

## Security model & boundaries

**Threat model: single operator.** The operator — and *only* the operator — may
reach Mission Control and the live shells. There are no other users, no sharing,
no guest access. A live arbitrary-command shell exposed over a public URL is the
entire risk surface, so the design fails closed at every gate and treats local
control of the laptop as the sole root of trust.

**Root of trust — local control of the laptop.** A new device can be paired
*only* from the localhost-authenticated UI on the laptop (the "Add device" action
is not reachable over the remote mux). To bring a phone in, you must already have
local/physical control of the machine. No remote client can pair another device,
escalate, or widen access. This is the load-bearing invariant: the public surface
can only ever be reached by a device the operator deliberately paired from the
laptop.

**Concrete gates (all must hold):**

1. **Opt-in, off by default.** Remote access is toggled on in Settings; until
   then nothing of the app is public. The GitHub webhook ingress is never
   affected.
2. **Bounded surface.** The remote app is served by a **separate mux** from the
   webhook ingress, with its own auth gate — one auditable surface.
3. **Pairing only from localhost.** Pairing-code *generation* lives behind the
   localhost token (laptop only). Pairing-code *redemption* is the one endpoint
   reachable remotely, and it is tightly constrained (next gate).
4. **Pairing codes: high-entropy, short-lived, single-use.** A code is
   cryptographically random, expires in minutes, and is consumed on first
   redemption. An intercepted or stale code is useless.
5. **Per-device tokens, hashed at rest.** Each paired device gets its own token,
   stored hashed (never plaintext) in the `remote_devices` table; the presented
   token is hashed before the indexed lookup so no secret-dependent string
   comparison happens in the app.
6. **Device tokens expire after 12h.** A QR-paired device token carries
   `expires_at = created_at + 12h`; validation rejects expired tokens and the
   phone must re-pair. This caps a lost phone's exposure at 12h independent of
   revocation.
7. **Fail closed.** Every remote route requires a valid device token; a missing,
   malformed, expired, or revoked token is rejected. No anonymous or
   token-optional path exists on the remote mux (except static shell assets and
   the rate-limited pairing-redemption endpoint, which is how a device obtains
   its first token).
8. **Rate limiting / lockout** on pairing-code redemption and failed token
   validation to resist brute force over the public URL.
9. **Strict origin check** preserved on the remote path (`checkLocalWSOrigin`).
10. **Per-device revocation + audit.** A "paired devices" list in Settings shows
    each device's label, expiry, and last-seen time with a revoke button, so a
    lost phone is killed independently of the laptop session and an unrecognized
    device is immediately visible.
11. **Defense in depth.** The zrok share URL is itself unguessable, but is treated
    as obfuscation only — never as an access-control gate. The device token is the
    real gate.

## Known limitations (v1)

- **Revoke / disable does not terminate live sockets.** Token and revocation
  status are checked at WebSocket handshake time only. Revoking a device (or
  disabling remote access entirely) immediately blocks *new* connections, but a
  phone that already holds an open `/ws/terminal` keeps it until the connection
  drops. Exposure is bounded by the 12h token expiry. Per-connection teardown on
  revoke is a deliberate follow-on — the added complexity (broadcasting a
  close signal to all in-flight sockets for a given device) was out of scope for
  the v1 single-operator implementation.

## Open questions

1. **tmux resize conflict.** tmux sizes a shared session to its *smallest*
   attached client. With laptop + phone attached simultaneously, the laptop pane
   shrinks to phone dimensions. Proposed v1 resolution: a **handoff model** (phone
   takes over while the operator is away from the desk), which sidesteps the
   resize fight and matches the AFK use case. True simultaneous side-by-side needs
   more thought (per-client window sizing, or accepting the shrink). Note: the
   current bridge already creates a *transient* terminalSession for a second
   concurrent browser (`terminal_hub.go:49`); confirm in implementation how this
   interacts with a phone+laptop pair on one tmux session.
**Resolved during planning:**

- **Share strategy (was open):** v1 reuses the **existing single zrok share** with
  a **composite public handler** — the GitHub webhook + OAuth mux is served
  unchanged, and a separate, device-token-gated remote-app mux is served on the
  same share only when remote access is enabled. Separation is achieved at the mux
  level (distinct, separately-authenticated handlers), not by a second share. This
  reuses the reserved-URL machinery and gives the phone one stable bookmark.
- **Device-token expiry (was open):** locked to **12h, forced re-pair** (see
  Component 2).

## Future: Praxis (v2, not now)

The clean seam in this design is that **"remote" is purely an ingress + auth
concern, not an app concern.** Swapping the standalone device-token auth for
Praxis SSO (the operator logs in via Facets; flowwyyy validates that identity) or
a Praxis relay (laptop dials out; phone connects through Praxis; no inbound public
port) changes *only* Component 2's auth layer — with zero PWA or responsive-layout
rework. The flowwyyy↔Praxis authenticated client being built for the AI-metrics
telemetry tasks (`flowwyyy-praxis-telemetry-client`, `praxis-ai-metrics-ingest`)
is a *shared auth primitive*, not a shared feature — those telemetry tasks remain
a separate effort and should not be clubbed with remote-connect.

## Affected packages / files (grounding)

- `internal/server/ingress.go` — add remote-access mux + zrok share lifecycle.
- `internal/server/session_token.go` — device-token class alongside the shared
  token; remote auth gate.
- `internal/flowdb/` — `devices` table (schema DDL + CRUD + migration).
- `internal/app/serve.go` / Settings API — "Remote access" toggle, pairing
  endpoints.
- `internal/server/ui/` — PWA manifest + service worker; responsive CSS across
  `Shell.tsx`, list views, `SessionDetail`, `Terminal.tsx`, graph/diff/analytics.

## Testing approach

- Go: table-driven tests for device-token mint/redeem/validate/revoke and the
  remote-access mux gate (real SQLite temp dir per repo convention; no DB mocks).
  zrok mocked via function var as other external processes are.
- Pairing flow: unit-test code mint/redemption and single-use/expiry semantics.
- Frontend: manual device testing for responsive breakpoints and the terminal
  accessory-key row on real iOS Safari + Android Chrome (the soft-keyboard
  behavior is not reliably reproducible in a desktop emulator).
