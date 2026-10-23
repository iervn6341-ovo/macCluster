package cluster

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	runtime "maccluster/internal/runtime"
)

type fakeExec struct {
	mu    sync.Mutex
	code  int
	out   string
	err   error
	calls [][]string
}

func (f *fakeExec) Exec(_ context.Context, _ string, argv []string, _ time.Duration) (runtime.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	return runtime.ExecResult{ExitCode: f.code, Output: f.out}, f.err
}

func (f *fakeExec) set(code int, out string) {
	f.mu.Lock()
	f.code, f.out = code, out
	f.mu.Unlock()
}

func listener(t *testing.T) (int, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, func() { l.Close() }
}

func probePod(c func(*corev1.Container)) *corev1.Pod {
	p := testPod()
	p.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "web", ContainerPort: 8080}}
	c(&p.Spec.Containers[0])
	return p
}

func runningInstance(port int) runtime.Instance {
	return runtime.Instance{Phase: "Running", Port: port, StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
}

func TestTCPProbeFollowsThePortBeingOpen(t *testing.T) {
	port, closeIt := listener(t)
	p := probePod(func(c *corev1.Container) {
		c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("web")}}, FailureThreshold: 1}
	})
	st := &probeState{}
	i := runningInstance(port)
	if out := st.evaluate(context.Background(), p, i); !out.ready {
		t.Fatalf("an open port is ready: %+v", out)
	}
	closeIt()
	st.readiness.next = time.Time{}
	out := st.evaluate(context.Background(), p, i)
	if out.ready || !strings.Contains(out.message, "connection refused") {
		t.Fatalf("a closed port is not ready and says why: %+v", out)
	}
	if len(out.notes) != 1 || out.notes[0].Reason != ReasonUnhealthy || !strings.HasPrefix(out.notes[0].Message, "Readiness probe failed:") {
		t.Fatalf("each failed attempt produces an Unhealthy event: %+v", out.notes)
	}
}

func TestHTTPSProbeAcceptsASelfSignedCertificateLikeKubelet(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "kube-probe/1.28" || r.Header.Get("X-Check") != "1" {
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	_, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	p := probePod(func(c *corev1.Container) {
		c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Scheme: corev1.URISchemeHTTPS, Path: "/", Port: intstr.FromInt32(8080), HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Check", Value: "1"}}}}}
	})
	if out := (&probeState{}).evaluate(context.Background(), p, runningInstance(port)); !out.ready {
		t.Fatalf("%+v", out)
	}
}

func TestExecProbeUsesTheRuntimesExecAndItsExitCode(t *testing.T) {
	ex := &fakeExec{}
	p := probePod(func(c *corev1.Container) {
		c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"cat", "/tmp/ready"}}}, FailureThreshold: 1}
	})
	st := &probeState{exec: ex}
	i := runningInstance(1)
	if out := st.evaluate(context.Background(), p, i); !out.ready || len(ex.calls) != 1 || strings.Join(ex.calls[0], " ") != "cat /tmp/ready" {
		t.Fatalf("exit 0 is ready: %+v %v", out, ex.calls)
	}
	ex.set(1, "no such file")
	st.readiness.next = time.Time{}
	out := st.evaluate(context.Background(), p, i)
	if out.ready || !strings.Contains(out.message, `exited with 1: no such file`) {
		t.Fatalf("%+v", out)
	}
	st2 := &probeState{} // a runtime without exec
	if out := st2.evaluate(context.Background(), p, i); out.ready || !strings.Contains(out.message, "cannot run commands") {
		t.Fatalf("%+v", out)
	}
	ex.err = errors.New("timed out after 1s")
	st.readiness.next = time.Time{}
	if out := st.evaluate(context.Background(), p, i); out.ready || !strings.Contains(out.message, "timed out") {
		t.Fatalf("a timeout is a failure with its reason: %+v", out)
	}
}

func TestLivenessFailureAsksForARestartOnlyAfterTheFailureThreshold(t *testing.T) {
	ex := &fakeExec{code: 1}
	p := probePod(func(c *corev1.Container) {
		c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}}, FailureThreshold: 3}
	})
	st := &probeState{exec: ex}
	i := runningInstance(1)
	for n := 1; n <= 3; n++ {
		st.liveness.next = time.Time{}
		out := st.evaluate(context.Background(), p, i)
		if n < 3 && out.kill != "" {
			t.Fatalf("failure %d of 3 must not restart yet: %+v", n, out)
		}
		if n == 3 {
			if !strings.HasPrefix(out.kill, "Liveness probe failed:") {
				t.Fatalf("the third failure restarts: %+v", out)
			}
			var killing bool
			for _, note := range out.notes {
				killing = killing || (note.Reason == ReasonKilling && strings.Contains(note.Message, "failed its liveness probe, will be restarted"))
			}
			if !killing {
				t.Fatalf("the restart is announced: %+v", out.notes)
			}
		}
	}
	// A new start of the container starts everything over.
	i.StartedAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	i.Restarts = 1
	st.liveness.next = time.Time{}
	if out := st.evaluate(context.Background(), p, i); out.kill != "" {
		t.Fatalf("the failure count belongs to the earlier run: %+v", out)
	}
}

