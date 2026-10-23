package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestServiceLifecycle(t *testing.T) {
	missing := exec.Command("/bin/sh", "-c", "exit 113").Run()
	for _, tc := range []struct {
		action string
		loaded bool
		want   []string
	}{
		{"start", false, []string{"print", "bootstrap"}},
		{"start", true, []string{"print", "kickstart"}},
		{"stop", true, []string{"bootout", "print"}},
		{"restart", true, []string{"bootout", "print", "bootstrap"}},
		{"status", true, []string{"print"}},
	} {
		for _, extra := range [][]string{nil, {"--state-dir", filepath.Join(t.TempDir(), "legacy-state")}} {
			suffix := ""
			if len(extra) != 0 {
				suffix = "legacy-state-dir-"
			}
			t.Run(suffix+tc.action+map[bool]string{true: "-loaded", false: "-unloaded"}[tc.loaded], func(t *testing.T) {
				home := t.TempDir()
				plist := filepath.Join(home, "Library", "LaunchAgents", agentServiceLabel+".plist")
				if err := os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(plist, []byte("existing config"), 0600); err != nil {
					t.Fatal(err)
				}
				var calls []string
				loaded := tc.loaded
				run := func(args ...string) ([]byte, error) {
					calls = append(calls, args[0])
					if args[0] == "bootout" {
						loaded = false
					}
					if args[0] == "print" && !loaded {
						return nil, missing
					}
					if args[0] == "bootstrap" {
						if !reflect.DeepEqual(args, []string{"bootstrap", "gui/123", plist}) {
							t.Fatalf("wrong bootstrap: %v", args)
						}
					} else if len(args) != 2 || args[1] != "gui/123/"+agentServiceLabel {
						t.Fatalf("wrong job: %v", args)
					}
					return []byte("running\n"), nil
				}
				if err := manageService(append([]string{tc.action}, extra...), home, "/bin/echo", "gui/123", io.Discard, run); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(calls, tc.want) {
					t.Fatalf("calls %v, want %v", calls, tc.want)
				}
			})
		}
	}
}

func TestServiceAbsentJobCanStopAndRestart(t *testing.T) {
	absent := exec.Command("/bin/sh", "-c", "exit 3").Run()
	missing := exec.Command("/bin/sh", "-c", "exit 113").Run()
	for _, action := range []string{"stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			home := t.TempDir()
			plist := filepath.Join(home, "Library", "LaunchAgents", agentServiceLabel+".plist")
			if err := os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plist, []byte("installed"), 0600); err != nil {
				t.Fatal(err)
			}
			var calls []string
			run := func(args ...string) ([]byte, error) {
				calls = append(calls, args[0])
				switch args[0] {
				case "bootout":
					return nil, absent
				case "print":
					return nil, missing
				}
				return nil, nil
			}
			if err := manageService([]string{action}, home, "/bin/echo", "gui/123", io.Discard, run); err != nil {
				t.Fatal(err)
			}
			want := []string{"bootout", "print"}
			if action == "restart" {
				want = append(want, "bootstrap")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("%v", calls)
			}
		})
	}
}

func TestServiceStopWaitsForJobRemoval(t *testing.T) {
	missing := exec.Command("/bin/sh", "-c", "exit 113").Run()
	lookups := 0
	run := func(args ...string) ([]byte, error) {
		if args[0] == "print" {
			lookups++
			if lookups > 1 {
				return nil, missing
			}
		}
		return nil, nil
	}
	if err := manageService([]string{"stop"}, t.TempDir(), "/bin/echo", "gui/123", io.Discard, run); err != nil {
		t.Fatal(err)
	}
	if lookups != 2 {
		t.Fatalf("lookups=%d", lookups)
	}
}

func TestServiceNoProcessDoesNotHideLoadedOrUninspectableJob(t *testing.T) {
	absent := exec.Command("/bin/sh", "-c", "exit 3").Run()
	for _, lookup := range []error{nil, context.DeadlineExceeded} {
		calls := 0
		run := func(args ...string) ([]byte, error) {
			calls++
			if args[0] == "bootout" {
				return nil, absent
			}
			return nil, lookup
		}
		if err := manageService([]string{"stop"}, t.TempDir(), "/bin/echo", "gui/123", io.Discard, run); err == nil {
			t.Fatal("unexpected success")
		}
		if calls != 2 {
			t.Fatalf("calls=%d", calls)
		}
	}
}

func TestServiceRestartRetriesOnlyRetiringJob(t *testing.T) {
	missing := exec.Command("/bin/sh", "-c", "exit 113").Run()
	retiring := exec.Command("/bin/sh", "-c", "exit 5").Run()
	for _, lookup := range []struct {
		name string
		err  error
		want int
	}{
		{"missing", missing, 2}, {"loaded", nil, 2}, {"lookup-timeout", context.DeadlineExceeded, 1},
	} {
		t.Run(lookup.name, func(t *testing.T) {
			home := t.TempDir()
			plist := filepath.Join(home, "Library", "LaunchAgents", agentServiceLabel+".plist")
			if err := os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plist, []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			run := func(args ...string) ([]byte, error) {
				switch args[0] {
				case "bootstrap":
					attempts++
					if attempts == 1 {
						return nil, retiring
					}
				case "print":
					if attempts == 0 {
						return nil, missing
					}
					return nil, lookup.err
				}
				return nil, nil
			}
			err := manageService([]string{"restart"}, home, "/bin/echo", "gui/123", io.Discard, run)
			if attempts != lookup.want || (err == nil) != (lookup.want == 2) {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
}

func TestServiceFailuresDoNotMutateJob(t *testing.T) {
	for _, action := range []string{"install", "start", "restart"} {
		t.Run(action, func(t *testing.T) {
			home := t.TempDir()
			plist := filepath.Join(home, "Library", "LaunchAgents", agentServiceLabel+".plist")
			os.MkdirAll(filepath.Dir(plist), 0700)
			os.WriteFile(plist, []byte("original"), 0600)
			var calls []string
			run := func(args ...string) ([]byte, error) {
				calls = append(calls, args[0])
				return nil, context.DeadlineExceeded
			}
			err := manageService([]string{action}, home, "/bin/echo", "gui/123", io.Discard, run)
			if !errors.Is(err, context.DeadlineExceeded) || len(calls) != 1 {
				t.Fatalf("%v %v", calls, err)
			}
			data, _ := os.ReadFile(plist)
			if string(data) != "original" {
				t.Fatal("configuration changed on failure")
			}
		})
	}
}

func TestServiceInstall(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "state & <data>")
	missing := exec.Command("/bin/sh", "-c", "exit 113").Run()
	run := func(args ...string) ([]byte, error) {
		if args[0] != "print" {
			t.Fatalf("install started service: %v", args)
		}
		return nil, missing
	}
	if err := manageService([]string{"install", "--state-dir", dir}, home, "/bin/echo", "gui/123", io.Discard, run); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", agentServiceLabel+".plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "state &amp; &lt;data&gt;") || !strings.Contains(string(data), "<key>AbandonProcessGroup</key><true/>") {
		t.Fatalf("invalid config: %s", data)
	}
	info, _ := os.Stat(plist)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", info.Mode())
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("/usr/bin/plutil", "-lint", plist).CombinedOutput(); err != nil {
			t.Fatalf("plist invalid: %s %v", out, err)
		}
	}
	loaded := func(...string) ([]byte, error) { return []byte("running"), nil }
	if err := manageService([]string{"install"}, home, "/bin/echo", "gui/123", io.Discard, loaded); err == nil {
		t.Fatal("replaced loaded service")
	}
}
