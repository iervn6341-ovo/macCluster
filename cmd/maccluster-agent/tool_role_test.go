package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLocalToolCannotStartOrQueryAgent(t *testing.T) {
	for _, command := range []string{"serve", "status", "netcheck", "join", "__launch", "run-node", "unknown"} {
		if validateToolCommand([]string{command}) == nil {
			t.Fatalf("local tool accepted %s", command)
		}
	}
	for _, command := range []string{"inventory", "env", "docker-logins", "docker-stats", "service"} {
		if err := validateToolCommand([]string{command}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSecondServeCannotReplaceLiveSocket(t *testing.T) {
	dir := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "agent.sock")
	if err = os.WriteFile(socket, []byte("live socket sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = serve(dir); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second serve: %v", err)
	}
	b, err := os.ReadFile(socket)
	if err != nil || string(b) != "live socket sentinel" {
		t.Fatalf("active socket was changed: %q, %v", b, err)
	}
}
