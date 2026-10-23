package runtime

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestParsePSTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:00.15":      150 * time.Millisecond,
		"1:02.50":      62500 * time.Millisecond,
		"1:02:03.00":   time.Hour + 2*time.Minute + 3*time.Second,
		"2-03:04:05.5": 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5500*time.Millisecond,
	} {
		got, ok := parsePSTime(in)
		if !ok || got != want {
			t.Errorf("parsePSTime(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "abc", "1:2:3:4", "x-1:00"} {
		if _, ok := parsePSTime(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestNativeUsageIsCumulativeCPUAndMemoryOfTheWorkloadsProcessGroup(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	// burns CPU in the shell itself and in a child, so the process group's sum is what counts
	runningNative(t, e, "u1", `i=0; while true; do i=$((i+1)); done`, "Never", nil)
	reader := e.(UsageReader)
	var first Usage
	waitFor(t, "a first sample", func() bool {
		for _, u := range reader.Usage(context.Background()) {
			if u.UID == "u1" && u.CPUUsageNanoSeconds > 0 {
				first = u
				return true
			}
		}
		return false
	})
	if first.MemoryWorkingSetBytes == 0 || first.StartedAt.IsZero() {
		t.Fatalf("memory and start time are reported: %+v", first)
	}
	time.Sleep(1200 * time.Millisecond)
	var second Usage
	for _, u := range reader.Usage(context.Background()) {
		if u.UID == "u1" {
			second = u
		}
	}
	if second.CPUUsageNanoSeconds <= first.CPUUsageNanoSeconds {
		t.Fatalf("CPU time is a counter that grows while the program runs: %d then %d", first.CPUUsageNanoSeconds, second.CPUUsageNanoSeconds)
	}
}

func TestDockerUsageComesFromTheEngineAPIsCounters(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Docker is not available")
	}
	if err := exec.Command("docker", "image", "inspect", "nginx:alpine").Run(); err != nil {
		t.Skip("nginx:alpine is not present locally")
	}
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	spec := Spec{UID: "usage-docker", Name: "usage-docker", Namespace: "ns", Executor: "docker", Image: "nginx:alpine", RestartPolicy: "Never"}
	inst, err := e.Ensure(context.Background(), spec)
	if err != nil || inst.Phase != "Running" {
		t.Fatalf("%+v %v", inst, err)
	}
	defer e.Stop(context.Background(), "usage-docker")
	waitFor(t, "docker stats", func() bool {
		for _, u := range e.(UsageReader).Usage(context.Background()) {
			if u.UID == "usage-docker" && u.MemoryWorkingSetBytes > 0 {
				return true
			}
		}
		return false
	})
}
