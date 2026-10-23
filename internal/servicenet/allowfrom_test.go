package servicenet

import (
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func mustNets(t *testing.T, list ...string) []*net.IPNet {
	t.Helper()
	nets, err := ParseAllowFrom(list)
	if err != nil {
		t.Fatal(err)
	}
	return nets
}

func TestParseAllowFrom(t *testing.T) {
	nets := mustNets(t, "192.0.2.0/24", " 203.0.113.7 ", "", "2001:db8::1", "2001:db8::/32")
	if len(nets) != 4 {
		t.Fatalf("%d entries", len(nets))
	}
	in := func(s string) bool {
		ip := net.ParseIP(s)
		for _, n := range nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	for ip, want := range map[string]bool{"192.0.2.82": true, "198.51.100.1": false, "203.0.113.7": true, "203.0.113.8": false, "2001:db8::1": true, "2001:db9::1": false} {
		if in(ip) != want {
			t.Errorf("%s: got %v", ip, !want)
		}
	}
	for _, bad := range []string{"not-an-ip", "192.0.2.0/33", "192.0.2/24", "192.0.2.0/"} {
		if _, err := ParseAllowFrom([]string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// roundTrip says whether data sent through the forwarded port comes back.
func roundTrip(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return false
	}
	buf := make([]byte, 4)
	_, err = io.ReadFull(c, buf)
	return err == nil && string(buf) == "ping"
}

func TestOnlyAllowedSourcesReachAnEntryPort(t *testing.T) {
	f := NewForwarder()
	defer f.Close()
	var logged atomic.Int32
	f.Logf = func(string, ...any) { logged.Add(1) }
	port, err := f.Ensure("pod/80", "127.0.0.1", echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	if !roundTrip(port) {
		t.Fatal("with no list everyone is allowed (the behavior before the list existed)")
	}
	// A list that does not contain the caller: closed at once, counted, no data.
	f.SetAllowFrom(mustNets(t, "192.0.2.0/24"))
	if roundTrip(port) {
		t.Fatal("a source outside the allow list was served")
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.Denied() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if f.Denied() == 0 {
		t.Fatal("the refusal was not counted")
	}
	// The same list applies to a route created later.
	port2, err := f.Ensure("pod/81", "127.0.0.1", echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip(port2) {
		t.Fatal("a route created after the list was set ignored it")
	}
	// Adding the caller's network opens both without recreating anything.
	f.SetAllowFrom(mustNets(t, "192.0.2.0/24", "127.0.0.0/8"))
	if !roundTrip(port) || !roundTrip(port2) {
		t.Fatal("an allowed source was refused")
	}
	// Refusals are logged once a minute per source, not once per attempt.
	f.SetAllowFrom(mustNets(t, "192.0.2.0/24"))
	for i := 0; i < 5; i++ {
		roundTrip(port)
	}
	time.Sleep(100 * time.Millisecond)
	if n := logged.Load(); n != 1 {
		t.Fatalf("logged %d times for one source", n)
	}
}

func TestChangingTheListDoesNotCutOpenConnections(t *testing.T) {
	f := NewForwarder()
	defer f.Close()
	port, err := f.Ensure("pod/80", "127.0.0.1", echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	// Make sure the connection has been accepted and is being proxied before the list changes: a connection that is
	// still waiting in the accept queue when the list changes is judged by the new list.
	c.Write([]byte("ping"))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("the connection did not work before the change: %v", err)
	}
	f.SetAllowFrom(mustNets(t, "192.0.2.0/24"))
	c.Write([]byte("pong"))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("an established connection was cut: %v", err)
	}
}
