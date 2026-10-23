package cluster

import (
	"context"
	"regexp"
	goruntime "runtime"
	"testing"

	core "k8s.io/api/core/v1"
)

func TestNodeStatusReportsWhatAKubeletReports(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS host facts")
	}
	n := &core.Node{}
	if err := updateHostStatus(context.Background(), n, NodeOptions{StateDir: t.TempDir()}, 1); err != nil {
		t.Fatal(err)
	}
	if q, ok := n.Status.Capacity[core.ResourceEphemeralStorage]; !ok || q.IsZero() {
		t.Fatalf("ephemeral-storage capacity missing: %v", n.Status.Capacity)
	}
	if q, ok := n.Status.Allocatable[core.ResourceEphemeralStorage]; !ok || q.IsZero() {
		t.Fatalf("ephemeral-storage allocatable missing: %v", n.Status.Allocatable)
	}
	var pid *core.NodeCondition
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == core.NodePIDPressure {
			pid = &n.Status.Conditions[i]
		}
	}
	if pid == nil || pid.Status == core.ConditionUnknown {
		t.Fatalf("PIDPressure must be measured, not Unknown: %+v", pid)
	}
	info := n.Status.NodeInfo
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(info.MachineID) {
		t.Fatalf("machineID: %q", info.MachineID)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(info.SystemUUID) {
		t.Fatalf("systemUUID: %q", info.SystemUUID)
	}
	if info.BootID == "" {
		t.Fatal("bootID missing")
	}
	again := &core.Node{}
	if err := updateHostStatus(context.Background(), again, NodeOptions{StateDir: t.TempDir()}, 1); err != nil {
		t.Fatal(err)
	}
	if again.Status.NodeInfo.MachineID != info.MachineID {
		t.Fatal("machineID must be stable for the machine")
	}
}
