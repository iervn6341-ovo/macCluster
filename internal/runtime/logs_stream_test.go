package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func logStreamFixture(t *testing.T) (*engine, *boundedWriter) {
	t.Helper()
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, key("uid")+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	w := &boundedWriter{file: f}
	t.Cleanup(func() { w.Close() })
	e := &engine{dir: dir, entries: map[string]*entry{"uid": {spec: Spec{UID: "uid", Executor: "native"}, log: f, logWriter: w}}}
	return e, w
}

func intPtr(n int) *int { return &n }

func TestNativeLogStreamPreservesSnapshotAndRepeatedLines(t *testing.T) {
	e, w := logStreamFixture(t)
	w.Write([]byte("old\nsame\nsame\n"))
	reader, err := e.StreamLogs(context.Background(), "uid", LogOptions{Tail: intPtr(2), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for i := 0; i < 10; i++ {
		if _, err := w.Write([]byte("same\n")); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("same\n", 12)
	if string(got) != want {
		t.Fatalf("duplicate lines lost or reordered: got %q", got)
	}
}

func TestNativeLogTailZeroFollowAndRotation(t *testing.T) {
	e, w := logStreamFixture(t)
	w.Write([]byte("history must not be replayed\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader, err := e.StreamLogs(ctx, "uid", LogOptions{Tail: intPtr(0), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// Each block is identical; snapshot comparison would lose repeated output.
	block := bytes.Repeat([]byte("same line\n"), 8192)
	for i := 0; i < 32; i++ {
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(block))
		if _, err := io.ReadFull(reader, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, block) {
			t.Fatalf("write %d was lost across disk rotation", i)
		}
	}
	w.Close()
	got, err := io.ReadAll(reader)
	if err != nil || len(got) != 0 {
		t.Fatalf("unexpected trailing bytes: %d %v", len(got), err)
	}
	st, err := os.Stat(w.file.Name())
	if err != nil || st.Size() > logLimit {
		t.Fatalf("disk log exceeded bound: %v %v", st, err)
	}
}

func TestNativeLogSnapshotSubscriptionIsAtomic(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		e, w := logStreamFixture(t)
		w.Write([]byte("before\n"))
		start, done := make(chan struct{}), make(chan struct{})
		go func() { defer close(done); <-start; w.Write([]byte("concurrent\n")) }()
		close(start)
		reader, err := e.StreamLogs(context.Background(), "uid", LogOptions{Follow: true})
		if err != nil {
			t.Fatal(err)
		}
		<-done
		w.Close()
		got, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || string(got) != "before\nconcurrent\n" {
			t.Fatalf("atomicity violation: %q %v", got, err)
		}
	}
}

func TestNativeLogCancelAndCloseReleaseSubscribers(t *testing.T) {
	for _, mode := range []string{"cancel", "close", "writer-close"} {
		t.Run(mode, func(t *testing.T) {
			e, w := logStreamFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader, err := e.StreamLogs(ctx, "uid", LogOptions{Follow: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			done := make(chan error, 1)
			go func() { var p [1]byte; _, err := reader.Read(p[:]); done <- err }()
			switch mode {
			case "cancel":
				cancel()
			case "close":
				reader.Close()
			case "writer-close":
				w.Close()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("blocked read returned no terminal error")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked reader leaked")
			}
			w.mu.Lock()
			count := len(w.subscribers)
			w.mu.Unlock()
			if count != 0 {
				t.Fatalf("%d subscriptions leaked", count)
			}
		})
	}
}

func TestNativeSlowLogReaderDoesNotBlockWriter(t *testing.T) {
	e, w := logStreamFixture(t)
	reader, err := e.StreamLogs(context.Background(), "uid", LogOptions{Tail: intPtr(0), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan error, 1)
	go func() { _, err := w.Write(bytes.Repeat([]byte("x"), logLimit+1)); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow reader blocked workload")
	}
	_, err = io.ReadAll(reader)
	if !errors.Is(err, ErrSlowLogConsumer) {
		t.Fatalf("lost data without explicit slow-reader error: %v", err)
	}
	w.mu.Lock()
	count := len(w.subscribers)
	w.mu.Unlock()
	if count != 0 {
		t.Fatal("slow subscriber retained")
	}
}

func TestNativeLogLimitAndUnsupportedMetadata(t *testing.T) {
	e, w := logStreamFixture(t)
	w.Write([]byte("first\nsecond\n"))
	r, err := e.StreamLogs(context.Background(), "uid", LogOptions{Tail: intPtr(1), Follow: true, LimitBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != "sec" {
		t.Fatalf("tail/limit: %q %v", got, err)
	}
	w.mu.Lock()
	count := len(w.subscribers)
	w.mu.Unlock()
	if count != 0 {
		t.Fatal("limit did not unsubscribe")
	}
	if _, err := e.StreamLogs(context.Background(), "uid", LogOptions{Previous: true}); !errors.Is(err, ErrNoPreviousLogs) {
		t.Fatalf("a workload that has not run twice has no previous logs: %v", err)
	}
	for _, opts := range []LogOptions{{Tail: intPtr(-1)}, {LimitBytes: -1}, {SinceSeconds: -1}, {SinceTime: time.Now(), SinceSeconds: 1}} {
		if _, err := e.StreamLogs(context.Background(), "uid", opts); err == nil {
			t.Fatalf("accepted invalid options: %+v", opts)
		}
	}
}

func TestNativeLogWriterEndsWithoutReconciliation(t *testing.T) {
	iface, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer iface.Close(context.Background())
	e := iface.(*engine)
	_, err = e.Ensure(context.Background(), Spec{UID: "exit", Executor: "native", Command: []string{"/bin/sh", "-c", "printf 'done\\n'"}, RestartPolicy: "Never"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := e.StreamLogs(ctx, "exit", LogOptions{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "done\n" {
		t.Fatalf("process exit did not finish logs: %q %v", got, err)
	}
}

func fakeDockerLogs(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker")
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestDockerLogStreamCombinesOutputAndPassesOptions(t *testing.T) {
	dir := fakeDockerLogs(t, "printf '%s\\n' \"$@\" > \"$MACCLUSTER_TEST_DOCKER_ARGS\"\nprintf 'stdout\\n'\nprintf 'stderr\\n' >&2")
	argsFile := filepath.Join(dir, "args")
	t.Setenv("MACCLUSTER_TEST_DOCKER_ARGS", argsFile)
	opts := LogOptions{Tail: intPtr(7), Follow: true, Timestamps: true, SinceTime: time.Date(2026, 10, 18, 0, 0, 0, 123000000, time.UTC)}
	r, err := streamDockerLogs(context.Background(), "owned-id", opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "stdout\n") || !strings.Contains(string(got), "stderr\n") {
		t.Fatalf("missing log channel: %q", got)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"logs", "--tail", "7", "--follow", "--timestamps", "--since", "2026-10-18T00:00:00.123Z", "--", "owned-id"}
	if !reflect.DeepEqual(strings.Split(strings.TrimSpace(string(args)), "\n"), want) {
		t.Fatalf("wrong Docker arguments: %q", args)
	}
	if got := dockerLogArgs("id", LogOptions{Tail: intPtr(0), SinceSeconds: 12}); !reflect.DeepEqual(got, []string{"logs", "--tail", "0", "--since", "12s", "--", "id"}) {
		t.Fatalf("tail zero or relative since: %v", got)
	}
}

func TestDockerLogCancelCloseAndLimitReapCLI(t *testing.T) {
	for _, mode := range []string{"cancel", "close", "limit"} {
		t.Run(mode, func(t *testing.T) {
			fakeDockerLogs(t, "printf '123456789'\nexec /bin/sleep 30")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader, err := streamDockerLogs(ctx, "id", LogOptions{Follow: true})
			if err != nil {
				t.Fatal(err)
			}
			r := reader.(*dockerLogReader)
			defer r.Close()
			if mode == "limit" {
				got, err := io.ReadAll(limitLogReader(r, 3))
				if err != nil || string(got) != "123" {
					t.Fatalf("limit %q %v", got, err)
				}
			} else {
				got := make([]byte, 9)
				if _, err := io.ReadFull(r, got); err != nil {
					t.Fatal(err)
				}
				if mode == "cancel" {
					cancel()
				} else {
					r.Close()
				}
			}
			select {
			case <-r.done:
			case <-time.After(2 * time.Second):
				t.Fatal("Docker logs process was not reaped")
			}
		})
	}
}
