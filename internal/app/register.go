package app

import "flow/internal/cli"

// registerCore registers every current flow verb into the shared cli registry.
// app.Run dispatches through cli.Lookup, so this is the single source of truth
// for which subcommands exist in the (still single) binary.
//
// ui/serve/attention/slack are registered here as TEMPORARY product shims so
// the single binary's behavior is unchanged. They are relocated into the
// product package in Task 6 and these shims are dropped in Task 10, when
// cmd/flowwyyy owns them. Until then they must stay so `flow ui serve`,
// `flow attention`, and `flow slack` keep working exactly as today.
func registerCore() {
	reg := func(name string, run func([]string) int) {
		cli.Register(cli.Command{Name: name, Run: run})
	}
	regHidden := func(name string, run func([]string) int) {
		cli.Register(cli.Command{Name: name, Run: run, Hidden: true})
	}

	reg("init", cmdInit)
	reg("add", cmdAdd)
	reg("do", cmdDo)
	reg("run", cmdRun)
	reg("playbook", cmdPlaybook)
	reg("done", cmdDone)
	reg("show", cmdShow)
	reg("search", cmdSearch)
	reg("standup", cmdStandup)
	reg("owner", cmdOwner)
	reg("list", cmdList)
	reg("edit", cmdEdit)
	reg("update", cmdUpdate)
	reg("archive", cmdArchive)
	reg("unarchive", cmdUnarchive)
	reg("delete", cmdDelete)
	reg("restore", cmdRestore)
	reg("workdir", cmdWorkdir)
	reg("skill", cmdSkill)
	reg("transcript", cmdTranscript)
	reg("hook", cmdHook)
	reg("spawn", cmdSpawn)
	reg("tell", cmdTell)
	reg("wait", cmdWait)
	reg("backup", cmdBackup)

	// Temporary product shims (relocated in Task 6, dropped in Task 10).
	reg("ui", cmdUI)
	regHidden("serve", cmdUIServe)
	reg("attention", cmdAttention)
	reg("slack", cmdSlack)

	// Hidden headless entry points.
	regHidden("__auto-exec", cmdAutoExec)
	regHidden("__owner-tick", cmdOwnerTick)
}

func init() { registerCore() }
