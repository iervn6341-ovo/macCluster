package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseLeavesProgramsRunningAndTheNextEngineTakesThemOver(t *testing.T) {
	dir := t.TempDir()
	e1, spec, pid := startNative(t, dir, "rel-1", "i=0; while :; do echo tick-$i; i=$((i+1)); sleep 0.2; done")
	waitFor(t, "first output", func() bool { l, _ := e1.Logs(context.Background(), "rel-1"); return strings.Contains(l, "tick-1") })
	if err := e1.(Releaser).Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !alive(pid) {
		t.Fatal("Release stopped the program")
	}
	if _, err := os.Stat(filepath.Join(dir, key("rel-1")+".native")); err != nil {
		t.Fatalf("the marker must stay for the next engine: %v", err)
	}
	// The state directory is free again, the program keeps writing while nobody follows it.
	before, _ := os.ReadFile(logFilePath(dir, "rel-1"))
	time.Sleep(700 * time.Millisecond)
	after, _ := os.ReadFile(logFilePath(dir, "rel-1"))
	if len(after) <= len(before) {
		t.Fatal("the program stopped writing its log")
	}
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	inst, err := e2.Ensure(context.Background(), spec)
	if err != nil || inst.PID != pid || inst.Phase != "Running" || inst.Restarts != 0 {
		t.Fatalf("not taken over unchanged: %+v %v", inst, err)
	}
	if inst.AdoptNote != "" {
		t.Fatalf("the same spec must not produce a note: %q", inst.AdoptNote)
	}
	waitFor(t, "output after takeover", func() bool { l, _ := e2.Logs(context.Background(), "rel-1"); return strings.Contains(l, "tick-1") })
}

func TestReleaseIsIdempotentAndCloseStillStops(t *testing.T) {
	dir := t.TempDir()
	e, _, pid := startNative(t, dir, "rel-2", "sleep 300")
	rel := e.(Releaser)
	if rel.Release(context.Background()) != nil || rel.Release(context.Background()) != nil {
		t.Fatal("second Release failed")
	}
	if !alive(pid) {
		t.Fatal("stopped by Release")
	}
	// Close is still "stop everything" (leaving a cluster, draining).
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Ensure(context.Background(), nativeSpec("rel-2", "sleep 300")); err != nil {
		t.Fatal(err)
	}
	if err := e2.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "process stopped by Close", func() bool { return !alive(pid) })
}

func TestReleaseDoesNotTouchDockerContainers(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	fakeDocker(t, `echo "$@" >> `+calls+`
case "$1" in
 create) echo cid1 ;;
 start) echo cid1 ;;
 inspect) echo '[{"Id":"cid1","State":{"Running":true,"ExitCode":0,"StartedAt":"2026-10-19T00:00:00Z"},"Config":{"Labels":{}}}]' ;;
 *) exit 0 ;;
esac`)
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Ensure(context.Background(), Spec{UID: "d1", Name: "d1", Namespace: "ns", Executor: "docker", Image: "nginx", RestartPolicy: "Always"}); err != nil {
		t.Fatal(err)
	}
	if err := e.(Releaser).Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(calls)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "rm") || strings.HasPrefix(line, "stop") || strings.HasPrefix(line, "kill") {
			t.Fatalf("Release stopped a container: %q", line)
		}
	}
}

func TestReleaseLeavesLaunchdJobsRunning(t *testing.T) {
	dir := t.TempDir()
	e1 := launchdEngine(t, dir)
	spec := nativeSpec("rel-ld", "echo up; sleep 300")
	inst, err := e1.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.(Releaser).Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !alive(inst.PID) {
		t.Fatal("Release stopped the launchd job")
	}
	e2, err := NewWithOptions(dir, "", Options{NativeBackend: "launchd"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e2.Ensure(context.Background(), spec)
	if err != nil || got.PID != inst.PID || got.Phase != "Running" {
		t.Fatalf("job not taken over: %+v %v", got, err)
	}
	if n := len(launchdLabels(t, e2)); n != 1 {
		t.Fatalf("the sweep must keep the tracked job, jobs=%d", n)
	}
	if err := e2.(GraceStopper).StopGrace(context.Background(), "rel-ld", 3*time.Second); err != nil {
		t.Fatal(err)
	}
}
