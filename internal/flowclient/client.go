package flowclient

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

type Client struct {
	Bin string
}

type Result struct {
	Stdout string
	Stderr string
	Code   int
}

func (c Client) Run(ctx context.Context, args ...string) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	code = 0
	if err != nil {
		code = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		}
	}
	return out.String(), errOut.String(), code, err
}

func Exec(bin string, args []string) int {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "error: exec flow: %v\n", err)
		return 1
	}
	return 0
}

func (c Client) command(ctx context.Context, args ...string) (Result, error) {
	stdout, stderr, code, err := c.Run(ctx, args...)
	return Result{Stdout: stdout, Stderr: stderr, Code: code}, err
}

func (c Client) Do(ctx context.Context, args ...string) (Result, error) {
	return c.command(ctx, append([]string{"do"}, args...)...)
}

func (c Client) Done(ctx context.Context, slug string) (Result, error) {
	return c.command(ctx, "done", slug)
}

func (c Client) Add(ctx context.Context, args ...string) (Result, error) {
	return c.command(ctx, append([]string{"add"}, args...)...)
}

func (c Client) Update(ctx context.Context, args ...string) (Result, error) {
	return c.command(ctx, append([]string{"update"}, args...)...)
}

func (c Client) Archive(ctx context.Context, ref string) (Result, error) {
	return c.command(ctx, "archive", ref)
}

func (c Client) Unarchive(ctx context.Context, ref string) (Result, error) {
	return c.command(ctx, "unarchive", ref)
}

func (c Client) Delete(ctx context.Context, ref string) (Result, error) {
	return c.command(ctx, "delete", ref)
}

func (c Client) Restore(ctx context.Context, ref string) (Result, error) {
	return c.command(ctx, "restore", ref)
}

func (c Client) Spawn(ctx context.Context, args ...string) (Result, error) {
	return c.command(ctx, append([]string{"spawn"}, args...)...)
}

func (c Client) RunPlaybook(ctx context.Context, args ...string) (Result, error) {
	return c.command(ctx, append([]string{"run", "playbook"}, args...)...)
}

func (c Client) PlaybookTickDue(ctx context.Context) (Result, error) {
	return c.command(ctx, "playbook", "tick-due")
}

func (c Client) OwnerTickDue(ctx context.Context) (Result, error) {
	return c.command(ctx, "__owner-tick")
}
