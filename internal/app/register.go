package app

import "flow/internal/cli"

// registerCore registers every current flow verb into the shared cli registry.
// app.Run dispatches through cli.Lookup, so this is the single source of truth
// for which subcommands exist in the (still single) binary.
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

	// Hidden headless entry points.
	regHidden("__auto-exec", cmdAutoExec)
	regHidden("__owner-tick", cmdOwnerTick)
}

func init() { registerCore() }
