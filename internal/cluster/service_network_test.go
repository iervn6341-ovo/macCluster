package cluster

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	runtime "maccluster/internal/runtime"
	"maccluster/internal/servicenet"
)

func netPod(service bool) *core.Pod {
	p := testPod()
	p.UID = "11111111-2222-3333-4444-555555555555"
	if service {
		p.Labels[servicenet.NetworkLabel] = "service"
	}
	p.Spec.Containers[0].Ports = []core.ContainerPort{{Name: "http", ContainerPort: 8080}}
	return p
}

func readyStatus(ready bool) core.PodStatus {
	s := core.ConditionFalse
	if ready {
		s = core.ConditionTrue
	}
	return core.PodStatus{Conditions: []core.PodCondition{{Type: core.PodReady, Status: s}}}
}

func portsCondition(s core.PodStatus) *core.PodCondition {
	for i := range s.Conditions {
		if string(s.Conditions[i].Type) == servicenet.PortsCondition {
			return &s.Conditions[i]
		}
	}
	return nil
}

// A node may not change a Pod's annotations, only its status: the mapping goes into a status condition and the
// agent makes no API call of its own to publish it.
func TestPublishWritesAStatusConditionAndNoAPICall(t *testing.T) {
	client := fake.NewSimpleClientset()
	n := &serviceNetwork{ip: "127.0.0.1", enabled: true, forwarding: servicenet.NewForwarder()}
	defer n.forwarding.Close()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	target := l.Addr().(*net.TCPAddr).Port
	p := netPod(true)
	status := readyStatus(true)
	wanted := map[string]bool{}
	if err := n.publish(context.Background(), client, p, runtime.Instance{Phase: "Running", Port: target}, &status, wanted); err != nil {
		t.Fatal(err)
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("publishing must not call the API itself: %v", client.Actions())
	}
	c := portsCondition(status)
	if c == nil || c.Status != core.ConditionTrue {
		t.Fatalf("condition: %+v", status.Conditions)
	}
	var ports map[string]int
	if err := json.Unmarshal([]byte(c.Message), &ports); err != nil || ports["8080"] == 0 || ports["8080"] != ports["http"] {
		t.Fatalf("mapping %q: %v", c.Message, err)
	}
	// What the controller reads back from that status is the same mapping.
	p.Status = status
	got, ok := servicenet.PublishedPorts(p)
	if !ok || got["8080"] != int32(ports["8080"]) {
		t.Fatalf("controller view: %v %v", got, ok)
	}
	// A Pod that is not ready publishes nothing, and says so.
	status = readyStatus(false)
	if err := n.publish(context.Background(), client, p, runtime.Instance{Phase: "Running", Port: target}, &status, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if c := portsCondition(status); c == nil || c.Status != core.ConditionFalse || c.Message != "" {
		t.Fatalf("not ready: %+v", status.Conditions)
	}
}

func TestOnlyServiceNetworkedPodsCarryTheCondition(t *testing.T) {
	n := &serviceNetwork{ip: "127.0.0.1", enabled: true, forwarding: servicenet.NewForwarder()}
	defer n.forwarding.Close()
	status := readyStatus(true)
	if err := n.publish(context.Background(), fake.NewSimpleClientset(), netPod(false), runtime.Instance{Phase: "Running", Port: 1}, &status, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if portsCondition(status) != nil {
		t.Fatalf("an ordinary Pod got a service-ports condition: %+v", status.Conditions)
	}
}

func TestTransitionTimeIsKeptWhileTheConditionDoesNotChange(t *testing.T) {
	n := &serviceNetwork{ip: "127.0.0.1", enabled: true, forwarding: servicenet.NewForwarder()}
	defer n.forwarding.Close()
	p := netPod(true)
	earlier := meta.NewTime(time.Now().Add(-time.Hour))
	p.Status.Conditions = []core.PodCondition{{Type: core.PodConditionType(servicenet.PortsCondition), Status: core.ConditionFalse, LastTransitionTime: earlier}}
	status := readyStatus(false)
	if err := n.publish(context.Background(), fake.NewSimpleClientset(), p, runtime.Instance{Phase: "Running", Port: 1}, &status, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if c := portsCondition(status); c == nil || !c.LastTransitionTime.Equal(&earlier) {
		t.Fatalf("transition time: %+v", c)
	}
}
