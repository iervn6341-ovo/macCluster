package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maccluster/internal/envs"
)

func TestNativeEnvironment(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0755)
	os.WriteFile(filepath.Join(root, "pyvenv.cfg"), []byte("home = /usr/bin\n"), 0644)
	// A stand-in for a venv's python: prints what the launcher prepared.
	script := "#!/bin/sh\necho venv=$VIRTUAL_ENV extra=$EXTRA pwd=$(pwd) arg=$1 podenv=$EXTRA2\nexec sleep 30\n"
	os.WriteFile(filepath.Join(root, "bin", "fake-python"), []byte(script), 0755)
	work := t.TempDir()
	envDir := filepath.Join(t.TempDir(), "environments")
	if err := envs.Add(envDir, envs.Env{Name: "py", Type: envs.TypeLink, Root: root, WorkDir: work, Env: map[string]string{"EXTRA": "from-env", "EXTRA2": "from-env"}}); err != nil {
		t.Fatal(err)
	}
	e, err := NewWithEnvironments(t.TempDir(), envDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer e.Close(ctx)
	if names := e.(interface{ Environments() []string }).Environments(); len(names) != 1 || names[0] != "py" {
		t.Fatal(names)
	}
	s := Spec{UID: "env-ok", Executor: "native", Environment: "py", Command: []string{"fake-python"}, Args: []string{"$(MACCLUSTER_PORT)"}, Env: map[string]string{"EXTRA2": "from-pod"}, RestartPolicy: "Never"}
	if _, err = e.Ensure(ctx, s); err != nil {
		t.Fatal(err)
	}
	var logs string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if logs, _ = e.Logs(ctx, s.UID); strings.Contains(logs, "podenv=") {
			break
		}
	}
	realWork, _ := filepath.EvalSymlinks(work)
	for _, want := range []string{"venv=" + root, "extra=from-env", "pwd=" + realWork, "podenv=from-pod"} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in %q", want, logs)
		}
	}
	e.Stop(ctx, s.UID)

	// Missing environment fails before any state or history is created.
	missing := s
	missing.UID, missing.Environment = "env-missing", "nope"
	if inst, err := e.Ensure(ctx, missing); err == nil || !strings.Contains(err.Error(), "not defined") || inst.PID != 0 {
		t.Fatalf("%+v %v", inst, err)
	}
	if len(e.List(ctx)) != 0 {
		t.Fatal("missing environment left an entry")
	}
	// Command lookup cannot escape the environment; bare names need an environment.
	escape := s
	escape.UID, escape.Command = "env-escape", []string{"../fake-python"}
	if _, err = e.Ensure(ctx, escape); err == nil {
		t.Fatal("escaping command accepted")
	}
	bare := Spec{UID: "bare", Executor: "native", Command: []string{"sleep"}}
	if _, err = e.Ensure(ctx, bare); err == nil {
		t.Fatal("bare command without environment accepted")
	}
	docker := Spec{UID: "d", Executor: "docker", Image: "x", Environment: "py"}
	if _, err = e.Ensure(ctx, docker); err == nil {
		t.Fatal("environment accepted for docker")
	}
}
