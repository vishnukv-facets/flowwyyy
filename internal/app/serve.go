package app

import (
	"errors"
	"flag"
	"flow/internal/agenthooks"
	"flow/internal/flowdb"
	"flow/internal/monitor"
	"flow/internal/server"
	"flow/internal/workdirreg"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	fs := flagSet("ui serve")
	host := fs.String("host", "127.0.0.1", "host to bind")
	port := fs.Int("port", 8787, "TCP port to bind")
	bg := fs.Bool("bg", false, "run the UI server in the background")
	// Sentinel -1 = "flag not explicitly set, let the poller resolve from
	// FLOW_MONITOR_POLL_INTERVAL env then default 60s". 0 = "operator
	// disabled the poller". Anything > 0 = explicit cadence. Go's flag
	// package can't distinguish "default 0" from "user passed 0", so we
	// use the negative sentinel.
	monitorInterval := fs.Duration("monitor-interval", -1,
		"background poll cadence for monitor sources; 0 disables; default 60s or $FLOW_MONITOR_POLL_INTERVAL")
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
	if *bg {
		return startUIBackground(*host, *port)
	}
	return serveUI(*host, *port, *monitorInterval)
}

func serveUI(host string, port int, monitorInterval time.Duration) int {
	root, err := flowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	dbPath, err := flowDBPath()
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
	commandPath := preferredUIFlowBinary(exe)
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
		DB:                  db,
		FlowRoot:            root,
		Version:             Version,
		CommandPath:         commandPath,
		HookURL:             hookURL,
		MonitorPollInterval: monitorInterval,
	})
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	fmt.Fprintf(os.Stderr, "flow ui listening on http://%s\n", addr)
	if commandPath != exe {
		fmt.Fprintf(os.Stderr, "flow ui commands using %s\n", commandPath)
	}
	if host == "0.0.0.0" {
		fmt.Fprintln(os.Stderr, "warning: bound to 0.0.0.0; anyone on this network can read and operate your flow data")
	}
	// Advertise the bound URL so sibling flow CLI processes (flow done,
	// flow update task --waiting) can build deep links to this server in
	// Slack notices. 0.0.0.0 isn't useful in a clickable URL — substitute
	// localhost so users on the same machine get a working link; remote
	// callers should set FLOW_BASE_URL anyway.
	advertiseHost := host
	if advertiseHost == "0.0.0.0" || advertiseHost == "::" {
		advertiseHost = "127.0.0.1"
	}
	advertisedURL := "http://" + net.JoinHostPort(advertiseHost, strconv.Itoa(port))
	if path, err := monitor.WriteServerURLFile(advertisedURL); err != nil {
		fmt.Fprintf(os.Stderr, "warning: publish server URL to %s: %v\n", path, err)
	}
	defer func() {
		if err := monitor.RemoveServerURLFile(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove server URL file: %v\n", err)
		}
	}()
	return srv.ListenAndServe(addr)
}

func startUIBackground(host string, port int) int {
	root, err := flowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: find executable: %v\n", err)
		return 1
	}
	exe = preferredUIFlowBinary(exe)
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

	cmd := exec.Command(exe, "ui", "serve", "--host", host, "--port", strconv.Itoa(port))
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

func preferredUIFlowBinary(fallback string) string {
	return "flow"
}
