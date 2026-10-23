package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const printSample = `gui/501/com.maccluster.pod.x = {
	active count = 0
	state = not running

	program = /bin/sh
	runs = 1
	last exit code = 7
	properties = {
		state = active
	}
}`

func TestParseLaunchdPrint(t *testing.T) {
	st := parseLaunchdPrint(printSample)
	if st.Running || st.PID != 0 || st.ExitCode == nil || *st.ExitCode != 7 || st.Signal != "" {
		t.Fatalf("exit: %+v", st)
	}
	st = parseLaunchdPrint("\tstate = running\n\tpid = 42\n\tlast exit code = (never exited)\n\t\tstate = active\n")
	if !st.Running || st.PID != 42 || st.ExitCode != nil {
		t.Fatalf("running: %+v", st)
	}
	st = parseLaunchdPrint("\tstate = not running\n\tlast terminating signal = Killed: 9\n")
	if st.Signal != "Killed: 9" || st.ExitCode != nil {
		t.Fatalf("signal: %+v", st)
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__launch" {
		os.Exit(RunLauncher(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestLaunchdPlistEscapesAndCarriesNoEnvironment(t *testing.T) {
	p := string(launchdPlist("l", "/bin/echo", []string{"a<b", `"&"`}, "/tmp", "/tmp/log"))
	for _, want := range []string{"<string>a&lt;b</string>", "&amp;", "<key>KeepAlive</key><false/>"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %s in %s", want, p)
		}
	}
	if strings.Contains(p, "EnvironmentVariables") {
		t.Fatal("the environment must travel in the private file, not the plist")
	}
}

func TestLaunchdKeepsMultiLineEnvironmentValues(t *testing.T) {
	e := launchdEngine(t, t.TempDir())
	ctx := context.Background()
	spec := nativeSpec("ld-env", `printf "%s" "$PEM" | wc -l | tr -d " "; echo "$ODD_NAME"; env | grep -c "^a-b.c=x" ; sleep 300`)
	spec.Env = map[string]string{"PEM": "line1\nline2\n", "ODD_NAME": "q'uo\"te $x", "a-b.c": "x"}
	if _, err := e.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "output", func() bool { l, _ := e.Logs(ctx, "ld-env"); return strings.Count(l, "\n") >= 3 })
	logs, _ := e.Logs(ctx, "ld-env")
	if !strings.HasPrefix(logs, "2\nq'uo\"te $x\n1\n") {
		t.Fatalf("environment was altered: %q", logs)
	}
	if left, _ := filepath.Glob(filepath.Join(e.(*engine).dir, "jobs", "*")); len(left) != 0 {
		t.Fatalf("environment or plist file left behind: %v", left)
	}
}

func TestBackendNameIsValidated(t *testing.T) {
	if _, err := NewWithOptions(t.TempDir(), "", Options{NativeBackend: "systemd"}); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

// launchdEngine skips when this machine has no GUI session to run jobs in.
func launchdEngine(t *testing.T, dir string) Engine {
	t.Helper()
	if os.Getuid() == 0 || checkLaunchdAvailable(Options{}) != nil {
		t.Skip("no launchd GUI domain for this user")
	}
	e, err := NewWithOptions(dir, "", Options{NativeBackend: "launchd"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ee := e.(*engine)
		for _, r := range ee.entries {
			if r.adoptPID > 0 {
				syscall.Kill(-r.adoptPID, syscall.SIGKILL)
			}
			ee.bootoutLaunchd(r)
			if r.gone != nil {
				// Let the exit watcher finish: it must not outlive the test (it reads package settings).
				select {
				case <-r.gone:
				case <-time.After(5 * time.Second):
				}
			}
		}
	})
	return e
}

func launchdLabels(t *testing.T, e Engine) []string {
	t.Helper()
	out, _ := exec.Command("launchctl", "list").Output()
	var labels []string
	prefix := launchdPrefix + e.(*engine).ownerTag() + "."
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 3 && strings.HasPrefix(f[2], prefix) {
			labels = append(labels, f[2])
		}
	}
	return labels
}

func TestLaunchdProgramRunsAsJobAndReportsExitCode(t *testing.T) {
	e := launchdEngine(t, t.TempDir())
	ctx := context.Background()
	inst, err := e.Ensure(ctx, nativeSpec("ld-exit", "echo from-job $MACCLUSTER_PORT; sleep 1; exit 7"))
	if err != nil || inst.PID == 0 || inst.Phase != "Running" {
		t.Fatalf("start: %+v %v", inst, err)
	}
	if ppid, _ := exec.Command("ps", "-o", "ppid=", "-p", itoa(inst.PID)).Output(); strings.TrimSpace(string(ppid)) != "1" {
		t.Fatalf("a launchd job must not be a child of the agent, ppid=%s", ppid)
	}
	waitFor(t, "failed phase", func() bool {
		i, _ := e.Ensure(ctx, nativeSpec("ld-exit", "echo from-job $MACCLUSTER_PORT; sleep 1; exit 7"))
		return i.Phase == "Failed" && i.ExitCode == 7
	})
	logs, err := e.Logs(ctx, "ld-exit")
	if err != nil || !strings.Contains(logs, "from-job ") {
		t.Fatalf("logs: %q %v", logs, err)
	}
	waitFor(t, "job removed", func() bool { return len(launchdLabels(t, e)) == 0 })
}

func TestLaunchdVeryShortProgramIsReported(t *testing.T) {
	e := launchdEngine(t, t.TempDir())
	ctx := context.Background()
	spec := nativeSpec("ld-short", "echo quick")
	if _, err := e.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "succeeded", func() bool { i, _ := e.Ensure(ctx, spec); return i.Phase == "Succeeded" })
	if logs, _ := e.Logs(ctx, "ld-short"); !strings.Contains(logs, "quick") {
		t.Fatalf("logs %q", logs)
	}
}

func TestLaunchdGracefulStopAndSignalReason(t *testing.T) {
	e := launchdEngine(t, t.TempDir())
	ctx := context.Background()
	spec := nativeSpec("ld-term", "trap 'echo got-term; exit 0' TERM; echo up; while :; do sleep 0.2; done")
	inst, err := e.Ensure(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "program up", func() bool { l, _ := e.Logs(ctx, "ld-term"); return strings.Contains(l, "up") })
	if err := e.(GraceStopper).StopGrace(ctx, "ld-term", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if alive(inst.PID) {
		t.Fatal("process survived a graceful stop")
	}
	if l, _ := e.Logs(ctx, "ld-term"); !strings.Contains(l, "got-term") {
		t.Fatalf("TERM was not delivered: %q", l)
	}
	if n := len(launchdLabels(t, e)); n != 0 {
		t.Fatalf("%d jobs left after stop", n)
	}

	// A kill that ends the process by signal is reported with that signal.
	spec = nativeSpec("ld-kill", "sleep 300")
	inst, err = e.Ensure(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	syscall.Kill(inst.PID, syscall.SIGKILL)
	var last Instance
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		last, _ = e.Ensure(ctx, spec)
		if last.Phase == "Failed" && strings.Contains(last.LastExitReason, "Killed") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(last.LastExitReason, "Killed") {
		t.Fatalf("a kill was not reported as a signal: %+v", last)
	}
}

func TestLaunchdProgramIsAdoptedAfterAgentCrash(t *testing.T) {
	dir := t.TempDir()
	e1 := launchdEngine(t, dir)
	ctx := context.Background()
	spec := nativeSpec("ld-adopt", "echo started; sleep 300")
	inst, err := e1.Ensure(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	abandon(t, e1)
	e2, err := NewWithOptions(dir, "", Options{NativeBackend: "launchd"})
	if err != nil {
		t.Fatal(err)
	}
	list := e2.List(ctx)
	if len(list) != 1 || list[0].PID != inst.PID || list[0].Phase != "Running" {
		t.Fatalf("not adopted: %+v", list)
	}
	if n := len(launchdLabels(t, e2)); n != 1 {
		t.Fatalf("the adopted job must survive the sweep, jobs=%d", n)
	}
	if err := e2.(GraceStopper).StopGrace(ctx, "ld-adopt", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "process gone", func() bool { return !alive(inst.PID) })
	waitFor(t, "job removed", func() bool { return len(launchdLabels(t, e2)) == 0 })
}

func TestLaunchdSweepRemovesOnlyUntrackedJobsOfThisState(t *testing.T) {
	dir := t.TempDir()
	e1 := launchdEngine(t, dir)
	ctx := context.Background()
	if _, err := e1.Ensure(ctx, nativeSpec("ld-orphan", "sleep 300")); err != nil {
		t.Fatal(err)
	}
	other := launchdEngine(t, t.TempDir())
	if _, err := other.Ensure(ctx, nativeSpec("ld-other", "sleep 300")); err != nil {
		t.Fatal(err)
	}
	// Lose the marker: the job is now one nobody tracks.
	abandon(t, e1)
	markers, _ := filepath.Glob(filepath.Join(dir, "*.native"))
	for _, m := range markers {
		os.Remove(m)
	}
	e2, err := NewWithOptions(dir, "", Options{NativeBackend: "launchd"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(launchdLabels(t, e2)); n != 0 {
		t.Fatalf("untracked job survived the sweep: %d", n)
	}
	if n := len(launchdLabels(t, other)); n != 1 {
		t.Fatalf("a job of another state directory was touched: %d", n)
	}
}

func TestLaunchdLogRotationKeepsWorking(t *testing.T) {
	old := logRotateAt
	logRotateAt = 4096
	defer func() { logRotateAt = old }()
	e := launchdEngine(t, t.TempDir())
	ctx := context.Background()
	spec := nativeSpec("ld-rotate", "i=0; while [ $i -lt 400 ]; do echo line-$i-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done; echo END; sleep 300")
	if _, err := e.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "end of output", func() bool { l, _ := e.Logs(ctx, "ld-rotate"); return strings.Contains(l, "END") })
	// The agent rotates what it has followed; the job must then keep appending at the start, not leave a hole.
	prev := logFilePath(e.(*engine).dir, "ld-rotate") + ".1"
	waitFor(t, "rotation", func() bool { _, err := os.Stat(prev); return err == nil })
	time.Sleep(600 * time.Millisecond)
	if logs, _ := e.Logs(ctx, "ld-rotate"); strings.Contains(logs, "\x00") {
		t.Fatal("the log has a hole: the job did not append after truncation")
	}
	// End the follower before the rotation threshold is restored.
	e.(GraceStopper).StopGrace(ctx, "ld-rotate", time.Second)
}

func itoa(n int) string { return strconv.Itoa(n) }
