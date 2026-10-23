package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerLimitAndSecurityArguments(t *testing.T) {
	got := strings.Join(dockerLimitArgs(&ResourceSpec{CPUMilli: 1500, MemoryBytes: 64 << 20}), " ")
	if got != "--cpus 1.500 --memory 67108864 --memory-swap 67108864" {
		t.Fatalf("limits: %s", got)
	}
	if len(dockerLimitArgs(nil)) != 0 || len(dockerLimitArgs(&ResourceSpec{})) != 0 {
		t.Fatal("no limits, no arguments")
	}
	sec := strings.Join(dockerSecurityArgs(&SecuritySpec{User: "1000:2000", GroupAdd: []string{"3000"}, ReadOnlyRootFS: true, NoNewPrivileges: true, CapDrop: []string{"ALL"}, CapAdd: []string{"NET_BIND_SERVICE"}}), " ")
	if sec != "--user 1000:2000 --group-add 3000 --read-only --security-opt no-new-privileges --cap-drop ALL --cap-add NET_BIND_SERVICE" {
		t.Fatalf("security: %s", sec)
	}
}

func TestSpecValidationOfLimitsAndSecurity(t *testing.T) {
	native := Spec{UID: "u", Executor: "native", Command: []string{"/bin/true"}}
	native.Limits = &ResourceSpec{MemoryBytes: 1}
	if err := validate(&native); err == nil {
		t.Fatal("hard limits are Docker-only")
	}
	docker := Spec{UID: "u", Executor: "docker", Image: "x", Security: &SecuritySpec{User: "root"}}
	if err := validate(&docker); err == nil {
		t.Fatal("a user name is not a numeric id")
	}
	docker.Security = &SecuritySpec{CapAdd: []string{"net raw"}}
	if err := validate(&docker); err == nil {
		t.Fatal("capability names are checked")
	}
	docker.Security = &SecuritySpec{User: "1000:1000", CapDrop: []string{"ALL"}}
	if err := validate(&docker); err != nil {
		t.Fatal(err)
	}
}

func TestResolvedEnvironmentReachesNativeProgramButIsNotPartOfTheSpecHash(t *testing.T) {
	dir := t.TempDir()
	e, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	spec := Spec{UID: "env-1", Name: "env-1", Executor: "native", Command: []string{"/bin/sh", "-c", "echo secret=$SECRET_X; echo port=$ALSO; sleep 30"}, RestartPolicy: "Never",
		Env: map[string]string{"ALSO": "$(MACCLUSTER_PORT)"}, EnvResolved: map[string]string{"SECRET_X": "multi\nline"}}
	if _, err := e.Ensure(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logs, _ := e.Logs(context.Background(), "env-1")
		if strings.Contains(logs, "secret=multi") {
			if !strings.Contains(logs, "port=") {
				t.Fatalf("logs: %q", logs)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h1, h2 := specHash(spec), specHash(Spec{UID: "env-1", Name: "env-1", Executor: "native", Command: spec.Command, RestartPolicy: "Never", Env: spec.Env, EnvResolved: map[string]string{"SECRET_X": "rotated"}}); h1 != h2 {
		t.Fatal("a rotated Secret must not make the workload look different")
	}
	// The value is never written to the state directory except through the program's own output.
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".native") || strings.HasSuffix(ent.Name(), ".json") {
			data, _ := os.ReadFile(filepath.Join(dir, ent.Name()))
			if strings.Contains(string(data), "multi") {
				t.Fatalf("%s contains the resolved value", ent.Name())
			}
		}
	}
}
