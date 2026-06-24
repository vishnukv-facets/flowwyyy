package app

// exports.go is the public surface of the app package for the product layer.
// It wraps unexported core helpers so product code (relocated ui/attention/
// slack handlers, the steerer-persona init-hook) can call them without those
// helpers having to be exported individually across their home files.
//
// The flag-parsing and flow-root/path/URL helpers that used to be exported here
// (FlagSet/ParseFlagSet/FlowRoot/FlowDBPath/FlowServerURL/UISessionToken/
// PreferredUIFlowBinary/LeadingHelpArg) were relocated to internal/cli, and the
// skill machinery (EmbeddedCoreSkill/SetEmbeddedSkill/RunSkillCommand) to
// internal/coreskill + internal/skillinstall — the product layer owns its own
// copies there (Phase-3 decoupling, seam §11.3.1, Tiers A + C), so the product
// no longer reaches into app for them. What remains here is the init-hook
// surface (Tier D, not yet shed).

// initHooks are run by `flow init` after core seeding. The product layer
// registers hooks (e.g. the steerer-persona seed) via RegisterInitHook so the
// core init path takes no dependency on product packages.
var initHooks []func() error

// RegisterInitHook registers fn to run during `flow init` after core seeding.
func RegisterInitHook(fn func() error) { initHooks = append(initHooks, fn) }
