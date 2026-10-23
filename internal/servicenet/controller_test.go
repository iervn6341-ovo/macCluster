package servicenet

import (
	"context"
	"testing"
	"time"

	coordination "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func healthyFixture(now time.Time) (core.Node, coordination.Lease) {
	ttl := int32(40)
	return core.Node{ObjectMeta: meta.ObjectMeta{Name: "mac-test", Annotations: map[string]string{"maccluster.example.com/installation-id": "test-install"}}, Status: core.NodeStatus{
		NodeInfo: core.NodeSystemInfo{OperatingSystem: "darwin"}, Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}, Addresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: "192.0.2.10"}},
	}}, coordination.Lease{ObjectMeta: meta.ObjectMeta{Name: "mac-test", Namespace: "kube-node-lease"}, Spec: coordination.LeaseSpec{LeaseDurationSeconds: &ttl, RenewTime: &meta.MicroTime{Time: now.Add(-time.Second)}}}
}

func serviceFixture() core.Service {
	return core.Service{ObjectMeta: meta.ObjectMeta{Name: "web", Namespace: "work", UID: "service-uid", Labels: map[string]string{NetworkLabel: "service"}, Annotations: map[string]string{SelectorAnnotation: `{"app":"web"}`}}, Spec: core.ServiceSpec{Ports: []core.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: core.ProtocolTCP}}}}
}

func podFixture() core.Pod {
	return core.Pod{ObjectMeta: meta.ObjectMeta{Name: "web-one", Namespace: "work", UID: "pod-one", Labels: map[string]string{NetworkLabel: "service", "maccluster.example.com/managed": "true", "app": "web"}, Annotations: map[string]string{PortsAnnotation: `{"8080":31001,"http":31001}`}}, Spec: core.PodSpec{NodeName: "mac-test"}, Status: core.PodStatus{HostIP: "192.0.2.10", Phase: core.PodRunning, Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}}}
}

func TestHealthyNodeRequiresReadyAndCurrentLease(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	node, lease := healthyFixture(now)
	if ip, ok := healthyNode(node, &lease, now); !ok || ip != "192.0.2.10" {
		t.Fatalf("healthy node rejected: %q %t", ip, ok)
	}
	for _, tc := range []struct {
		name   string
		change func(*core.Node, *coordination.Lease)
	}{
		{"not ready", func(n *core.Node, l *coordination.Lease) { n.Status.Conditions[0].Status = core.ConditionFalse }},
		{"unknown readiness", func(n *core.Node, l *coordination.Lease) { n.Status.Conditions[0].Status = core.ConditionUnknown }},
		{"missing readiness", func(n *core.Node, l *coordination.Lease) { n.Status.Conditions = nil }},
		{"expired lease", func(n *core.Node, l *coordination.Lease) { l.Spec.RenewTime.Time = now.Add(-41 * time.Second) }},
		{"future lease", func(n *core.Node, l *coordination.Lease) { l.Spec.RenewTime.Time = now.Add(6 * time.Second) }},
		{"missing renew", func(n *core.Node, l *coordination.Lease) { l.Spec.RenewTime = nil }},
		{"missing duration", func(n *core.Node, l *coordination.Lease) { l.Spec.LeaseDurationSeconds = nil }},
		{"zero duration", func(n *core.Node, l *coordination.Lease) { *l.Spec.LeaseDurationSeconds = 0 }},
		{"negative duration", func(n *core.Node, l *coordination.Lease) { *l.Spec.LeaseDurationSeconds = -1 }},
		{"excessive duration", func(n *core.Node, l *coordination.Lease) { *l.Spec.LeaseDurationSeconds = 61 }},
		{"non Mac", func(n *core.Node, l *coordination.Lease) { n.Status.NodeInfo.OperatingSystem = "linux" }},
		{"unmanaged node", func(n *core.Node, l *coordination.Lease) { n.Annotations = nil }},
		{"deleted node", func(n *core.Node, l *coordination.Lease) { n.DeletionTimestamp = &meta.Time{Time: now} }},
		{"missing address", func(n *core.Node, l *coordination.Lease) { n.Status.Addresses = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, l := healthyFixture(now)
			tc.change(&n, &l)
			if _, ok := healthyNode(n, &l, now); ok {
				t.Fatal("unhealthy node accepted")
			}
		})
	}
	if _, ok := healthyNode(node, nil, now); ok {
		t.Fatal("missing lease accepted")
	}
}

