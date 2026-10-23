package runtime

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runningNative(t *testing.T, e Engine, uid, script string, policy string, env map[string]string) Instance {
	t.Helper()
	spec := Spec{UID: uid, Name: uid, Namespace: "ns", Executor: "native", Command: []string{"/bin/sh", "-c", script}, RestartPolicy: policy, Env: env, WorkingDir: "/tmp"}
	inst, err := e.Ensure(context.Background(), spec)
	if err != nil || inst.PID == 0 {
		t.Fatalf("start: %+v %v", inst, err)
	}
	t.Cleanup(func() { syscall.Kill(-inst.PID, syscall.SIGKILL) })
	return inst
}

func TestExecRunsInTheProgramsEnvironmentAndReportsExitCodes(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	runningNative(t, e, "x1", "sleep 300", "Never", map[string]string{"PROBE_VAR": "visible"})
	ex := e.(Execer)
	res, err := ex.Exec(context.Background(), "x1", []string{"/bin/sh", "-c", "echo $PROBE_VAR $MACCLUSTER_PORT; pwd -P"}, 3*time.Second)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "visible ") || !strings.Contains(res.Output, "/tmp") {
		t.Fatalf("env and directory of the program: %+v %v", res, err)
	}
	res, err = ex.Exec(context.Background(), "x1", []string{"/bin/sh", "-c", "echo bad >&2; exit 3"}, 3*time.Second)
	if err != nil || res.ExitCode != 3 || res.Output != "bad" {
		t.Fatalf("a failing command is a result, not an error: %+v %v", res, err)
	}
	if _, err = ex.Exec(context.Background(), "x1", []string{"/bin/sleep", "5"}, 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if _, err = ex.Exec(context.Background(), "nope", []string{"/bin/true"}, time.Second); err == nil {
		t.Fatal("an unknown or stopped workload cannot run commands")
	}
}

func TestKillKeepsTheWorkloadAndTheRestartPolicyDecides(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	inst := runningNative(t, e, "k1", "sleep 300", "Always", nil)
	if err := e.(Killer).Kill(context.Background(), "k1", "Liveness probe failed: test", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the process to end", func() bool { return !alive(inst.PID) })
	var seen Instance
	waitFor(t, "the exit to be recorded with the probe's reason", func() bool {
		for _, i := range e.List(context.Background()) {
			if i.UID == "k1" {
				seen = i
			}
		}
		return seen.LastExitReason == "Liveness probe failed: test"
	})
	if seen.Phase != "Failed" || seen.FailureCount != 1 {
		t.Fatalf("a killed container is a failed one: %+v", seen)
	}
	waitFor(t, "the Always policy to start it again", func() bool {
		i, _ := e.Ensure(context.Background(), Spec{UID: "k1", Name: "k1", Namespace: "ns", Executor: "native", Command: []string{"/bin/sh", "-c", "sleep 300"}, RestartPolicy: "Always", WorkingDir: "/tmp"})
		if i.PID != 0 && i.PID != inst.PID && i.Phase == "Running" {
			t.Cleanup(func() { syscall.Kill(-i.PID, syscall.SIGKILL) })
			return true
		}
		return false
	})
}

func TestKillEscalatesToSIGKILLAfterTheGracePeriod(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	inst := runningNative(t, e, "k2", `trap '' TERM; while true; do sleep 0.2; done`, "Never", nil)
	start := time.Now()
	e.(Killer).Kill(context.Background(), "k2", "r", time.Second)
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("a program that ignores TERM gets its grace period, then KILL: %s", elapsed)
	}
	waitFor(t, "the process to be gone", func() bool { return !alive(inst.PID) })
}

func TestStopGraceLetsTheProgramHandleTermAndDoesNotBlockTheEngine(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	inst := runningNative(t, e, "g1", `trap 'echo got-TERM; exit 0' TERM; echo started; while true; do sleep 0.2; done`, "Never", nil)
	waitFor(t, "the program to be ready", func() bool { l, _ := e.Logs(context.Background(), "g1"); return strings.Contains(l, "started") })
	done := make(chan struct{})
	go func() { e.(GraceStopper).StopGrace(context.Background(), "g1", 5*time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("a program that handles TERM should end well inside its grace period")
	}
	if logs, _ := e.Logs(context.Background(), "g1"); !strings.Contains(logs, "got-TERM") {
		t.Fatalf("the program must see SIGTERM, not SIGKILL: %q", logs)
	}
	if alive(inst.PID) {
		t.Fatal("still running")
	}

	// A program that ignores TERM keeps the stop waiting for its whole grace period; other workloads must not wait with it.
	stubborn := runningNative(t, e, "g2", `trap '' TERM; while true; do sleep 0.2; done`, "Never", nil)
	go e.(GraceStopper).StopGrace(context.Background(), "g2", 2*time.Second)
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	e.List(context.Background())
	if _, err := e.Ensure(context.Background(), Spec{UID: "g3", Name: "g3", Namespace: "ns", Executor: "native", Command: []string{"/bin/sh", "-c", "sleep 30"}, RestartPolicy: "Never"}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the engine lock was held while waiting for a stop: %s", elapsed)
	}
	waitFor(t, "the stubborn program to be killed after its grace period", func() bool { return !alive(stubborn.PID) })
	for _, i := range e.List(context.Background()) {
		if i.UID == "g3" {
			t.Cleanup(func() { syscall.Kill(-i.PID, syscall.SIGKILL) })
		}
	}
}
