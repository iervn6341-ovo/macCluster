package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAgentSupervisorOrdinaryExitStillRestarts(t *testing.T) {
	for _, code := range []string{"0", "1", "2"} {
		t.Run(code, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", agentSupervisor, "maccluster-supervisor", "/bin/sh", "-c", "exit "+code)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("agent exit %s: supervisor returned %v", code, err)
			}
		})
	}
}

func TestAgentSupervisorSignalPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
		parent bool
		want   int
	}{
		{"agent-SIGKILL-stays-stopped", syscall.SIGKILL, false, 0},
		{"agent-SIGTERM-still-restarts", syscall.SIGTERM, false, 1},
		{"supervisor-SIGTERM-forwards-and-reaps", syscall.SIGTERM, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", agentSupervisor, "maccluster-supervisor", "/bin/sh", "-c", `printf '%s\n' "$$"; exec /bin/sleep 60`)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			cmd.WaitDelay = time.Second
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			line, err := bufio.NewReader(out).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				t.Fatal(err)
			}
			target := pid
			if tc.parent {
				target = cmd.Process.Pid
			}
			if err = syscall.Kill(target, tc.signal); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			got := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				got = exit.ExitCode()
			}
			if ctx.Err() != nil || got != tc.want {
				t.Fatalf("exit=%d want=%d timeout=%v", got, tc.want, ctx.Err())
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("child was not reaped: %v", err)
			}
		})
	}
}

func TestServiceConfigHasNoSideEffects(t *testing.T) {
	var out bytes.Buffer
	run := func(...string) ([]byte, error) { t.Fatal("config invoked launchctl"); return nil, nil }
	if err := manageService([]string{"config", "--state-dir", "/tmp/test state"}, "/unused", "/tmp/test agent", "gui/123", &out, run); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "<key>SuccessfulExit</key><false/>") || !strings.Contains(out.String(), "maccluster-supervisor") {
		t.Fatal("missing restart policy")
	}
	if err := manageService([]string{"config", "unexpected"}, "/unused", "/unused", "gui/123", io.Discard, run); err == nil {
		t.Fatal("accepted trailing argument")
	}
}
