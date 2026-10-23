package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExternalNativeKillHistorySurvivesRestartAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	api, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := api.(*engine)
	defer func() { e.Close(ctx) }()
	s := Spec{UID: "killed", Executor: "native", Command: []string{"/bin/sleep", "30"}, RestartPolicy: "Always"}
	first, err := e.Ensure(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var killed Instance
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		killed, err = e.Ensure(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if killed.Phase == "Failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if killed.Phase != "Failed" || killed.LastExitCode != -1 || !strings.Contains(killed.LastExitReason, "killed") || killed.LastExitAt == "" || killed.FailureCount != 1 {
		t.Fatalf("kill not recorded: %+v", killed)
	}
	e.mu.Lock()
	e.entries[s.UID].retry = time.Time{}
	e.mu.Unlock()
	restarted, err := e.Ensure(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Phase != "Running" || restarted.PID == first.PID || restarted.Restarts != 1 || restarted.LastExitAt != killed.LastExitAt || restarted.FailureCount != 1 {
		t.Fatalf("restart lost history: %+v", restarted)
	}
	path := filepath.Join(dir, key(s.UID)+".history.json")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("history permissions: %v %v", st, err)
	}
	if err = e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	api, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	e = api.(*engine)
	restored, err := e.Ensure(ctx, s)
	if err != nil || restored.FailureCount != 1 || restored.LastExitAt != killed.LastExitAt {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	if err = e.Stop(ctx, s.UID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("intentional stop retained history: %v", err)
	}
}

func TestNativeNormalExitHistory(t *testing.T) {
	api, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close(context.Background())
	s := Spec{UID: "success", Executor: "native", Command: []string{"/bin/sh", "-c", "exit 0"}, RestartPolicy: "Never"}
	if _, err = api.Ensure(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, err := api.Ensure(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		if v.Phase == "Succeeded" {
			if v.LastExitAt == "" || v.LastExitCode != 0 || v.LastExitReason != "Completed" || v.FailureCount != 0 {
				t.Fatalf("incorrect normal exit: %+v", v)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("normal exit not observed")
}

func TestDockerRemovalVersusOutageAndRecovery(t *testing.T) {
	ctx := context.Background()
	mode := filepath.Join(t.TempDir(), "mode")
	t.Setenv("DOCKER_TEST_MODE", mode)
	fakeDocker(t, `mode=$(cat "$DOCKER_TEST_MODE")
case "$1" in
 ps) [ "$mode" != outage ] || exit 1 ;;
 inspect)
 [ "$mode" = running ] || exit 1
 echo '[{"Id":"owned","State":{"Running":true,"ExitCode":0}}]' ;;
 create) echo owned ;;
 start) echo running > "$DOCKER_TEST_MODE"; echo owned ;;
 rm) [ "$mode" = running ] || exit 1 ;;
 *) exit 1 ;;
esac`)
	if err := os.WriteFile(mode, []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	api, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := api.(*engine)
	defer e.Close(ctx)
	s := Spec{UID: "docker-loss", Executor: "docker", Image: "example", RestartPolicy: "Always"}
	if _, err = e.Ensure(ctx, s); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(mode, []byte("outage"), 0600)
	v, err := e.Ensure(ctx, s)
	if err != nil || v.Phase != "Unknown" || v.ContainerID != "owned" || v.FailureCount != 0 || v.LastExitAt != "" {
		t.Fatalf("outage falsely exited: %+v %v", v, err)
	}
	os.WriteFile(mode, []byte("running"), 0600)
	v, err = e.Ensure(ctx, s)
	if err != nil || v.Phase != "Running" || v.FailureCount != 0 {
		t.Fatalf("outage recovery: %+v %v", v, err)
	}
	os.WriteFile(mode, []byte("missing"), 0600)
	v, err = e.Ensure(ctx, s)
	if err != nil || v.Phase != "Failed" || v.LastExitReason != "ContainerRemoved" || v.FailureCount != 1 || v.ContainerID != "" {
		t.Fatalf("missing undetected: %+v %v", v, err)
	}
	e.mu.Lock()
	e.entries[s.UID].retry = time.Time{}
	e.mu.Unlock()
	v, err = e.Ensure(ctx, s)
	if err != nil || v.Phase != "Running" || v.Restarts != 1 || v.LastExitReason != "ContainerRemoved" || v.FailureCount != 1 {
		t.Fatalf("recreate: %+v %v", v, err)
	}
	os.WriteFile(mode, []byte("outage"), 0600)
	if err = e.Stop(ctx, s.UID); err == nil {
		t.Fatal("stop claimed success during outage")
	}
	os.WriteFile(mode, []byte("missing"), 0600)
	if err = e.Stop(ctx, s.UID); err != nil {
		t.Fatalf("missing stop not idempotent: %v", err)
	}
	if err = e.Stop(ctx, s.UID); err != nil {
		t.Fatal(err)
	}
}

func TestDockerStartupOutageRecovers(t *testing.T) {
	mode := filepath.Join(t.TempDir(), "mode")
	t.Setenv("DOCKER_TEST_MODE", mode)
	os.WriteFile(mode, []byte("outage"), 0600)
	fakeDocker(t, `[ "$(cat "$DOCKER_TEST_MODE")" != outage ] || exit 1
case "$1" in
 ps) ;;
 create) echo owned ;;
 start) echo owned ;;
 inspect) echo '[{"Id":"owned","State":{"Running":true}}]' ;;
 rm) ;;
 *) exit 1 ;;
esac`)
	api, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer api.Close(ctx)
	e := api.(*engine)
	s := Spec{UID: "startup-outage", Executor: "docker", Image: "example", RestartPolicy: "Always"}
	v, err := e.Ensure(ctx, s)
	if err == nil || v.Phase != "Unknown" || v.LastExitAt != "" || v.FailureCount != 0 {
		t.Fatalf("startup outage counted exit: %+v %v", v, err)
	}
	os.WriteFile(mode, []byte("running"), 0600)
	e.mu.Lock()
	e.entries[s.UID].retry = time.Time{}
	e.mu.Unlock()
	v, err = e.Ensure(ctx, s)
	if err != nil || v.Phase != "Running" || v.FailureCount != 0 {
		t.Fatalf("startup recovery: %+v %v", v, err)
	}
}
