package app

import "flag"

// exports.go is the public surface of the app package for the product layer.
// It wraps unexported core helpers so product code (relocated ui/attention/
// slack handlers, the steerer-persona init-hook) can call them without those
// helpers having to be exported individually across their home files.

// FlagSet returns a configured *flag.FlagSet (ContinueOnError) for a command.
func FlagSet(name string) *flag.FlagSet { return flagSet(name) }

// LeadingHelpArg reports whether args starts with -h/--help.
func LeadingHelpArg(args []string) bool { return leadingHelpArg(args) }

// ParseFlagSet parses args using the standard app flag handling contract.
func ParseFlagSet(fs *flag.FlagSet, args []string) (handled bool, rc int) {
	return parseFlagSet(fs, args)
}

// FlowRoot returns the resolved flow root directory ($FLOW_ROOT or ~/.flow).
func FlowRoot() (string, error) { return flowRoot() }

// FlowDBPath returns the absolute path to the flow SQLite database.
func FlowDBPath() (string, error) { return flowDBPath() }

// FlowServerURL builds a Mission Control URL for the given path.
func FlowServerURL(path string) string { return flowServerURL(path) }

// UISessionToken returns the current Mission Control session token.
func UISessionToken() string { return uiSessionToken() }

// PreferredUIFlowBinary picks which binary UI-related child processes re-exec.
func PreferredUIFlowBinary(current string) string { return preferredUIFlowBinary(current) }

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
