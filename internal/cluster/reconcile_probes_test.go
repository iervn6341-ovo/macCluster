package cluster

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	runtime "maccluster/internal/runtime"
)

// podEngine is a runtime for reconcile tests: one running workload, recording what the reconciler asks of it.
type podEngine struct {
	fakeExec
	started string
	mu2     sync.Mutex
	kills   []string
	stopped []time.Duration
	order   []string
}

func (e *podEngine) Ensure(_ context.Context, s runtime.Spec) (runtime.Instance, error) {
	return runtime.Instance{UID: s.UID, Phase: "Running", Port: 1, StartedAt: e.started}, nil
}
func (e *podEngine) Stop(context.Context, string) error { return nil }
func (e *podEngine) List(context.Context) []runtime.Instance {
	return []runtime.Instance{{UID: "unique", Phase: "Running", Port: 1, StartedAt: e.started}}
}
func (e *podEngine) Logs(context.Context, string) (string, error) { return "", nil }
func (e *podEngine) Close(context.Context) error                  { return nil }
func (e *podEngine) Kill(_ context.Context, _ string, reason string, _ time.Duration) error {
	e.mu2.Lock()
	e.kills = append(e.kills, reason)
	e.mu2.Unlock()
	return nil
}
func (e *podEngine) StopGrace(_ context.Context, _ string, grace time.Duration) error {
	e.mu2.Lock()
	e.stopped = append(e.stopped, grace)
	e.order = append(e.order, "stop")
	e.mu2.Unlock()
	return nil
}
func (e *podEngine) Exec(ctx context.Context, uid string, argv []string, d time.Duration) (runtime.ExecResult, error) {
	e.mu2.Lock()
	e.order = append(e.order, "exec:"+argv[0])
	e.mu2.Unlock()
	return e.fakeExec.Exec(ctx, uid, argv, d)
}

func reconcilePod(c func(*corev1.Container)) *corev1.Pod {
	p := testPod()
	p.Spec.NodeName = "node-1"
	p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
	c(&p.Spec.Containers[0])
	return p
}

func TestReconcileRestartsAContainerOncePerStartWhenLivenessFails(t *testing.T) {
	p := reconcilePod(func(c *corev1.Container) {
		c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}}, FailureThreshold: 2}
	})
	client := fake.NewSimpleClientset(p)
	engine := &podEngine{started: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	engine.fakeExec.code = 1
	probes := map[string]*probeState{}
	for i := 0; i < 5; i++ {
		if probes["unique"] != nil {
			probes["unique"].liveness.next = time.Time{}
		}
		if err := reconcile(context.Background(), client, "node-1", engine, probes, nodeSettings{}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // the restart request is made in the background
	engine.mu2.Lock()
	kills := append([]string(nil), engine.kills...)
	engine.mu2.Unlock()
	if len(kills) != 1 || kills[0] != "Liveness probe failed: command \"false\" exited with 1:" {
		t.Fatalf("one restart request per container start, however long it keeps failing: %q", kills)
	}
	events, _ := client.CoreV1().Events("test").List(context.Background(), metav1.ListOptions{})
	reasons := map[string]int{}
	for _, e := range events.Items {
		reasons[e.Reason]++
	}
	if reasons[ReasonUnhealthy] != 1 || reasons[ReasonKilling] != 1 {
		t.Fatalf("the same failure is reported once, and the restart once: %v", reasons)
	}
	got, _ := client.CoreV1().Pods("test").Get(context.Background(), "test", metav1.GetOptions{})
	if cs := got.Status.ContainerStatuses; len(cs) == 0 || cs[0].Started == nil || !*cs[0].Started {
		t.Fatalf("status: %+v", got.Status)
	}
}

func TestDeletingAPodRunsPreStopThenStopsGracefullyThenRemovesIt(t *testing.T) {
	now := metav1.Now()
	grace := int64(10)
	p := reconcilePod(func(c *corev1.Container) {
		c.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"drain"}}}}
	})
	p.DeletionTimestamp = &now
	p.DeletionGracePeriodSeconds = &grace
	client := fake.NewSimpleClientset(p)
	engine := &podEngine{started: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	probes := map[string]*probeState{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := reconcile(context.Background(), client, "node-1", engine, probes, nodeSettings{}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.CoreV1().Pods("test").Get(context.Background(), "test", metav1.GetOptions{}); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := client.CoreV1().Pods("test").Get(context.Background(), "test", metav1.GetOptions{}); err == nil {
		t.Fatal("the Pod object must be removed once its workload has stopped")
	}
	engine.mu2.Lock()
	defer engine.mu2.Unlock()
	if len(engine.order) != 2 || engine.order[0] != "exec:drain" || engine.order[1] != "stop" {
		t.Fatalf("preStop runs first, then the stop: %v", engine.order)
	}
	if len(engine.stopped) != 1 || engine.stopped[0] > 10*time.Second || engine.stopped[0] < 8*time.Second {
		t.Fatalf("the stop gets what is left of the grace period (10s minus the hook): %v", engine.stopped)
	}
}

func TestPodGraceUsesTheDeletionRequestFirst(t *testing.T) {
	p := testPod()
	if podGrace(p) != 30*time.Second {
		t.Fatalf("default: %s", podGrace(p))
	}
	spec := int64(7)
	p.Spec.TerminationGracePeriodSeconds = &spec
	if podGrace(p) != 7*time.Second {
		t.Fatalf("spec: %s", podGrace(p))
	}
	del := int64(0)
	p.DeletionGracePeriodSeconds = &del
	if podGrace(p) != 0 {
		t.Fatalf("a deletion with --grace-period=0: %s", podGrace(p))
	}
}
