package app

// exports.go is the public surface of the app package for the product layer.
// It wraps unexported core helpers so product code (relocated ui/attention/
// slack handlers, the steerer-persona init-hook) can call them without those
// helpers having to be exported individually across their home files.
//
// The flag-parsing and flow-root/path/URL helpers that used to be exported here
// (FlagSet/ParseFlagSet/FlowRoot/FlowDBPath/FlowServerURL/UISessionToken/
// PreferredUIFlowBinary/LeadingHelpArg) were relocated to internal/cli — the
// product layer owns its own copies there (Phase-3 decoupling, seam §11.3.1,
// Tier A), so the product no longer reaches into app for them. What remains
// here is the skill machinery + init-hook surface (Tiers C/D, not yet shed).

// EmbeddedCoreSkill returns the embedded core skill fragment (SKILL.core.md).
// The product layer composes this with its own fragment to install the full
// agent skill via `flowwyyy skill install`.
func EmbeddedCoreSkill() []byte { return embeddedCoreSkill }

// SetEmbeddedSkill overrides the skill content that `flow skill install/update`
// and `flow skill print` emit. The flowwyyy binary sets this to the composed
// core+product skill so a product install preserves today's full agent
// experience; the core binary leaves it at the core-only default.
func SetEmbeddedSkill(b []byte) { embeddedSkill = b }

// RunSkillCommand dispatches a `skill` subcommand (install|update|print|
// uninstall) using the current embeddedSkill. The product layer calls this
// after SetEmbeddedSkill so install reuses the core path/hook-wiring logic.
func RunSkillCommand(args []string) int { return cmdSkill(args) }

// initHooks are run by `flow init` after core seeding. The product layer
// registers hooks (e.g. the steerer-persona seed) via RegisterInitHook so the
// core init path takes no dependency on product packages.
var initHooks []func() error

// RegisterInitHook registers fn to run during `flow init` after core seeding.
func RegisterInitHook(fn func() error) { initHooks = append(initHooks, fn) }
