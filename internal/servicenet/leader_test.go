package servicenet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	coordination "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func fastElection(identity string) ElectionConfig {
	return ElectionConfig{Namespace: "sys", Name: "lock", Identity: identity, LeaseDuration: 1500 * time.Millisecond, RenewDeadline: 1000 * time.Millisecond, RetryPeriod: 100 * time.Millisecond}
}

func TestElectionConfigIsValidated(t *testing.T) {
	good := fastElection("a")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ElectionConfig){
		"no namespace":   func(c *ElectionConfig) { c.Namespace = "" },
		"no name":        func(c *ElectionConfig) { c.Name = "" },
		"no identity":    func(c *ElectionConfig) { c.Identity = "" },
		"renew >= lease": func(c *ElectionConfig) { c.RenewDeadline = c.LeaseDuration },
		"retry >= renew": func(c *ElectionConfig) { c.RetryPeriod = c.RenewDeadline },
		"zero retry":     func(c *ElectionConfig) { c.RetryPeriod = 0 },
	} {
		c := good
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	d := DefaultElection()
	d.Namespace, d.Identity = "ns", "me"
	if err := d.Validate(); err != nil || d.LeaseDuration != 15*time.Second || d.RenewDeadline != 10*time.Second || d.RetryPeriod != 2*time.Second {
		t.Fatalf("the recommended defaults: %+v %v", d, err)
	}
}

// replica runs one elector whose work records when it starts and stops.
type replica struct {
	id      string
	cancel  context.CancelFunc
	done    chan struct{}
	working atomic.Bool
	started atomic.Int32
	health  *Health
}

func startReplica(t *testing.T, client *fake.Clientset, id string) *replica {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{id: id, cancel: cancel, done: make(chan struct{}), health: NewHealth(time.Minute)}
	go func() {
		defer close(r.done)
		RunElected(ctx, client, fastElection(id), r.health, func(c context.Context) {
			r.working.Store(true)
			r.started.Add(1)
			<-c.Done()
			r.working.Store(false)
		}, nil)
	}()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestOnlyOneReplicaWorksAndAnotherTakesOver(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := startReplica(t, client, "replica-a")
	waitFor(t, "a to lead", 5*time.Second, a.working.Load)
	b := startReplica(t, client, "replica-b")
	time.Sleep(600 * time.Millisecond) // long enough for b to try and fail to take over
	if b.working.Load() || b.started.Load() != 0 {
		t.Fatal("a second replica worked while the first led")
	}
	if !a.health.leaderNow() || b.health.leaderNow() {
		t.Fatal("the health view of leadership is wrong")
	}
	// The leader shuts down: it lets go of the Lease, so the other takes over quickly, not after a full lease timeout.
	begin := time.Now()
	a.cancel()
	<-a.done
	waitFor(t, "b to take over", 5*time.Second, b.working.Load)
	if took := time.Since(begin); took > 1400*time.Millisecond {
		t.Fatalf("hand-over took %s; releasing the Lease on shutdown should not wait for it to expire", took)
	}
	if a.working.Load() {
		t.Fatal("the stopped leader is still working")
	}
}

func (h *Health) leaderNow() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.leader }

func TestALeaderThatCrashesIsReplacedAfterTheLeaseExpires(t *testing.T) {
	client := fake.NewSimpleClientset()
	// What a crashed leader leaves behind: a Lease held by someone who no longer renews it.
	old := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	holder, dur := "crashed", int32(1)
	_, err := client.CoordinationV1().Leases("sys").Create(context.Background(), &coordination.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "lock", Namespace: "sys"},
		Spec:       coordination.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &dur, AcquireTime: &old, RenewTime: &old},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b := startReplica(t, client, "replica-b")
	waitFor(t, "b to take over an expired Lease", 6*time.Second, b.working.Load)
	l, _ := client.CoordinationV1().Leases("sys").Get(context.Background(), "lock", metav1.GetOptions{})
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "replica-b" {
		t.Fatalf("holder: %v", l.Spec.HolderIdentity)
	}
}

func TestHealthEndpoints(t *testing.T) {
	h := NewHealth(30 * time.Second)
	now := time.Now()
	h.now = func() time.Time { return now }
	h.Beat()
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := call("/healthz"); w.Code != http.StatusOK {
		t.Fatalf("a fresh heartbeat is healthy: %d", w.Code)
	}
	now = now.Add(time.Minute)
	if w := call("/healthz"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a stale heartbeat must fail the liveness probe: %d", w.Code)
	}
	h.Beat()
	// A standby is healthy but is not the leader.
	if w := call("/healthz"); w.Code != http.StatusOK {
		t.Fatalf("standby: %d", w.Code)
	}
	if w := call("/leader"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("not leading: %d", w.Code)
	}
	h.SetLeader(true)
	h.ReconcileDone(nil)
	w := call("/leader")
	var body map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["leader"] != true || body["lastError"] != "" {
		t.Fatalf("leading: %d %s", w.Code, w.Body.String())
	}
	h.ReconcileDone(context.DeadlineExceeded)
	call("/leader")
	json.Unmarshal(call("/leader").Body.Bytes(), &body)
	if body["lastError"] == "" {
		t.Fatal("the last reconcile error must be visible")
	}
}
