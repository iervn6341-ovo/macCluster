package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLsofFilesKeepsOnlyRegularFiles(t *testing.T) {
	out := "p100\nf1\ntREG\nn/tmp/a.log\nf2\ntREG\nn/tmp/a.log\np101\nf1\ntPIPE\nn->0xabc\nf2\ntCHR\nn/dev/null\np102\nf1\ntREG\nn/tmp/b.log\n"
	got := parseLsofFiles(out)
	if len(got) != 2 || got[0] != "/tmp/a.log" || got[1] != "/tmp/b.log" {
		t.Fatalf("got %v", got)
	}
}

func redirectService(script string) Spec {
	return Spec{UID: "redir", Name: "redir", Namespace: "ns", Executor: "native", Command: []string{"/bin/sh", "-c", script}, RestartPolicy: "Never"}
}

func logsContain(t *testing.T, e Engine, uid string, want ...string) func() bool {
	return func() bool {
		l, _ := e.Logs(context.Background(), uid)
		for _, w := range want {
			if !strings.Contains(l, w) {
				return false
			}
		}
		return true
	}
}

// Both backends: the program writes to a file it opened itself (the vllm case), not to the agent's log.
func runRedirectTests(t *testing.T, newEngine func(t *testing.T, dir string) Engine) {
	t.Run("fresh file is followed from its first line", func(t *testing.T) {
		e := newEngine(t, t.TempDir())
		f := filepath.Join(t.TempDir(), "out.log")
		real, _ := filepath.EvalSymlinks(filepath.Dir(f)) // lsof reports the resolved path (/private/var/...)
		script := `(i=0; while [ $i -lt 200 ]; do echo line-$i; i=$((i+1)); sleep 0.1; done) >> "` + f + `" 2>&1; sleep 60`
		if _, err := e.Ensure(context.Background(), redirectService(script)); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "early and later lines", logsContain(t, e, "redir", "line-0\n", "line-12\n", "following output redirected to "+filepath.Join(real, "out.log")))
	})
	t.Run("an existing file contributes only what is new", func(t *testing.T) {
		e := newEngine(t, t.TempDir())
		f := filepath.Join(t.TempDir(), "old.log")
		os.WriteFile(f, []byte("OLD-CONTENT-FROM-BEFORE\n"), 0600)
		time.Sleep(2500 * time.Millisecond) // older than the slack that counts a file as created by this run
		script := `(i=0; while [ $i -lt 200 ]; do echo new-$i; i=$((i+1)); sleep 0.1; done) >> "` + f + `" 2>&1; sleep 60`
		spec := redirectService(script)
		spec.UID = "redir-old"
		if _, err := e.Ensure(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "new lines", logsContain(t, e, "redir-old", "new-10\n"))
		if l, _ := e.Logs(context.Background(), "redir-old"); strings.Contains(l, "OLD-CONTENT") {
			t.Fatalf("an older file's history was copied: %q", l)
		}
	})
	t.Run("output the agent already receives is not duplicated and truncation is survived", func(t *testing.T) {
		e := newEngine(t, t.TempDir())
		f := filepath.Join(t.TempDir(), "trunc.log")
		spec := redirectService(`echo plain-once; (while :; do echo tick >> "` + f + `"; sleep 0.1; done) 2>&1 & sleep 60`)
		spec.UID = "redir-dup"
		if _, err := e.Ensure(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		if l, _ := e.Logs(context.Background(), "redir-dup"); strings.Count(l, "plain-once") != 1 {
			t.Fatalf("duplicated: %q", l)
		}
		// Only the program's own fd 1/2 count: a file it merely opens per command is not followed.
		if l, _ := e.Logs(context.Background(), "redir-dup"); strings.Contains(l, "following output redirected") {
			t.Fatalf("followed a file that is not the program's output: %q", l)
		}
	})
	t.Run("a truncated source continues", func(t *testing.T) {
		e := newEngine(t, t.TempDir())
		f := filepath.Join(t.TempDir(), "t.log")
		spec := redirectService(`(i=0; while [ $i -lt 400 ]; do echo seq-$i; i=$((i+1)); sleep 0.1; done) >> "` + f + `" 2>&1; sleep 60`)
		spec.UID = "redir-trunc"
		if _, err := e.Ensure(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "first lines", logsContain(t, e, "redir-trunc", "seq-3\n"))
		os.Truncate(f, 0)
		waitFor(t, "lines after truncation", func() bool {
			l, _ := e.Logs(context.Background(), "redir-trunc")
			return strings.Contains(l, "seq-3") && len(l) > 0 && strings.Count(l, "seq-") > 5
		})
	})
}

func TestRedirectedOutputIsFollowedProcessBackend(t *testing.T) {
	runRedirectTests(t, func(t *testing.T, dir string) Engine {
		e, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.Close(context.Background()) })
		return e
	})
}

func TestRedirectedOutputIsFollowedLaunchdBackend(t *testing.T) {
	runRedirectTests(t, func(t *testing.T, dir string) Engine { return launchdEngine(t, dir) })
}
