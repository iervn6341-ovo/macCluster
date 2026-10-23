package runtime

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestParseListening(t *testing.T) {
	out := "p100\nn*:8100\nn[::]:8100\nn127.0.0.1:9100\nn[::1]:49557\nn192.0.2.10:8100\nnnot a socket\np101\nn*:7777\n"
	got := parseListening(out, []int{8100, 9100, 6000})
	want := []Listener{{Port: 8100, Addresses: []string{"*", "192.0.2.10", "::"}}, {Port: 9100, Addresses: []string{"127.0.0.1"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if parseListening("", []int{1}) != nil {
		t.Fatal("nothing listening")
	}
	if !listeningComplete(got, []int{8100, 9100}) || listeningComplete(got, []int{8100, 9100, 6000}) || !listeningComplete(nil, nil) {
		t.Fatal("completeness")
	}
}

func listenerSpec(uid string, port int, bind string, delay int) Spec {
	script := fmt.Sprintf("sleep %d; exec python3 -m http.server %d --bind %s", delay, port, bind)
	s := nativeSpec(uid, script)
	s.FixedPorts = []int{port}
	return s
}

func quickDiscovery(t *testing.T) {
	t.Helper()
	oldFirst, oldFast := listenerFirstDelay, listenerFastEvery
	listenerFirstDelay, listenerFastEvery = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { listenerFirstDelay, listenerFastEvery = oldFirst, oldFast })
}

func listeningOf(e Engine, uid string) []Listener {
	for _, i := range e.List(context.Background()) {
		if i.UID == uid {
			return i.Listening
		}
	}
	return nil
}

func TestTheAddressAProgramListensOnIsFoundOnceAndRemembered(t *testing.T) {
	quickDiscovery(t)
	loop, wide := freePort(t), freePort(t)
	// Keep this test deterministic on hosted runners where lsof may have different permissions or PATH settings.
	oldLsof := lsofListening
	lsofListening = func([]int) string {
		return fmt.Sprintf("p1\nn127.0.0.1:%d\np2\nn*:%d\n", loop, wide)
	}
	t.Cleanup(func() { lsofListening = oldLsof })
	dir := t.TempDir()
	e, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Spec{listenerSpec("lis-loop", loop, "127.0.0.1", 30), listenerSpec("lis-wide", wide, "0.0.0.0", 30)} {
		inst, err := e.Ensure(context.Background(), s)
		if err != nil || inst.PID == 0 {
			t.Fatalf("%+v %v", inst, err)
		}
		t.Cleanup(func() { syscallKill(inst.PID) })
	}
	waitFor(t, "both programs to be found", func() bool {
		return len(listeningOf(e, "lis-loop")) == 1 && len(listeningOf(e, "lis-wide")) == 1
	})
	if l := listeningOf(e, "lis-loop")[0]; l.Port != loop || strings.Join(l.Addresses, ",") != "127.0.0.1" {
		t.Fatalf("loopback program: %+v", l)
	}
	if l := listeningOf(e, "lis-wide")[0]; l.Port != wide || !contains(l.Addresses, "*") {
		t.Fatalf("all-interfaces program: %+v", l)
	}
	// It is remembered in the marker: a new engine that takes the programs over knows without asking lsof again.
	waitFor(t, "the answer to be stored", func() bool {
		m, _, ok := readMarker(e.(*engine).entries["lis-wide"].marker)
		return ok && len(m.Listeners) == 1
	})
	abandon(t, e)
	var asked atomic.Bool
	lsofListening = func([]int) string { asked.Store(true); return "" }
	e2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close(context.Background())
	time.Sleep(600 * time.Millisecond)
	if l := listeningOf(e2, "lis-wide"); len(l) != 1 || !contains(l[0].Addresses, "*") {
		t.Fatalf("not remembered after the agent restarted: %+v", l)
	}
	if asked.Load() {
		t.Fatal("lsof was asked again for a program whose answer was already remembered")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestAProgramThatNeverListensIsGivenUpOn(t *testing.T) {
	quickDiscovery(t)
	oldGive := listenerGiveUp
	listenerGiveUp = 700 * time.Millisecond
	defer func() { listenerGiveUp = oldGive }()
	var calls atomic.Int32
	oldLsof := lsofListening
	lsofListening = func([]int) string { calls.Add(1); return "" }
	defer func() { lsofListening = oldLsof }()
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := nativeSpec("lis-never", "sleep 30")
	s.FixedPorts = []int{freePort(t)}
	inst, err := e.Ensure(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscallKill(inst.PID); e.Close(context.Background()) })
	time.Sleep(1500 * time.Millisecond)
	before := calls.Load()
	time.Sleep(800 * time.Millisecond)
	if calls.Load() != before || before == 0 || len(listeningOf(e, "lis-never")) != 0 {
		t.Fatalf("lsof calls %d then %d; listening %v", before, calls.Load(), listeningOf(e, "lis-never"))
	}
}

func TestNoQuestionsAreAskedForAProgramWithoutDeclaredPorts(t *testing.T) {
	// A dynamic port is declared too (MACCLUSTER_PORT), so discovery runs for it; nothing runs without any port.
	var r entry
	e := &engine{}
	e.startListenerDiscovery(&r, 0, []int{1}, make(chan struct{}))
	e.startListenerDiscovery(&r, 5, nil, make(chan struct{}))
}

func syscallKill(pid int) { syscall.Kill(-pid, syscall.SIGKILL) }