func TestDesiredSlicesFiltersPods(t *testing.T) {
	s := serviceFixture()
	for _, tc := range []struct {
		name   string
		change func(*core.Pod)
	}{
		{"without opt-in", func(p *core.Pod) { delete(p.Labels, NetworkLabel) }},
		{"unmanaged", func(p *core.Pod) { delete(p.Labels, "maccluster.example.com/managed") }},
		{"different namespace", func(p *core.Pod) { p.Namespace = "other" }},
		{"selector mismatch", func(p *core.Pod) { p.Labels["app"] = "other" }},
		{"not ready", func(p *core.Pod) { p.Status.Conditions[0].Status = core.ConditionFalse }},
		{"missing ready condition", func(p *core.Pod) { p.Status.Conditions = nil }},
		{"not running", func(p *core.Pod) { p.Status.Phase = core.PodPending }},
		{"deleted", func(p *core.Pod) { p.DeletionTimestamp = &meta.Time{Time: time.Now()} }},
		{"unhealthy node", func(p *core.Pod) { p.Spec.NodeName = "other" }},
		{"host address mismatch", func(p *core.Pod) { p.Status.HostIP = "192.0.2.11" }},
		{"malformed ports", func(p *core.Pod) { p.Annotations[PortsAnnotation] = "not JSON" }},
		{"unmapped target port", func(p *core.Pod) { p.Annotations[PortsAnnotation] = `{"8081":31001}` }},
		{"zero port", func(p *core.Pod) { p.Annotations[PortsAnnotation] = `{"8080":0}` }},
		{"invalid port", func(p *core.Pod) { p.Annotations[PortsAnnotation] = `{"8080":65536}` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := podFixture()
			tc.change(&p)
			got, err := desiredSlices(&s, []core.Pod{p}, map[string]string{"mac-test": "192.0.2.10"})
			if err != nil || len(got) != 0 {
				t.Fatalf("unexpected endpoints: %+v, %v", got, err)
			}
		})
	}
}

