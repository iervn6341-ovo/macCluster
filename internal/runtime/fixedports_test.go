package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	p, err := allocatePort()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitLog(t *testing.T, e Engine, uid, want string) string {
	t.Helper()
	var logs string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		logs, _ = e.Logs(context.Background(), uid)
		if strings.Contains(logs, want) {
			return logs
		}
	}
	t.Fatalf("log never contained %q: %q", want, logs)
	return ""
}

func fixedSpec(uid string, ports ...int) Spec {
	return Spec{UID: uid, Name: uid, Executor: "native", Command: []string{"/bin/sh"}, FixedPorts: ports, RestartPolicy: "Never",
		Args: []string{"-c", "echo first=$MACCLUSTER_PORT all=$MACCLUSTER_PORTS arg=$1 list=$2 end; exec sleep 30", "x", "$(MACCLUSTER_PORT)", "$(MACCLUSTER_PORTS)"}}
}

func TestFixedPortsAreExposedAndNotAllocated(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer e.Close(ctx)
	a, b := freePort(t), freePort(t)
	inst, err := e.Ensure(ctx, fixedSpec("fixed-1", a, b))
	if err != nil {
		t.Fatal(err)
	}
	if inst.Phase != "Running" || inst.Port != a || len(inst.Ports) != 2 || inst.Ports[0] != a || inst.Ports[1] != b {
		t.Fatalf("%+v", inst)
	}
	want := fmt.Sprintf("first=%d all=%d,%d arg=%d list=%d,%d end", a, a, b, a, a, b)
	waitLog(t, e, "fixed-1", want)
	// Dynamic workloads keep the old behaviour and report a single allocated port.
	dyn, err := e.Ensure(ctx, Spec{UID: "dyn", Executor: "native", Command: []string{"/bin/sh"}, Args: []string{"-c", "echo all=$MACCLUSTER_PORTS first=$MACCLUSTER_PORT; exec sleep 30"}, RestartPolicy: "Never"})
	if err != nil || dyn.Port <= 0 || len(dyn.Ports) != 0 {
		t.Fatalf("%+v %v", dyn, err)
	}
	waitLog(t, e, "dyn", fmt.Sprintf("all=%d first=%d", dyn.Port, dyn.Port))
}

func TestFixedPortBusyStaysPendingAndRecovers(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer e.Close(ctx)
	p := freePort(t)
	// Something outside the agent holds the port.
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		inst, err := e.Ensure(ctx, fixedSpec("busy", p))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("fixed port %d is already in use", p)) {
			t.Fatalf("try %d: %v", i, err)
		}
		if inst.Phase != "Pending" || inst.Restarts != 0 || inst.FailureCount != 0 || inst.PID != 0 {
			t.Fatalf("busy port must stay Pending without counting failures: %+v", inst)
		}
	}
	l.Close()
	inst, err := e.Ensure(ctx, fixedSpec("busy", p))
	if err != nil || inst.Phase != "Running" || inst.Port != p {
		t.Fatalf("%+v %v", inst, err)
	}
}

func TestSecondWorkloadCannotShareAFixedPort(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer e.Close(ctx)
	shared, own := freePort(t), freePort(t)
	if _, err := e.Ensure(ctx, fixedSpec("first", shared)); err != nil {
		t.Fatal(err)
	}
	// The first process does not listen (it only sleeps), so the OS check passes;
	// the engine's own bookkeeping must still refuse the overlap.
	_, err = e.Ensure(ctx, fixedSpec("second", own, shared))
	if err == nil || !strings.Contains(err.Error(), "already in use by first") {
		t.Fatalf("overlap accepted: %v", err)
	}
	if err := e.Stop(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	inst, err := e.Ensure(ctx, fixedSpec("second", own, shared))
	if err != nil || inst.Phase != "Running" {
		t.Fatalf("%+v %v", inst, err)
	}
}

func TestFixedPortValidation(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer e.Close(ctx)
	many := make([]int, 9)
	for i := range many {
		many[i] = 20000 + i
	}
	for name, s := range map[string]Spec{
		"zero":      fixedSpec("v1", 0),
		"too big":   fixedSpec("v2", 70000),
		"duplicate": fixedSpec("v3", 20001, 20001),
		"too many":  fixedSpec("v4", many...),
		"docker":    {UID: "v5", Executor: "docker", Image: "x", FixedPorts: []int{20002}},
	} {
		if _, err := e.Ensure(ctx, s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if len(e.List(ctx)) != 0 {
		t.Fatal("invalid specs left entries behind")
	}
}

func TestStatusJSONKeepsFixedPortsApartFromDockerPortsString(t *testing.T) {
	b, err := json.Marshal(Instance{UID: "u", Port: 8100, Ports: []int{8100, 9100}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"fixedPorts":[8100,9100]`) || strings.Contains(string(b), `"ports"`) {
		t.Fatalf("status must expose fixedPorts only: %s", b)
	}
}
