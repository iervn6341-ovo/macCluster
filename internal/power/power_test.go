package power

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func fakeKeeper(started *int) *Keeper {
	return &Keeper{Command: func(pid int) *exec.Cmd {
		*started++
		return exec.Command("sleep", "60")
	}}
}

func TestWhenWorkloadsHoldsOnlyWhileSomethingRuns(t *testing.T) {
	n := 0
	k := fakeKeeper(&n)
	defer k.Close()
	if k.Update(WhenWorkloads, 0) || k.Held() {
		t.Fatal("held with nothing running")
	}
	if !k.Update(WhenWorkloads, 2) || !k.Held() || n != 1 {
		t.Fatalf("not held with workloads running (started %d)", n)
	}
	if !k.Update("", 1) || n != 1 {
		t.Fatalf("the default mode must behave as when-workloads without a second holder (started %d)", n)
	}
	if k.Update(WhenWorkloads, 0) || k.Held() {
		t.Fatal("still held after the last workload stopped")
	}
}

func TestAlwaysAndNever(t *testing.T) {
	n := 0
	k := fakeKeeper(&n)
	defer k.Close()
	if !k.Update(Always, 0) {
		t.Fatal("always must hold with no workloads")
	}
	if k.Update(Never, 5) || k.Held() {
		t.Fatal("never must release and stay released")
	}
	if k.Update(Never, 5) || n != 1 {
		t.Fatalf("never started a holder: %d", n)
	}
}

func TestAHolderThatExitedIsReplaced(t *testing.T) {
	n := 0
	k := &Keeper{Command: func(pid int) *exec.Cmd { n++; return exec.Command("true") }}
	defer k.Close()
	k.Update(Always, 0)
	time.Sleep(200 * time.Millisecond)
	k.Update(Always, 0)
	if n != 2 {
		t.Fatalf("a dead holder was not replaced: started %d", n)
	}
}

func TestACommandThatCannotStartMeansNotHeld(t *testing.T) {
	k := &Keeper{Command: func(pid int) *exec.Cmd { return exec.Command("/nonexistent/caffeinate") }}
	if k.Update(Always, 1) || k.Held() {
		t.Fatal("reported held although the holder did not start")
	}
}

func TestModes(t *testing.T) {
	for _, ok := range []string{"", WhenWorkloads, Always, Never} {
		if !ValidMode(ok) {
			t.Fatalf("%q", ok)
		}
	}
	if ValidMode("sometimes") {
		t.Fatal("an unknown mode was accepted")
	}
}

// The real thing: while the keeper holds, macOS lists a PreventSystemSleep assertion from caffeinate.
func TestRealCaffeinateHoldsAnAssertion(t *testing.T) {
	if _, err := exec.LookPath("caffeinate"); err != nil {
		t.Skip("no caffeinate")
	}
	k := New()
	defer k.Close()
	if !k.Update(Always, 0) {
		t.Fatal("did not start")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("pmset", "-g", "assertions").Output()
		if strings.Contains(string(out), "caffeinate") && strings.Contains(string(out), "PreventSystemSleep") {
			k.Update(Never, 0)
			time.Sleep(500 * time.Millisecond)
			out, _ = exec.Command("pmset", "-g", "assertions").Output()
			for _, l := range strings.Split(string(out), "\n") {
				if strings.Contains(l, "caffeinate") && strings.Contains(l, "-s") {
					t.Fatalf("the assertion outlived Release: %s", l)
				}
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no caffeinate assertion appeared in pmset")
}
