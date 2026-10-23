package runtime

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func readAll(t *testing.T, r io.ReadCloser, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIndexMapsOffsetsToTheBatchThatContainedThem(t *testing.T) {
	t0 := time.Unix(1000, 0)
	idx := []indexEntry{{end: 10, at: t0.UnixNano()}, {end: 25, at: t0.Add(5 * time.Second).UnixNano()}}
	for offset, want := range map[int64]time.Duration{0: 0, 9: 0, 10: 5 * time.Second, 24: 5 * time.Second} {
		got, ok := timeAt(idx, offset)
		if !ok || !got.Equal(t0.Add(want)) {
			t.Errorf("offset %d: %v %v, want +%v", offset, got, ok, want)
		}
	}
	if _, ok := timeAt(idx, 25); ok {
		t.Error("bytes after the last record are unknown, not stamped with an old time")
	}
}

func TestRenderLinesAppliesSinceThenTailThenTimestamps(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lines := []stampedLine{{base, []byte("a\n")}, {base.Add(time.Second), []byte("b\n")}, {base.Add(2 * time.Second), []byte("c\n")}, {base.Add(3 * time.Second), []byte("d")}}
	two := 2
	if got := string(renderLines(lines, base.Add(time.Second), nil, false)); got != "b\nc\nd" {
		t.Fatalf("since: %q", got)
	}
	if got := string(renderLines(lines, base.Add(time.Second), &two, false)); got != "c\nd" {
		t.Fatalf("tail applies to what --since kept: %q", got)
	}
	if got := string(renderLines(lines[:2], time.Time{}, nil, true)); got != "2026-01-01T00:00:00Z a\n2026-01-01T00:00:01Z b\n" {
		t.Fatalf("timestamps: %q", got)
	}
	zero := 0
	if len(renderLines(lines, time.Time{}, &zero, false)) != 0 {
		t.Fatal("--tail=0 is empty")
	}
}

func TestLiveLinesAreStampedOnceEvenWhenAChunkEndsMidLine(t *testing.T) {
	s := &lineStamper{}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := string(s.stamp([]byte("one\ntw"), at)) + string(s.stamp([]byte("o\nthree\n"), at.Add(time.Second)))
	want := "2026-01-01T00:00:00Z one\n2026-01-01T00:00:00Z two\n2026-01-01T00:00:01Z three\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNativeLogsHaveTimestampsAndSinceAndTheirRunsAreSeparate(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	spec := Spec{UID: "lt", Name: "lt", Namespace: "ns", Executor: "native", RestartPolicy: "Always",
		Command: []string{"/bin/sh", "-c", `echo "run-$RUNS first"; sleep 1.3; echo "run-$RUNS second"; if [ -f /tmp/maccluster-lt-$$ ]; then :; fi; exit 1`}, Env: map[string]string{"RUNS": "x"}}
	ctx := context.Background()
	var inst Instance
	waitFor(t, "the first run to print", func() bool {
		i, _ := e.Ensure(ctx, spec)
		inst = i
		l, _ := e.Logs(ctx, "lt")
		return strings.Contains(l, "run-x first")
	})
	streamer := e.(interface {
		StreamLogs(context.Context, string, LogOptions) (io.ReadCloser, error)
	})

	// timestamps
	waitFor(t, "second line", func() bool { l, _ := e.Logs(ctx, "lt"); return strings.Contains(l, "second") })
	r, err := streamer.StreamLogs(ctx, "lt", LogOptions{Timestamps: true})
	out := readAll(t, r, err)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		ts, _, ok := strings.Cut(line, " ")
		if _, perr := time.Parse(time.RFC3339Nano, ts); !ok || perr != nil {
			t.Fatalf("every line starts with an RFC3339 time: %q", line)
		}
	}
	first, second := strings.Split(out, "\n")[0], strings.Split(out, "\n")[1]
	t1, _ := time.Parse(time.RFC3339Nano, strings.Fields(first)[0])
	t2, _ := time.Parse(time.RFC3339Nano, strings.Fields(second)[0])
	if gap := t2.Sub(t1); gap < 900*time.Millisecond || gap > 2500*time.Millisecond {
		t.Fatalf("the two lines were printed 1.3 s apart; stamped %s apart", gap)
	}

	// since: only lines from the last second
	r, err = streamer.StreamLogs(ctx, "lt", LogOptions{SinceSeconds: 1})
	recent := readAll(t, r, err)
	if strings.Contains(recent, "first") || !strings.Contains(recent, "second") {
		t.Fatalf("--since=1s must drop the older line: %q", recent)
	}
	r, err = streamer.StreamLogs(ctx, "lt", LogOptions{SinceTime: t2})
	if got := readAll(t, r, err); strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("--since-time: %q", got)
	}

	// the program exits 1 and Always restarts it: the new run has a log of its own and --previous returns the old one
	waitFor(t, "a second run", func() bool {
		i, _ := e.Ensure(ctx, spec)
		return i.Restarts >= 1 && i.PID != 0 && i.PID != inst.PID && i.Phase == "Running"
	})
	waitFor(t, "the new run's first line", func() bool {
		r, err := streamer.StreamLogs(ctx, "lt", LogOptions{})
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(r)
		r.Close()
		return strings.Contains(string(b), "first") && !strings.Contains(string(b), "second")
	})
	r, err = streamer.StreamLogs(ctx, "lt", LogOptions{Previous: true})
	prev := readAll(t, r, err)
	if !strings.Contains(prev, "first") || !strings.Contains(prev, "second") {
		t.Fatalf("--previous is the whole earlier run: %q", prev)
	}
	r, err = streamer.StreamLogs(ctx, "lt", LogOptions{})
	cur := readAll(t, r, err)
	if strings.Contains(cur, "second") {
		t.Fatalf("kubectl logs shows the current run only: %q", cur)
	}
	if all, _ := e.Logs(ctx, "lt"); !strings.Contains(all, "second") || strings.Count(all, "first") < 2 {
		t.Fatalf("the app still shows the history of every run: %q", all)
	}
	_ = bytes.MinRead
	_ = os.Stdout
}

func TestFollowingWithTimestampsStampsLiveLinesAsTheyArrive(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	runningNative(t, e, "lf", `echo before; sleep 0.6; printf 'half'; sleep 0.5; echo ' and rest'; sleep 30`, "Never", nil)
	ctx := context.Background()
	waitFor(t, "the first line", func() bool { l, _ := e.Logs(ctx, "lf"); return strings.Contains(l, "before") })
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := e.(interface {
		StreamLogs(context.Context, string, LogOptions) (io.ReadCloser, error)
	}).StreamLogs(cctx, "lf", LogOptions{Follow: true, Timestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got string
	buf := make([]byte, 4096)
	for !strings.Contains(got, "half and rest\n") {
		n, err := r.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended early: %q %v", got, err)
		}
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " before") || !strings.HasSuffix(lines[1], " half and rest") || strings.Count(lines[1], "T") != 1 {
		t.Fatalf("a line that arrives in two pieces gets one prefix: %q", got)
	}
}

func TestDockerLogArgumentsFollowTheRunBoundaries(t *testing.T) {
	got := strings.Join(dockerLogArgs("cid", LogOptions{Previous: true, SinceTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), until: "2026-01-01T00:10:00Z", Timestamps: true}), " ")
	if !strings.Contains(got, "--since 2026-01-01T00:00:00Z") || !strings.Contains(got, "--until 2026-01-01T00:10:00Z") || !strings.Contains(got, "--timestamps") {
		t.Fatalf("%s", got)
	}
}