func TestStartupProbeGatesTheOtherProbesAndCanKill(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	_, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	ex := &fakeExec{code: 1} // liveness would fail, but must not run before startup passes
	p := probePod(func(c *corev1.Container) {
		c.StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(8080)}}, FailureThreshold: 3}
		c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}}, FailureThreshold: 1}
		c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(8080)}}}
	})
	st := &probeState{exec: ex}
	i := runningInstance(port)
	for n := 0; n < 2; n++ {
		st.startup.next = time.Time{}
		out := st.evaluate(context.Background(), p, i)
		if out.started || out.ready || out.kill != "" || len(ex.calls) != 0 {
			t.Fatalf("while starting up nothing else runs and nothing is killed yet: %+v calls=%v", out, ex.calls)
		}
	}
	st.startup.next = time.Time{}
	if out := st.evaluate(context.Background(), p, i); !strings.HasPrefix(out.kill, "Startup probe failed") {
		t.Fatalf("a program that never starts is restarted: %+v", out)
	}

	st = &probeState{exec: ex}
	up.Store(true)
	out := st.evaluate(context.Background(), p, i)
	if !out.started || len(ex.calls) != 1 || out.kill == "" && false {
		t.Fatalf("once started, liveness runs: %+v calls=%v", out, ex.calls)
	}
	if !strings.HasPrefix(out.kill, "Liveness probe failed") {
		t.Fatalf("and it is judged: %+v", out)
	}
}

func TestPostStartHookGatesReadinessAndFailureRestarts(t *testing.T) {
	ex := &fakeExec{}
	p := probePod(func(c *corev1.Container) {
		c.Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"init-things"}}}}
	})
	st := &probeState{exec: ex}
	i := runningInstance(1)
	var out probeOutcome
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out = st.evaluate(context.Background(), p, i)
		if out.ready {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !out.ready || !out.started || len(ex.calls) != 1 {
		t.Fatalf("the hook runs once per start and then the Pod is ready: %+v calls=%v", out, ex.calls)
	}
	ex.set(2, "boom")
	i2 := runningInstance(1)
	i2.StartedAt = time.Now().UTC().Format(time.RFC3339)
	i2.Restarts = 1
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out = st.evaluate(context.Background(), p, i2)
		if out.kill != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasPrefix(out.kill, "FailedPostStartHook:") || !strings.Contains(out.kill, "exited with 2: boom") {
		t.Fatalf("a failed postStart hook restarts the container: %+v", out)
	}
	var warned bool
	for _, n := range out.notes {
		warned = warned || (n.Reason == ReasonFailedPostStart && n.Type == corev1.EventTypeWarning)
	}
	if !warned {
		t.Fatalf("and says so: %+v", out.notes)
	}
}

func TestProbesAndHooksAreValidatedAgainstTheDeclaredPorts(t *testing.T) {
	ok := probePod(func(c *corev1.Container) {
		c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("web")}}}
		c.StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
		c.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: 1}}}
	})
	if _, err := Translate(ok); err != nil {
		t.Fatalf("liveness, startup and lifecycle are supported now: %v", err)
	}
	for name, edit := range map[string]func(*corev1.Container){
		"wrong port": func(c *corev1.Container) { c.LivenessProbe.TCPSocket.Port = intstr.FromInt32(9999) },
		"host":       func(c *corev1.Container) { c.LivenessProbe.TCPSocket.Host = "example.com" },
		"empty exec": func(c *corev1.Container) { c.StartupProbe.Exec.Command = nil },
		"two handlers": func(c *corev1.Container) {
			c.StartupProbe.TCPSocket = &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}
		},
		"hook http": func(c *corev1.Container) {
			c.Lifecycle.PostStart = &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(1)}}
		},
	} {
		p := probePod(func(c *corev1.Container) {
			c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("web")}}}
			c.StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
			c.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: 1}}}
			edit(c)
		})
		var se *SpecError
		if _, err := Translate(p); !errors.As(err, &se) || se.Reason != ReasonInvalidPodSpec {
			t.Errorf("%s: got %v", name, err)
		}
	}
	grpc := probePod(func(c *corev1.Container) {
		c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 9}}}
	})
	var se *SpecError
	if _, err := Translate(grpc); !errors.As(err, &se) || se.Reason != ReasonUnsupportedPodSpec || !strings.Contains(err.Error(), "grpc") {
		t.Fatalf("grpc is named as unsupported: %v", err)
	}
}