func TestDesiredSlicesResolvesTargetPortsPerReplica(t *testing.T) {
	for _, tc := range []struct {
		name        string
		target      intstr.IntOrString
		servicePort int32
		mapping     string
	}{
		{"numeric", intstr.FromInt(8080), 80, `{"8080":31002}`},
		{"named", intstr.FromString("http"), 80, `{"http":31002}`},
		{"default target", intstr.IntOrString{}, 8080, `{"8080":31002}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serviceFixture()
			s.Spec.Ports[0].TargetPort = tc.target
			s.Spec.Ports[0].Port = tc.servicePort
			one, two := podFixture(), podFixture()
			two.Name = "web-two"
			two.UID = "pod-two"
			two.Annotations[PortsAnnotation] = tc.mapping
			got, err := desiredSlices(&s, []core.Pod{one, two}, map[string]string{"mac-test": "192.0.2.10"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("got %d slices, want 2", len(got))
			}
			seen := map[types.UID]int32{}
			for _, slice := range got {
				if slice.Namespace != s.Namespace || slice.Labels[discovery.LabelServiceName] != s.Name || slice.Labels[discovery.LabelManagedBy] != ManagedBy {
					t.Fatalf("incorrect ownership: %+v", slice.ObjectMeta)
				}
				if len(slice.OwnerReferences) != 1 || slice.OwnerReferences[0].UID != s.UID || slice.OwnerReferences[0].Controller == nil || !*slice.OwnerReferences[0].Controller {
					t.Fatalf("incorrect owner: %+v", slice.OwnerReferences)
				}
				if len(slice.Ports) != 1 || len(slice.Endpoints) != 1 {
					t.Fatalf("replica must retain its own port: %+v", slice)
				}
				ep := slice.Endpoints[0]
				if ep.Conditions.Ready == nil || !*ep.Conditions.Ready || ep.TargetRef == nil || ep.TargetRef.Namespace != s.Namespace || len(ep.Addresses) != 1 || ep.Addresses[0] != "192.0.2.10" {
					t.Fatalf("incorrect endpoint: %+v", ep)
				}
				if slice.Ports[0].Port == nil || slice.Ports[0].Protocol == nil || *slice.Ports[0].Protocol != core.ProtocolTCP {
					t.Fatalf("incorrect port: %+v", slice.Ports)
				}
				seen[ep.TargetRef.UID] = *slice.Ports[0].Port
			}
			if seen[one.UID] != 31001 || seen[two.UID] != 31002 {
				t.Fatalf("replica ports conflated: %v", seen)
			}
		})
	}
}

func TestControllerWithdrawsOnLeaseExpiryAndOptOut(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	node, lease := healthyFixture(now)
	service, pod := serviceFixture(), podFixture()
	ordinary := *service.DeepCopy()
	ordinary.Name = "ordinary"
	ordinary.UID = "ordinary-uid"
	ordinary.Labels = nil
	outside := *service.DeepCopy()
	outside.Namespace = "outside"
	outside.UID = "outside-uid"
	client := fake.NewSimpleClientset(&node, &lease, &service, &pod, &ordinary, &outside)
	c := Controller{Client: client, Namespaces: []string{"work"}, Now: func() time.Time { return now }}
	assertCount := func(want int) {
		t.Helper()
		slices, err := client.DiscoveryV1().EndpointSlices("").List(ctx, meta.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(slices.Items) != want {
			t.Fatalf("got %d slices, want %d", len(slices.Items), want)
		}
		for _, s := range slices.Items {
			if s.Namespace != "work" || s.Labels[discovery.LabelServiceName] != "web" {
				t.Fatalf("non opt-in or outside service was reconciled: %+v", s.ObjectMeta)
			}
		}
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertCount(1)
	now = now.Add(41 * time.Second)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertCount(0)
	lease.Spec.RenewTime = &meta.MicroTime{Time: now}
	if _, err := client.CoordinationV1().Leases(lease.Namespace).Update(ctx, &lease, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertCount(1)
	service.Labels = nil
	if _, err := client.CoreV1().Services(service.Namespace).Update(ctx, &service, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertCount(0)
}

func TestPublishedPortsPreferTheStatusConditionOverTheOldAnnotation(t *testing.T) {
	p := podFixture()
	p.Annotations[PortsAnnotation] = `{"8080":1111}`
	cond := func(status core.ConditionStatus, msg string) core.Pod {
		q := *p.DeepCopy()
		q.Status.Conditions = append(q.Status.Conditions, core.PodCondition{Type: core.PodConditionType(PortsCondition), Status: status, Message: msg})
		return q
	}
	q := cond(core.ConditionTrue, `{"8080":2222,"http":2222}`)
	if got, ok := PublishedPorts(&q); !ok || got["8080"] != 2222 {
		t.Fatalf("the condition must win: %v %v", got, ok)
	}
	q = cond(core.ConditionFalse, "")
	if _, ok := PublishedPorts(&q); ok {
		t.Fatal("a False condition means nothing is published, whatever an old annotation says")
	}
	q = cond(core.ConditionTrue, "not json")
	if _, ok := PublishedPorts(&q); ok {
		t.Fatal("a malformed condition message must publish nothing")
	}
	// No condition: an older agent's annotation still works.
	if got, ok := PublishedPorts(&p); !ok || got["8080"] != 1111 {
		t.Fatalf("fallback: %v %v", got, ok)
	}
	p.Annotations = nil
	if _, ok := PublishedPorts(&p); ok {
		t.Fatal("nothing published")
	}
}

func TestControllerBuildsEndpointsFromTheStatusCondition(t *testing.T) {
	p := podFixture()
	delete(p.Annotations, PortsAnnotation)
	p.Status.Conditions = append(p.Status.Conditions, core.PodCondition{Type: core.PodConditionType(PortsCondition), Status: core.ConditionTrue, Message: `{"8080":31005,"http":31005}`})
	svc := serviceFixture()
	slices, err := desiredSlices(&svc, []core.Pod{p}, map[string]string{"mac-test": "192.0.2.10"})
	if err != nil || len(slices) != 1 {
		t.Fatalf("%d slices, %v", len(slices), err)
	}
	for _, s := range slices {
		if *s.Ports[0].Port != 31005 {
			t.Fatalf("port: %d", *s.Ports[0].Port)
		}
	}
}
