package cluster

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestReconcileObservesAssignedPodsWithoutStatusPolling(t *testing.T) {
	p := reconcilePod(func(*corev1.Container) {})
	client := fake.NewSimpleClientset(p)
	var calls int
	var got []PodObservation
	settings := nodeSettings{observePods: func(pods []PodObservation) { calls++; got = pods }}
	engine := &podEngine{}
	probes := map[string]*probeState{}
	if err := reconcile(context.Background(), client, "node-1", engine, probes, settings); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(got) != 1 || got[0].Phase != "Running" || got[0].UID != string(p.UID) {
		t.Fatalf("observed: %+v", got)
	}
	// An unavailable list must not be mistaken for zero assigned Pods.
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, kruntime.Object, error) { return true, nil, errors.New("unavailable") })
	if err := reconcile(context.Background(), client, "node-1", engine, probes, settings); err == nil {
		t.Fatal("expected list failure")
	}
	if calls != 1 {
		t.Fatal("failed list emitted removal observation")
	}
	client.ReactionChain = client.ReactionChain[1:]
	if err := client.CoreV1().Pods(p.Namespace).Delete(context.Background(), p.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(context.Background(), client, "node-1", engine, probes, settings); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(got) != 0 {
		t.Fatal("empty successful list not observed")
	}
}
