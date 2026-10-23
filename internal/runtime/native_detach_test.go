package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// abandon simulates an agent crash: the state directory is released but nothing is stopped.
func abandon(t *testing.T, e Engine) {
	t.Helper()
	ee := e.(*engine)
	ee.lock.Close()
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nativeSpec(uid, script string) Spec {
	return Spec{UID: uid, Name: uid, Namespace: "ns", Executor: "native", Command: []string{"/bin/sh", "-c", script}, RestartPolicy: "Never"}
}

func startNative(t *testing.T, dir, uid, script string) (Engine, Spec, int) {
	t.Helper()
	e, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	spec := nativeSpec(uid, script)
	inst, err := e.Ensure(context.Background(), spec)
	if err != nil || inst.PID == 0 {
		t.Fatalf("start: %+v %v", inst, err)
	}
	t.Cleanup(func() { syscall.Kill(-inst.PID, syscall.SIGKILL) })
	return e, spec, inst.PID
}

func TestAdoptsNativeProcessAfterAgentCrash(t *testing.T) {
	dir := t.TempDir()
	e1, spec, pid := startNative(t, dir, "adopt-1", "echo started; sleep 300")
	abandon(t, e1)

	e2, err := New(dir)
	if err != nil {
		t.Fatalf("engine must start with a verifiable record present: %v", err)
	}
	defer e2.Close(context.Background())
	list := e2.List(context.Background())
	if len(list) != 1 || list[0].PID != pid || list[0].Phase != "Running" {
		t.Fatalf("process not recognised: %+v", list)
	}
	inst, err := e2.Ensure(context.Background(), spec)
	if err != nil || inst.PID != pid || inst.Phase != "Running" {
		t.Fatalf("Ensure must keep the adopted process, not start another: %+v %v", inst, err)
	}
	logs, err := e2.Logs(context.Background(), "adopt-1")
	if err != nil || !strings.Contains(logs, "started") {
		t.Fatalf("log of the earlier run lost: %q %v", logs, err)
	}
	if err := e2.Stop(context.Background(), "adopt-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "adopted process to stop", func() bool { return !alive(pid) })
	if _, err := os.Stat(filepath.Join(dir, key("adopt-1")+".native")); !os.IsNotExist(err) {
		t.Fatal("marker must go with the process")
	}
}

func TestLogsWrittenWhileTheAgentWasAwayAreKept(t *testing.T) {
	dir := t.TempDir()
	e1, _, _ := startNative(t, dir, "adopt-logs", "while true; do echo tick; sleep 0.1; done")
	abandon(t, e1)
	logPath := logFilePath(dir, "adopt-logs")
	waitFor(t, "detached process to write log output", func() bool {
		data, err := os.ReadFile(logPath)
		return err == nil && strings.Count(string(data), "tick") >= 8
	})
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	logs, err := e2.Logs(context.Background(), "adopt-logs")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs, "tick"); n < 8 {
		t.Fatalf("output written while detached is missing (%d lines)", n)
	}
	_ = e2.Stop(context.Background(), "adopt-logs")
}

func TestAdoptedProcessExitIsDetected(t *testing.T) {
	dir := t.TempDir()
	e1, _, pid := startNative(t, dir, "adopt-exit", "sleep 300")
	abandon(t, e1)
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	syscall.Kill(-pid, syscall.SIGKILL)
	waitFor(t, "exit of the adopted process", func() bool {
		l := e2.List(context.Background())
		return len(l) == 1 && l[0].Phase == "Failed" && l[0].LastExitReason == "ExitedWithUnknownStatus"
	})
}

func TestReusedPIDIsNotAdoptedOrKilled(t *testing.T) {
	dir := t.TempDir()
	e1, spec, pid := startNative(t, dir, "adopt-reuse", "sleep 300")
	abandon(t, e1)
	path := filepath.Join(dir, key("adopt-reuse")+".native")
	var m nativeMarker
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &m); err != nil || m.PID != pid || m.Start == "" || m.Command == "" {
		t.Fatalf("marker lacks process identity: %s %v", data, err)
	}
	m.Start = "Mon Jan  1 00:00:00 2001" // the PID now "belongs" to a different process
	if err := writeMarker(path, m); err != nil {
		t.Fatal(err)
	}
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	if l := e2.List(context.Background()); len(l) != 0 {
		t.Fatalf("a process with a different identity was adopted: %+v", l)
	}
	if !alive(pid) {
		t.Fatal("a process that is not ours must be left alone")
	}
	inst, err := e2.Ensure(context.Background(), spec)
	if err != nil || inst.PID == pid || inst.Phase != "Running" {
		t.Fatalf("a fresh process must be started: %+v %v", inst, err)
	}
}

func TestCommandLineChangeAfterStartDoesNotPreventAdoption(t *testing.T) {
	// Launchers like /usr/bin/python3 re-exec the real interpreter, so the command line seen at start differs
	// from the one seen later. Identity is PID + start time, not the command line.
	dir := t.TempDir()
	e1, spec, pid := startNative(t, dir, "adopt-exec", "sleep 300")
	abandon(t, e1)
	path := filepath.Join(dir, key("adopt-exec")+".native")
	var m nativeMarker
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.Command = "/usr/bin/python3 -u -m http.server 1 (as recorded before the launcher re-executed)"
	if err := writeMarker(path, m); err != nil {
		t.Fatal(err)
	}
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	inst, err := e2.Ensure(context.Background(), spec)
	if err != nil || inst.PID != pid {
		t.Fatalf("a process whose command line changed must still be adopted: %+v %v", inst, err)
	}
}

