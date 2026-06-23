package app

import "flag"

// exports.go is the public surface of the app package for the product layer.
// It wraps unexported core helpers so product code (relocated ui/attention/
// slack handlers, the steerer-persona init-hook) can call them without those
// helpers having to be exported individually across their home files.

// FlagSet returns a configured *flag.FlagSet (ContinueOnError) for a command.
func FlagSet(name string) *flag.FlagSet { return flagSet(name) }

// FlowRoot returns the resolved flow root directory ($FLOW_ROOT or ~/.flow).
func FlowRoot() (string, error) { return flowRoot() }

// FlowDBPath returns the absolute path to the flow SQLite database.
func FlowDBPath() (string, error) { return flowDBPath() }

// FlowServerURL builds a Mission Control URL for the given path.
func FlowServerURL(path string) string { return flowServerURL(path) }

// UISessionToken returns the current Mission Control session token.
func UISessionToken() string { return uiSessionToken() }

// initHooks are run by `flow init` after core seeding. The product layer
// registers hooks (e.g. the steerer-persona seed) via RegisterInitHook so the
// core init path takes no dependency on product packages.
var initHooks []func() error

// RegisterInitHook registers fn to run during `flow init` after core seeding.
func RegisterInitHook(fn func() error) { initHooks = append(initHooks, fn) }
