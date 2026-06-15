# Security

flowwyyy's core is a fully local CLI (the `flow` binary): the task manager,
knowledge base, and session spawner make no network calls and send no
telemetry — all state lives under `~/.flow/`. The blast radius of a core bug is
bounded by what flowwyyy already does on your machine: spawn terminal tabs,
write under `~/.flow/`, modify `~/.claude/settings.json` (the SessionStart
hook), and shell out to `claude` / `codex` / `osascript`.

The **opt-in** layers do reach the network — look here first for anything
network- or secret-related:

- **Mission Control** (`flow ui serve`) binds loopback only and ships **no
  auth**; the binary refuses non-loopback hosts without an explicit `--host`.
- **Connectors** (Slack, GitHub, zrok public ingress) make outbound/inbound
  calls when configured. Tokens and signing secrets are stored in the **OS
  keyring**, never in plaintext config.

If you find something that looks like a security issue (path traversal,
unintended privilege use, supply-chain concern, etc.), please open a
[GitHub issue](https://github.com/vishnukv-facets/flowwyyy/issues/new) with as
much detail as you're comfortable sharing publicly. flowwyyy has no
private-disclosure channel today; the project is small enough and the
attack surface narrow enough that public triage is acceptable for now.

If you'd prefer not to file publicly, mention that in the issue and a
maintainer will follow up.

## Supported versions

flow follows SemVer (`0.x.y` until the API stabilises). Only the latest
release receives security fixes. Binaries are published on the
[GitHub Releases](https://github.com/vishnukv-facets/flowwyyy/releases) page;
released binaries auto-detect version bumps and prompt to refresh the
embedded skill on next run.
