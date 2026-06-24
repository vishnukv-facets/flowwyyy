package product

import (
	"errors"
	"flag"
	"flow/internal/agenthooks"
	"flow/internal/cli"
	"flow/internal/flowclient"
	"flow/internal/flowdb"
	"flow/internal/server"
	"flow/internal/workdirreg"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cmdUI(args []string) int {
	if len(args) == 0 {
		printUIUsage()
		return 0
	}
	switch args[0] {
	case "serve":
		return cmdUIServe(args[1:])
	case "-h", "--help", "help":
		printUIUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "error: unknown ui subcommand %q\n", args[0])
		printUIUsage()
		return 2
	}
}

func printUIUsage() {
	fmt.Println(`flow ui

Usage:
  flow ui serve [--host 127.0.0.1] [--port 8787] [--bg]

Serve the local Mission Control UI backed by the current flow database.`)
}

func cmdUIServe(args []string) int {
	fs := cli.FlagSet("ui serve")
	host := fs.String("host", "127.0.0.1", "host to bind")
	port := fs.Int("port", 8787, "TCP port to bind")
	bg := fs.Bool("bg", false, "run the UI server in the background")
	noQuote := fs.Bool("no-quote", false, "disable the anime quote shown beside the Mission Control greeting")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "error: ui serve takes no positional arguments")
		return 2
	}
	// Free the port from a stale `flow ui serve` first, so a rebuild + restart
	// actually takes over instead of leaving the OLD binary serving (a fresh bind
	// would fail with "address already in use" and, under --bg, die in the log
	// unnoticed). Done in the parent for both paths: foreground binds next, and the
	// --bg child re-execs into a now-free port.
	stopExistingFlowServer(*port)
	if *bg {
		return startUIBackground(*host, *port, *noQuote)
	}
	return serveUI(*host, *port, *noQuote)
}

// stopExistingFlowServer frees `port` by stopping an existing `flow ui serve`
// bound to it. It ONLY terminates a process it can confirm is a flow ui-serve —
// never an unrelated service that happens to hold the port — with SIGTERM,
// escalating to SIGKILL if the port isn't released. Best-effort: if lsof/ps are
// missing or the holder is foreign, it leaves the bind attempt to surface the
// conflict (and logs why it stood down).
func stopExistingFlowServer(port int) {
	for _, pid := range portListenerPIDs(port) {
		if pid <= 0 || pid == os.Getpid() {
			continue
		}
		cmdline := processCommandLine(pid)
		if !isFlowUIServeCommand(cmdline) {
			fmt.Fprintf(os.Stderr, "warning: port %d held by a non-flow process (pid %d); not stopping it: %s\n", port, pid, cmdline)
			continue
		}
		fmt.Fprintf(os.Stderr, "stopping existing flow ui server (pid %d) on port %d\n", pid, port)
		_ = syscall.Kill(pid, syscall.SIGTERM)
		if waitPortFree(port, 3*time.Second) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
		waitPortFree(port, 2*time.Second)
	}
}

// isFlowUIServeCommand reports whether a process command line is a flow ui-serve.
// The guard that keeps stopExistingFlowServer from killing an unrelated service
// that merely shares the port: an attacker/neighbor process won't carry the
// distinctive "ui serve" invocation.
func isFlowUIServeCommand(cmdline string) bool {
	return strings.Contains(cmdline, "ui serve")
}

// portListenerPIDs returns the PIDs LISTENing on the given TCP port (via lsof).
// Empty on any error (lsof absent, no listener) — the caller treats that as
// "port free / can't tell" and proceeds to bind.
func portListenerPIDs(port int) []int {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func processCommandLine(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// waitPortFree polls until no process LISTENs on the port (or timeout). Returns
// whether the port came free.
func waitPortFree(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if len(portListenerPIDs(port)) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func serveUI(host string, port int, noQuote bool) int {
	root, err := cli.FlowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	dbPath, err := cli.FlowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		return 1
	}
	defer db.Close()
	if _, err := workdirreg.SyncGitRemotes(db); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync workdir remotes: %v\n", err)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: find executable: %v\n", err)
		return 1
	}
	// Prefer the flowclient-resolved core `flow` binary (set by the flowwyyy
	// main): the server execs it for mutations + launch prep, and the agent hook
	// command path / scheduler tick-due must invoke `flow`, not flowwyyy. In the
	// no-FlowBin case (tests / running flow directly), fall back to the running
	// executable, which is itself a flow-capable binary.
	commandPath := FlowBin
	if commandPath == "" {
		commandPath = cli.PreferredUIFlowBinary(exe)
	}
	hookHost := host
	if hookHost == "0.0.0.0" || hookHost == "::" {
		hookHost = "127.0.0.1"
	}
	hookURL := "http://" + net.JoinHostPort(hookHost, strconv.Itoa(port)) + "/api/hooks/agent"
	if changed, err := agenthooks.InstallKnownWorkdirsWithOptions(db, agenthooks.InstallOptions{
		CommandPath: commandPath,
		HookURL:     hookURL,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: install local agent hooks for existing workdirs: %v\n", err)
	} else if changed > 0 {
		fmt.Fprintf(os.Stderr, "installed local agent hooks in %d existing workdir(s)\n", changed)
	}
	srv := server.New(server.Config{
		DB:           db,
		FlowRoot:     root,
		Version:      Version,
		CommandPath:  commandPath,
		Flow:         flowclient.Client{Bin: commandPath},
		HookURL:      hookURL,
		DisableQuote: noQuote,
	})
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	fmt.Fprintf(os.Stderr, "flow ui listening on http://%s\n", addr)
	if commandPath != exe {
		fmt.Fprintf(os.Stderr, "flow ui commands using %s\n", commandPath)
	}
	if host == "0.0.0.0" {
		fmt.Fprintln(os.Stderr, "warning: bound to 0.0.0.0; anyone on this network can read and operate your flow data")
	}
	return srv.ListenAndServe(addr)
}

func startUIBackground(host string, port int, noQuote bool) int {
	root, err := cli.FlowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: find executable: %v\n", err)
		return 1
	}
	exe = cli.PreferredUIFlowBinary(exe)
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: create log dir: %v\n", err)
		return 1
	}
	logPath := filepath.Join(logDir, "ui-serve.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open log: %v\n", err)
		return 1
	}
	defer logFile.Close()

	bgArgs := []string{"ui", "serve", "--host", host, "--port", strconv.Itoa(port)}
	if noQuote {
		bgArgs = append(bgArgs, "--no-quote")
	}
	cmd := exec.Command(exe, bgArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error: start background server: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not release background process: %v\n", err)
	}
	fmt.Printf("flow ui serving at http://%s\n", net.JoinHostPort(host, strconv.Itoa(port)))
	fmt.Printf("pid %d · log %s\n", pid, logPath)
	return 0
}
