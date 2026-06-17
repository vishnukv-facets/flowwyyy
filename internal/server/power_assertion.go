package server

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type powerAssertionProcess interface {
	Stop() error
}

type execPowerAssertionProcess struct {
	cmd *exec.Cmd
}

func caffeinateCommandForPID(pid int) *exec.Cmd {
	return exec.Command("caffeinate", "-i", "-w", strconv.Itoa(pid))
}

var newPowerAssertionProcess = func(pid int) (powerAssertionProcess, error) {
	cmd := caffeinateCommandForPID(pid)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execPowerAssertionProcess{cmd: cmd}, nil
}

func (p *execPowerAssertionProcess) Stop() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return nil
}

type powerAssertion struct {
	proc powerAssertionProcess
}

func keepAwakeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_UI_KEEP_AWAKE"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func startPowerAssertion() (*powerAssertion, error) {
	if !keepAwakeEnabled() {
		return nil, nil
	}
	proc, err := newPowerAssertionProcess(os.Getpid())
	if err != nil {
		return nil, err
	}
	return &powerAssertion{proc: proc}, nil
}

func (p *powerAssertion) stop() {
	if p == nil || p.proc == nil {
		return
	}
	_ = p.proc.Stop()
}

func (s *Server) syncPowerAssertion() {
	s.powerAssertionMu.Lock()
	defer s.powerAssertionMu.Unlock()

	if keepAwakeEnabled() {
		if s.powerAssertion != nil {
			return
		}
		assertion, err := startPowerAssertion()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: keep-awake assertion: %v\n", err)
			return
		}
		s.powerAssertion = assertion
		return
	}
	if s.powerAssertion != nil {
		s.powerAssertion.stop()
		s.powerAssertion = nil
	}
}

func (s *Server) stopPowerAssertion() {
	s.powerAssertionMu.Lock()
	defer s.powerAssertionMu.Unlock()
	if s.powerAssertion == nil {
		return
	}
	s.powerAssertion.stop()
	s.powerAssertion = nil
}
