package server

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

type fakePowerProcess struct {
	stopped bool
}

func (f *fakePowerProcess) Stop() error {
	f.stopped = true
	return nil
}

func TestKeepAwakeEnabledDefaultsOff(t *testing.T) {
	t.Setenv("FLOW_UI_KEEP_AWAKE", "")
	if keepAwakeEnabled() {
		t.Fatal("keep-awake should default off")
	}
}

func TestPowerAssertionStartsCaffeinateWhenEnabled(t *testing.T) {
	t.Setenv("FLOW_UI_KEEP_AWAKE", "true")
	var gotPID int
	var proc fakePowerProcess
	old := newPowerAssertionProcess
	newPowerAssertionProcess = func(pid int) (powerAssertionProcess, error) {
		gotPID = pid
		return &proc, nil
	}
	t.Cleanup(func() { newPowerAssertionProcess = old })

	assertion, err := startPowerAssertion()
	if err != nil {
		t.Fatalf("startPowerAssertion: %v", err)
	}
	if assertion == nil {
		t.Fatal("expected assertion when FLOW_UI_KEEP_AWAKE=true")
	}
	if gotPID <= 0 {
		t.Fatalf("pid = %d, want current process pid", gotPID)
	}

	assertion.stop()
	if !proc.stopped {
		t.Fatal("stop should release the caffeinate process")
	}
}

func TestPowerAssertionSkipsWhenDisabled(t *testing.T) {
	t.Setenv("FLOW_UI_KEEP_AWAKE", "false")
	old := newPowerAssertionProcess
	newPowerAssertionProcess = func(pid int) (powerAssertionProcess, error) {
		t.Fatalf("unexpected caffeinate start for pid %d", pid)
		return nil, nil
	}
	t.Cleanup(func() { newPowerAssertionProcess = old })

	assertion, err := startPowerAssertion()
	if err != nil {
		t.Fatalf("startPowerAssertion disabled: %v", err)
	}
	if assertion != nil {
		t.Fatal("disabled keep-awake should not create an assertion")
	}
}

func TestCaffeinateArgsPinIdleSleepToServerPID(t *testing.T) {
	cmd := caffeinateCommandForPID(1234)
	if got, want := cmd.Args, []string{"caffeinate", "-i", "-w", strconv.Itoa(1234)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("caffeinate args = %#v, want %#v", got, want)
	}
}

func TestPowerAssertionStartErrorPropagates(t *testing.T) {
	t.Setenv("FLOW_UI_KEEP_AWAKE", "true")
	old := newPowerAssertionProcess
	newPowerAssertionProcess = func(pid int) (powerAssertionProcess, error) {
		return nil, errors.New("no caffeinate")
	}
	t.Cleanup(func() { newPowerAssertionProcess = old })

	assertion, err := startPowerAssertion()
	if err == nil {
		t.Fatal("expected start error")
	}
	if assertion != nil {
		t.Fatal("failed start should not return an assertion")
	}
}

func TestServerSyncPowerAssertionFollowsSetting(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	t.Setenv("FLOW_UI_KEEP_AWAKE", "")

	var proc fakePowerProcess
	starts := 0
	old := newPowerAssertionProcess
	newPowerAssertionProcess = func(pid int) (powerAssertionProcess, error) {
		starts++
		return &proc, nil
	}
	t.Cleanup(func() { newPowerAssertionProcess = old })

	srv.syncPowerAssertion()
	if starts != 0 || srv.powerAssertion != nil {
		t.Fatalf("disabled setting started assertion: starts=%d assertion=%v", starts, srv.powerAssertion)
	}

	t.Setenv("FLOW_UI_KEEP_AWAKE", "true")
	srv.syncPowerAssertion()
	if starts != 1 || srv.powerAssertion == nil {
		t.Fatalf("enabled setting did not start assertion: starts=%d assertion=%v", starts, srv.powerAssertion)
	}

	srv.syncPowerAssertion()
	if starts != 1 {
		t.Fatalf("sync should not start duplicate assertions, starts=%d", starts)
	}

	t.Setenv("FLOW_UI_KEEP_AWAKE", "false")
	srv.syncPowerAssertion()
	if !proc.stopped {
		t.Fatal("disabling setting should stop assertion")
	}
	if srv.powerAssertion != nil {
		t.Fatal("disabled setting should clear server assertion")
	}
}
