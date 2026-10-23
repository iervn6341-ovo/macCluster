package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeLifecycle(t *testing.T) {
	dir := t.TempDir()
	e, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer e.Close(ctx)
	s := Spec{UID: "native-test", Executor: "native", Command: []string{"/bin/sh"}, Args: []string{"-c", "echo port=$MACCLUSTER_PORT arg=$1; exec sleep 30", "test", "$(MACCLUSTER_PORT)"}, RestartPolicy: "Never"}
	first, err := e.Ensure(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if first.PID <= 0 || first.Port <= 0 || first.Phase != "Running" {
		t.Fatalf("bad instance: %+v", first)
	}
	again, err := e.Ensure(ctx, s)
	if err != nil || again.PID != first.PID {
		t.Fatalf("not idempotent: %+v %v", again, err)
	}
	deadline := time.Now().Add(time.Second)
	var logs string
	for time.Now().Before(deadline) {
		logs, err = e.Logs(ctx, s.UID)
		if err == nil && strings.Contains(logs, "port=") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs, "arg=") || strings.Contains(logs, "$(MACCLUSTER_PORT)") {
		t.Fatalf("substitution/logs failed: %q", logs)
	}
	st, err := os.Stat(filepath.Join(dir, key(s.UID)+".log"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("log permissions: %v %v", st, err)
	}
	if err = e.Stop(ctx, s.UID); err != nil {
		t.Fatal(err)
	}
	if len(e.List(ctx)) != 0 {
		t.Fatal("instance remains after stop")
	}
	if err = e.Stop(ctx, s.UID); err != nil {
		t.Fatal(err)
	}
}
func TestNativeExitAndValidation(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer e.Close(ctx)
	for _, s := range []Spec{{UID: "bad", Executor: "other"}, {UID: "bad", Executor: "native", Command: []string{"sh"}}, {UID: "bad", Executor: "docker"}} {
		if _, err = e.Ensure(ctx, s); err == nil {
			t.Fatalf("accepted invalid spec %+v", s)
		}
	}
	s := Spec{UID: "exit", Executor: "native", Command: []string{"/bin/sh", "-c", "exit 7"}, RestartPolicy: "Never"}
	if _, err = e.Ensure(ctx, s); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list := e.List(ctx)
		if len(list) == 1 && list[0].Phase == "Failed" {
			if list[0].ExitCode != 7 {
				t.Fatalf("wrong exit code %+v", list[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("exit not observed")
}
func TestStateOwnershipAndCrashMarker(t *testing.T) {
	dir := t.TempDir()
	e, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := New(dir); err == nil {
		other.Close(context.Background())
		t.Fatal("duplicate owner allowed")
	}
	e.Close(context.Background())
	// An unverifiable launch record no longer stops the whole engine: it blocks only its own UID.
	os.WriteFile(filepath.Join(dir, "old.native"), []byte("pid=123 uid=legacy-uid"), 0600)
	other, err := New(dir)
	if err != nil {
		t.Fatalf("engine refused to start because of one unverifiable record: %v", err)
	}
	defer other.Close(context.Background())
	if _, err := other.Ensure(context.Background(), Spec{UID: "legacy-uid", Name: "x", Executor: "native", Command: []string{"/bin/sleep", "30"}}); err == nil {
		t.Fatal("a UID with an unverifiable launch record must not be started")
	}
	if _, err := other.Ensure(context.Background(), Spec{UID: "fresh-uid", Name: "y", Executor: "native", Command: []string{"/bin/sleep", "30"}}); err != nil {
		t.Fatalf("other UIDs must be unaffected: %v", err)
	}
}
func TestRestartBackoff(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer e.Close(ctx)
	s := Spec{UID: "restart", Executor: "native", Command: []string{"/bin/sh", "-c", "exit 2"}, RestartPolicy: "OnFailure"}
	e.Ensure(ctx, s)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		v, err := e.Ensure(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if v.Restarts > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("restart not attempted")
}

func TestBoundedLogs(t *testing.T) {
	b := &boundedBuffer{}
	b.Write([]byte(strings.Repeat("a", logLimit)))
	b.Write([]byte("tail"))
	if len(b.data) != logLimit || !strings.HasSuffix(string(b.data), "tail") {
		t.Fatal("Docker output bound failed")
	}
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := &boundedWriter{file: f}
	if _, err = w.Write([]byte(strings.Repeat("a", logLimit+50))); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil || st.Size() > logLimit {
		t.Fatalf("unbounded native logs: %v %v", st, err)
	}
}

func TestCancelledNativeOperations(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := Spec{UID: "cancel", Executor: "native", Command: []string{"/bin/sh", "-c", "trap '' TERM; exec sleep 30"}, RestartPolicy: "Never"}
	if _, err = e.Ensure(ctx, s); err == nil {
		t.Fatal("cancelled Ensure started process")
	}
	e.Stop(context.Background(), s.UID)
	v, err := e.Ensure(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = e.Stop(ctx, s.UID); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancelled stop failed to promptly kill process")
	}
	if err = syscall.Kill(v.PID, 0); err != syscall.ESRCH {
		t.Fatalf("owned process still exists: %v", err)
	}
}
func fakeDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}
func TestDockerRestartRefreshesState(t *testing.T) {
	fakeDocker(t, `case "$1" in
 start) echo owned ;;
 inspect) echo '[{"Id":"owned","State":{"Running":true,"ExitCode":0,"StartedAt":"2026-10-15T00:00:00Z"}}]' ;;
 *) exit 1 ;;
 esac`)
	e := &engine{}
	r := &entry{instance: Instance{ContainerID: "owned", Phase: "Failed", ExitCode: 7, Message: "old error", StartedAt: "old"}, exited: true}
	if err := e.startDocker(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if r.instance.Phase != "Running" || r.exited || r.instance.ExitCode != 0 || r.instance.Message != "" || r.instance.StartedAt == "old" {
		t.Fatalf("stale restart state: %+v", r.instance)
	}
}
func TestDockerRecoveryRejectsForeignOwner(t *testing.T) {
	fakeDocker(t, `case "$1" in
 ps) echo foreign ;;
 inspect) echo '[{"Id":"foreign","Config":{"Labels":{"maccluster.uid":"uid","maccluster.owner":"foreign-owner"}}}]' ;;
 *) exit 1 ;;
 esac`)
	e := &engine{dir: t.TempDir()}
	r := &entry{spec: Spec{UID: "uid", Executor: "docker", Image: "nginx:alpine"}}
	if err := e.startDocker(context.Background(), r); err == nil {
		t.Fatal("adopted foreign container")
	}
	if r.instance.ContainerID != "" {
		t.Fatal("foreign ID recorded")
	}
}

func TestNativeStopCleansProcessGroup(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	s := Spec{UID: "group", Executor: "native", Command: []string{"/bin/sh", "-c", "sleep 30 & echo ready; wait"}, RestartPolicy: "Never"}
	v, err := e.Ensure(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	ready := false
	for time.Now().Before(deadline) {
		out, _ := e.Logs(context.Background(), s.UID)
		if strings.Contains(out, "ready") {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("child not started")
	}
	if err = e.Stop(context.Background(), s.UID); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-v.PID, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned process group remains after stop")
}

func TestDockerListRecoversOwnedOrphans(t *testing.T) {
	dir := t.TempDir()
	e, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	marker := filepath.Join(t.TempDir(), "removed")
	t.Setenv("DOCKER_TEST_REMOVED", marker)
	fakeDocker(t, fmt.Sprintf(`case "$1" in
 ps) echo 'owned foreign' ;;
 inspect)
 if [ "$2" = owned ]; then
 echo '[{"Id":"owned","State":{"Running":true},"Config":{"Labels":{"maccluster.uid":"uid","maccluster.owner":"%s","maccluster.name":"pod","maccluster.namespace":"test"}}}]'
 else
 echo '[{"Id":"foreign","State":{"Running":true},"Config":{"Labels":{"maccluster.uid":"foreign-uid","maccluster.owner":"different-owner"}}}]'
 fi ;;
 rm) if [ "$3" != owned ]; then exit 99; fi; echo removed > "$DOCKER_TEST_REMOVED" ;;
 *) exit 1 ;;
 esac`, key(dir)))
	list := e.List(context.Background())
	if len(list) != 1 || list[0].UID != "uid" || list[0].Namespace != "test" {
		t.Fatalf("wrong recovered instances: %+v", list)
	}
	s := Spec{UID: "uid", Name: "pod", Namespace: "test", Executor: "docker", Image: "nginx:alpine", RestartPolicy: "Never"}
	v, err := e.Ensure(context.Background(), s)
	if err != nil || v.ContainerID != "owned" {
		t.Fatalf("spec adoption failed: %+v %v", v, err)
	}
	if err = e.Stop(context.Background(), "uid"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatal("owned orphan not deleted")
	}
	if len(e.List(context.Background())) != 0 {
		t.Fatal("foreign container tracked or orphan recovered again")
	}
}
func TestDockerRecoveryOptionalWhenUnavailable(t *testing.T) {
	fakeDocker(t, "exit 1\n")
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	if list := e.List(context.Background()); len(list) != 0 {
		t.Fatalf("unexpected instances: %+v", list)
	}
	s := Spec{UID: "native-optional", Executor: "native", Command: []string{"/bin/sleep", "30"}, RestartPolicy: "Never"}
	if _, err = e.Ensure(context.Background(), s); err != nil {
		t.Fatalf("Docker failure blocked native workload: %v", err)
	}
}

func TestDockerCreatePullProgressDoesNotCorruptID(t *testing.T) {
	fakeDocker(t, `case "$1" in
 ps) ;;
 create) echo 'Pulling image layers and verifying checksums...' >&2; echo 'owned' ;;
 start) [ "$2" = owned ] || exit 91; echo owned ;;
 inspect) [ "$2" = owned ] || exit 92; echo '[{"Id":"owned","State":{"Running":true,"ExitCode":0}}]' ;;
 rm) [ "$3" = owned ] || exit 93 ;;
 logs) echo stdout-message; echo stderr-message >&2 ;;
 *) exit 1 ;;
 esac`)
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	v, err := e.Ensure(context.Background(), Spec{UID: "pull", Executor: "docker", Image: "nginx:alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if v.ContainerID != "owned" || v.Phase != "Running" {
		t.Fatalf("pull output contaminated container state: %+v", v)
	}
	out, err := e.Logs(context.Background(), "pull")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "stdout-message") || !strings.Contains(out, "stderr-message") {
		t.Fatalf("missing workload log stream: %q", out)
	}
}

func TestDockerFailureMessageSurvivesStoppedInspect(t *testing.T) {
	e := &engine{}
	r := &entry{instance: Instance{Message: "start failed: invalid log configuration"}, exited: true}
	d := dockerInfo{}
	d.State.ExitCode = 1
	e.applyDocker(r, d)
	if r.instance.Message != "start failed: invalid log configuration" {
		t.Fatalf("start error lost: %+v", r.instance)
	}
	d.State.Error = "Docker daemon state error"
	e.applyDocker(r, d)
	if r.instance.Message != d.State.Error {
		t.Fatalf("daemon error hidden: %+v", r.instance)
	}
	d.State.Running = true
	d.State.Error = ""
	e.applyDocker(r, d)
	if r.instance.Message != "" {
		t.Fatalf("stale error after recovery: %+v", r.instance)
	}
}
