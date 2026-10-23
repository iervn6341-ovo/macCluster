package servicenet

import (
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-closed; workers.Wait() })
	return ln.Addr().(*net.TCPAddr).Port
}

func dialForwarded(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

func assertEcho(t *testing.T, c net.Conn) {
	t.Helper()
	message := "forwarded TCP payload\x00\xff"
	if _, err := io.WriteString(c, message); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != message {
		t.Fatalf("received %q", got)
	}
}

func assertConnectionClosed(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	_, err := c.Read(b[:])
	if err == nil {
		t.Fatal("connection remained open")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("connection was not closed: %v", err)
	}
}

func TestForwarderTCPRetainAndClose(t *testing.T) {
	target := echoServer(t)
	f := NewForwarder()
	t.Cleanup(f.Close)
	kept, err := f.Ensure("keep", "127.0.0.1", target)
	if err != nil {
		t.Fatal(err)
	}
	dropped, err := f.Ensure("drop", "127.0.0.1", target)
	if err != nil {
		t.Fatal(err)
	}
	same, err := f.Ensure("keep", "127.0.0.1", target)
	if err != nil || same != kept {
		t.Fatalf("Ensure changed stable route: %d %v", same, err)
	}
	keepConn, dropConn := dialForwarded(t, kept), dialForwarded(t, dropped)
	assertEcho(t, keepConn)
	assertEcho(t, dropConn)
	f.Retain(map[string]bool{"keep": true})
	assertConnectionClosed(t, dropConn)
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(dropped)), time.Second)
	if err == nil {
		c.Close()
		t.Fatal("removed listener still accepts connections")
	}
	assertEcho(t, keepConn)
	f.Close()
	assertConnectionClosed(t, keepConn)
	f.Close() // Cleanup is idempotent.
}

func TestForwarderReplacesChangedTargetAndSupportsHalfClose(t *testing.T) {
	first, second := echoServer(t), echoServer(t)
	f := NewForwarder()
	t.Cleanup(f.Close)
	port, err := f.Ensure("route", "127.0.0.1", first)
	if err != nil {
		t.Fatal(err)
	}
	old := dialForwarded(t, port)
	assertEcho(t, old)
	port, err = f.Ensure("route", "127.0.0.1", second)
	if err != nil {
		t.Fatal(err)
	}
	assertConnectionClosed(t, old)
	c := dialForwarded(t, port)
	payload := "request completed with write shutdown"
	if _, err := io.WriteString(c, payload); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("half-close truncated payload: %q", got)
	}
}

func TestForwarderRejectsInvalidAddressAndPort(t *testing.T) {
	f := NewForwarder()
	defer f.Close()
	for _, tc := range []struct {
		ip   string
		port int
	}{{"", 80}, {"not-an-IP", 80}, {"0.0.0.0", 80}, {"::", 80}, {"224.0.0.1", 80}, {"127.0.0.1", 0}, {"127.0.0.1", 65536}} {
		if _, err := f.Ensure("invalid", tc.ip, tc.port); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
}