func TestDeadProcessMarkerIsForgotten(t *testing.T) {
	dir := t.TempDir()
	e1, spec, pid := startNative(t, dir, "adopt-dead", "sleep 300")
	abandon(t, e1)
	syscall.Kill(-pid, syscall.SIGKILL)
	waitFor(t, "process to die", func() bool { return !alive(pid) || true })
	time.Sleep(300 * time.Millisecond)
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	if l := e2.List(context.Background()); len(l) != 0 {
		t.Fatalf("dead process listed: %+v", l)
	}
	inst, err := e2.Ensure(context.Background(), spec)
	if err != nil || inst.Phase != "Running" || inst.PID == pid {
		t.Fatalf("service must simply start again: %+v %v", inst, err)
	}
}

func TestAdoptedProcessWithDifferentSpecIsTakenOverWithANote(t *testing.T) {
	dir := t.TempDir()
	e1, _, pid := startNative(t, dir, "adopt-spec", "sleep 300")
	abandon(t, e1)
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	inst, err := e2.Ensure(context.Background(), nativeSpec("adopt-spec", "sleep 301"))
	if err != nil {
		t.Fatalf("an upgrade must not orphan the running program: %v", err)
	}
	if inst.PID != pid || inst.Phase != "Running" || !strings.Contains(inst.AdoptNote, "differs") {
		t.Fatalf("expected adoption with a note: %+v", inst)
	}
	if !alive(pid) {
		t.Fatal("the running process must not be touched")
	}
}

func TestMarkersOfOlderVersionsAreStillRead(t *testing.T) {
	s := nativeSpec("x", "sleep 1")
	if !fingerprintMatches(specHash(s), s) || fingerprintMatches(specHash(s), nativeSpec("x", "sleep 2")) {
		t.Fatal("the old whole-spec hash must still compare")
	}
	if !fingerprintMatches(specFingerprint(s), s) || fingerprintMatches(specFingerprint(s), nativeSpec("x", "sleep 2")) {
		t.Fatal("v2 fingerprint")
	}
	// A field outside the stable set does not change the fingerprint.
	other := s
	other.Namespace, other.RestartPolicy, other.Name = "other", "Always", "renamed"
	if specFingerprint(other) != specFingerprint(s) {
		t.Fatal("unrelated fields must not change the fingerprint")
	}
}

func TestIntentOnlyRecordBlocksOnlyThatUID(t *testing.T) {
	dir := t.TempDir()
	if err := writeMarker(filepath.Join(dir, key("blocked")+".native"), nativeMarker{UID: "blocked", Intent: true}); err != nil {
		t.Fatal(err)
	}
	e, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	if _, err := e.Ensure(context.Background(), nativeSpec("blocked", "sleep 30")); err == nil {
		t.Fatal("must not start a UID whose launch may have succeeded")
	}
	if inst, err := e.Ensure(context.Background(), nativeSpec("other", "sleep 30")); err != nil || inst.Phase != "Running" {
		t.Fatalf("other UIDs unaffected: %+v %v", inst, err)
	}
	if err := e.Stop(context.Background(), "blocked"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, key("blocked")+".native")); !os.IsNotExist(err) {
		t.Fatal("deleting the workload must clear the record")
	}
}

func TestLogRotationKeepsPreviousGenerationAndHistoryIsReadable(t *testing.T) {
	old := logRotateAt
	logRotateAt = 2048
	defer func() { logRotateAt = old }()
	dir := t.TempDir()
	script := `i=0; while [ $i -lt 60 ]; do echo "line-$i-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"; i=$((i+1)); sleep 0.05; done; sleep 300`
	e, _, _ := startNative(t, dir, "rot", script)
	waitFor(t, "log rotation", func() bool {
		_, err := os.Stat(logFilePath(dir, "rot") + ".1")
		return err == nil
	})
	waitFor(t, "script to finish writing", func() bool {
		logs, _ := e.Logs(context.Background(), "rot")
		return strings.Contains(logs, "line-59-")
	})
	logs, _ := e.Logs(context.Background(), "rot")
	live, _ := os.ReadFile(logFilePath(dir, "rot"))
	prev, _ := os.ReadFile(logFilePath(dir, "rot") + ".1")
	if len(prev) == 0 || !strings.HasPrefix(logs, string(prev)) || !strings.HasSuffix(logs, string(live)) || len(logs) <= len(live) {
		t.Fatalf("history must be the previous generation followed by the live file: logs=%d prev=%d live=%d", len(logs), len(prev), len(live))
	}
	if err := e.Stop(context.Background(), "rot"); err != nil {
		t.Fatal(err)
	}
	// Still readable when the service is stopped and after the agent restarted without re-creating it.
	e.Close(context.Background())
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	if logs, err := e2.Logs(context.Background(), "rot"); err != nil || !strings.Contains(logs, "line-59-") {
		t.Fatalf("history not readable without a live entry: %v", err)
	}
	if _, err := e2.Logs(context.Background(), "no-such-uid"); err == nil {
		t.Fatal("a UID without any log must stay unknown")
	}
}
