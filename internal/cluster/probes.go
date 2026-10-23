package cluster

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	runtime "maccluster/internal/runtime"
)

const (
	ReasonUnhealthy          = "Unhealthy"
	ReasonKilling            = "Killing"
	ReasonFailedPostStart    = "FailedPostStartHook"
	ReasonFailedPreStop      = "FailedPreStopHook"
	defaultProbeTimeout      = time.Second
	defaultGracePeriod       = 30 * time.Second
	minimumStopGrace         = 2 * time.Second
	hookTimeoutWhenNoGrace   = 30 * time.Second
	probeKindStartup         = "Startup"
	probeKindLiveness        = "Liveness"
	probeKindReadiness       = "Readiness"
	maxProbeMessageRunes     = 300
	userAgentOfProbes        = "kube-probe/1.28"
	unsupportedExecCapabilty = "this runtime cannot run commands in a workload"
)

// action is what a probe or a lifecycle hook does: one of a command, an HTTP request, a TCP connection or a sleep.
type action struct {
	exec  []string
	http  *corev1.HTTPGetAction
	tcp   *corev1.TCPSocketAction
	sleep *time.Duration
}

func actionOfProbe(h corev1.ProbeHandler) action {
	return action{exec: execCommand(h.Exec), http: h.HTTPGet, tcp: h.TCPSocket}
}

func actionOfHook(h *corev1.LifecycleHandler) action {
	if h == nil {
		return action{}
	}
	a := action{exec: execCommand(h.Exec), http: h.HTTPGet, tcp: h.TCPSocket}
	if h.Sleep != nil {
		d := time.Duration(h.Sleep.Seconds) * time.Second
		a.sleep = &d
	}
	return a
}

func execCommand(e *corev1.ExecAction) []string {
	if e == nil {
		return nil
	}
	return e.Command
}

func (a action) defined() bool {
	return len(a.exec) > 0 || a.http != nil || a.tcp != nil || a.sleep != nil
}

// handlerEnv is what an action needs to reach the workload.
type handlerEnv struct {
	exec runtime.Execer
	uid  string
	inst runtime.Instance
	c    corev1.Container
}

// hostPort maps a port the Pod declares to the port on this Mac: a native program that binds fixed ports uses them
// as they are; everything else (Docker's published port, a native program's allocated port) is the instance's port.
func (e handlerEnv) hostPort(p intstr.IntOrString) (int, error) {
	port, ok := resolveProbePort(p, e.c)
	if !ok {
		return 0, fmt.Errorf("port %s is not declared", p.String())
	}
	if containsPort(e.inst.Ports, port) {
		return port, nil
	}
	if e.inst.Port <= 0 {
		return 0, fmt.Errorf("no port is mapped for the container yet")
	}
	return e.inst.Port, nil
}

func shorten(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxProbeMessageRunes {
		return string(r[:maxProbeMessageRunes]) + "..."
	}
	return s
}

// run performs the action and says whether it succeeded, with the reason when it did not.
func (a action) run(ctx context.Context, env handlerEnv, timeout time.Duration, httpOKBelow int) (bool, string) {
	switch {
	case a.sleep != nil:
		select {
		case <-time.After(*a.sleep):
			return true, ""
		case <-ctx.Done():
			return false, ctx.Err().Error()
		}
	case len(a.exec) > 0:
		if env.exec == nil {
			return false, unsupportedExecCapabilty
		}
		res, err := env.exec.Exec(ctx, env.uid, a.exec, timeout)
		if err != nil {
			return false, shorten(err.Error())
		}
		if res.ExitCode != 0 {
			return false, shorten(fmt.Sprintf("command %q exited with %d: %s", strings.Join(a.exec, " "), res.ExitCode, res.Output))
		}
		return true, ""
	case a.tcp != nil:
		port, err := env.hostPort(a.tcp.Port)
		if err != nil {
			return false, err.Error()
		}
		conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return false, shorten(err.Error())
		}
		conn.Close()
		return true, ""
	case a.http != nil:
		return a.runHTTP(ctx, env, timeout, httpOKBelow)
	}
	return false, "no action"
}

func (a action) runHTTP(ctx context.Context, env handlerEnv, timeout time.Duration, okBelow int) (bool, string) {
	h := a.http
	port, err := env.hostPort(h.Port)
	if err != nil {
		return false, err.Error()
	}
	scheme := "http"
	if h.Scheme == corev1.URISchemeHTTPS {
		scheme = "https"
	}
	path := h.Path
	if path == "" {
		path = "/"
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Path: strings.SplitN(path, "?", 2)[0]}
	if parts := strings.SplitN(path, "?", 2); len(parts) == 2 {
		u.RawQuery = parts[1]
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("User-Agent", userAgentOfProbes)
	for _, hd := range h.HTTPHeaders {
		if strings.EqualFold(hd.Name, "Host") {
			req.Host = hd.Value
		} else {
			req.Header.Add(hd.Name, hd.Value)
		}
	}
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// Like kubelet, a probe does not verify the certificate of the program it asks.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
	}
	res, err := client.Do(req)
	if err != nil {
		return false, shorten(err.Error())
	}
	res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < okBelow {
		return true, ""
	}
	return false, fmt.Sprintf("HTTP probe failed with statuscode: %d", res.StatusCode)
}

// probeRun is the bookkeeping of one probe for one run of the container.
type probeRun struct {
	started             string // the container start this belongs to
	next                time.Time
	successes, failures int
	ok                  bool
	message             string
	lastFailure         string // set when the latest attempt failed, for the event
}

func (r *probeRun) reset(started string, initiallyOK bool) {
	r.started, r.next, r.successes, r.failures, r.ok, r.message, r.lastFailure = started, time.Time{}, 0, 0, initiallyOK, "", ""
}

func thresholds(p *corev1.Probe) (success, failure int) {
	success, failure = int(p.SuccessThreshold), int(p.FailureThreshold)
	if success <= 0 {
		success = 1
	}
	if failure <= 0 {
		failure = 3
	}
	return
}

// step runs the probe if it is due (after the initial delay, once per period) and updates its state with the
// success and failure thresholds. A liveness probe starts out healthy; readiness and startup start out not ok.
func (r *probeRun) step(ctx context.Context, probe *corev1.Probe, env handlerEnv, initiallyOK bool) {
	if r.started != env.inst.StartedAt {
		r.reset(env.inst.StartedAt, initiallyOK)
	}
	r.lastFailure = ""
	now := time.Now()
	started, _ := time.Parse(time.RFC3339, env.inst.StartedAt)
	if now.Before(started.Add(time.Duration(probe.InitialDelaySeconds) * time.Second)) {
		if r.message == "" {
			r.message = "waiting for the initial delay"
		}
		return
	}
	if now.Before(r.next) {
		return
	}
	period := time.Duration(probe.PeriodSeconds) * time.Second
	if period <= 0 {
		period = 10 * time.Second
	}
	r.next = now.Add(period)
	timeout := time.Duration(probe.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	good, why := actionOfProbe(probe.ProbeHandler).run(pctx, env, timeout, 400)
	cancel()
	successThreshold, failureThreshold := thresholds(probe)
	if good {
		r.successes++
		r.failures = 0
		if r.successes >= successThreshold {
			r.ok = true
		}
		r.message = ""
		return
	}
	r.failures++
	r.successes = 0
	if r.failures >= failureThreshold {
		r.ok = false
	}
	r.message, r.lastFailure = why, why
}

// hookRun is one execution of a lifecycle hook, which runs in the background.
type hookRun struct {
	runFor string // the container start it belongs to
	done   bool
	err    string
}

// probeOutcome is what one evaluation of a Pod's probes and postStart hook decided.
type probeOutcome struct {
	started bool   // startup probe passed (or there is none) and postStart finished
	ready   bool   // started and the readiness probe passes (or there is none)
	message string // why not, for the Pod's status message
	kill    string // restart the container, with this as the reason
	notes   []Note // Unhealthy events for the attempts that failed
}

func killNotes(kind string, c corev1.Container, restarts int) []Note {
	return []Note{{corev1.EventTypeNormal, ReasonKilling, fmt.Sprintf("Container %s failed its %s probe, will be restarted (restart %d)", c.Name, strings.ToLower(kind), restarts+1)}}
}

// evaluate runs what is due and decides. Startup gates the others; liveness can ask for a restart; readiness
// decides Ready. Nothing runs, and everything starts over, whenever the container is not running.
func (st *probeState) evaluate(ctx context.Context, p *corev1.Pod, i runtime.Instance) probeOutcome {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := probeOutcome{started: true, ready: true}
	if len(p.Spec.Containers) == 0 {
		return out
	}
	c := p.Spec.Containers[0]
	if i.Phase != "Running" {
		st.startup.started, st.liveness.started, st.readiness.started = "", "", ""
		st.post.runFor = ""
		return probeOutcome{}
	}
	env := handlerEnv{exec: st.exec, uid: string(p.UID), inst: i, c: c}

	if hook := actionOfHook(postStart(c)); hook.defined() {
		if st.post.runFor != i.StartedAt {
			st.post = hookRun{runFor: i.StartedAt}
			run := st.post.runFor
			go func() {
				hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), podGrace(p)+hookTimeoutWhenNoGrace)
				defer cancel()
				ok, why := hook.run(hctx, env, podGrace(p), 400)
				st.mu.Lock()
				defer st.mu.Unlock()
				if st.post.runFor == run {
					st.post.done = true
					if !ok {
						st.post.err = why
					}
				}
			}()
		}
		if !st.post.done {
			return probeOutcome{message: "the postStart hook is running"}
		}
		if st.post.err != "" {
			out.notes = append(out.notes, Note{corev1.EventTypeWarning, ReasonFailedPostStart, "postStart hook failed: " + st.post.err})
			out.kill = "FailedPostStartHook: " + st.post.err
			out.notes = append(out.notes, Note{corev1.EventTypeNormal, ReasonKilling, fmt.Sprintf("Container %s failed its postStart hook, will be restarted (restart %d)", c.Name, i.Restarts+1)})
			return probeOutcome{message: out.kill, kill: out.kill, notes: out.notes}
		}
	}

	if c.StartupProbe != nil {
		st.startup.step(ctx, c.StartupProbe, env, false)
		if st.startup.lastFailure != "" {
			out.notes = append(out.notes, Note{corev1.EventTypeWarning, ReasonUnhealthy, probeKindStartup + " probe failed: " + st.startup.lastFailure})
		}
		if _, failureThreshold := thresholds(c.StartupProbe); !st.startup.ok && st.startup.failures >= failureThreshold {
			out.kill = probeKindStartup + " probe failed: " + st.startup.message
			out.notes = append(out.notes, killNotes(probeKindStartup, c, i.Restarts)...)
			return probeOutcome{message: out.kill, kill: out.kill, notes: out.notes}
		}
		if !st.startup.ok {
			msg := st.startup.message
			if msg == "" {
				msg = "the startup probe has not succeeded yet"
			}
			return probeOutcome{message: msg, notes: out.notes}
		}
	}

	if c.LivenessProbe != nil {
		st.liveness.step(ctx, c.LivenessProbe, env, true)
		if st.liveness.lastFailure != "" {
			out.notes = append(out.notes, Note{corev1.EventTypeWarning, ReasonUnhealthy, probeKindLiveness + " probe failed: " + st.liveness.lastFailure})
		}
		if _, failureThreshold := thresholds(c.LivenessProbe); !st.liveness.ok && st.liveness.failures >= failureThreshold {
			out.kill = probeKindLiveness + " probe failed: " + st.liveness.message
			out.notes = append(out.notes, killNotes(probeKindLiveness, c, i.Restarts)...)
			return probeOutcome{started: true, message: out.kill, kill: out.kill, notes: out.notes}
		}
	}

	if c.ReadinessProbe != nil {
		st.readiness.step(ctx, c.ReadinessProbe, env, false)
		if st.readiness.lastFailure != "" {
			out.notes = append(out.notes, Note{corev1.EventTypeWarning, ReasonUnhealthy, probeKindReadiness + " probe failed: " + st.readiness.lastFailure})
		}
		out.ready, out.message = st.readiness.ok, st.readiness.message
	}
	return out
}

func postStart(c corev1.Container) *corev1.LifecycleHandler {
	if c.Lifecycle == nil {
		return nil
	}
	return c.Lifecycle.PostStart
}

func preStop(c corev1.Container) *corev1.LifecycleHandler {
	if c.Lifecycle == nil {
		return nil
	}
	return c.Lifecycle.PreStop
}

// podGrace is how long the Pod may take to stop: what a deletion asked for, else the Pod's own setting.
func podGrace(p *corev1.Pod) time.Duration {
	switch {
	case p.DeletionGracePeriodSeconds != nil:
		return time.Duration(*p.DeletionGracePeriodSeconds) * time.Second
	case p.Spec.TerminationGracePeriodSeconds != nil:
		return time.Duration(*p.Spec.TerminationGracePeriodSeconds) * time.Second
	}
	return defaultGracePeriod
}

// stopState tracks stopping a Pod that is being deleted: the preStop hook and the graceful stop take as long as the
// grace period, so they run in the background and the reconcile loop keeps serving every other Pod.
type stopState int

const (
	stopIdle stopState = iota
	stopRunning
	stopDone
)

type stopTracker struct {
	mu    sync.Mutex
	state stopState
	err   error
}

// beginStop starts stopping the Pod in the background the first time it is called and reports whether it finished.
func (t *stopTracker) beginStop(ctx context.Context, p *corev1.Pod, engine runtime.Engine, ex runtime.Execer, i func() runtime.Instance, note func(Note)) (finished bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.state {
	case stopRunning:
		return false, nil
	case stopDone:
		return true, t.err
	}
	t.state = stopRunning
	grace := podGrace(p)
	go func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace+hookTimeoutWhenNoGrace)
		defer cancel()
		deadline := time.Now().Add(grace)
		if len(p.Spec.Containers) > 0 {
			c := p.Spec.Containers[0]
			if hook := actionOfHook(preStop(c)); hook.defined() {
				if inst := i(); inst.Phase == "Running" {
					env := handlerEnv{exec: ex, uid: string(p.UID), inst: inst, c: c}
					if ok, why := hook.run(bg, env, grace, 400); !ok {
						note(Note{corev1.EventTypeWarning, ReasonFailedPreStop, "preStop hook failed: " + why})
					}
				}
			}
		}
		remaining := time.Until(deadline)
		if remaining < minimumStopGrace {
			remaining = minimumStopGrace
		}
		var err error
		if gs, ok := engine.(runtime.GraceStopper); ok {
			err = gs.StopGrace(bg, string(p.UID), remaining)
		} else {
			err = engine.Stop(bg, string(p.UID))
		}
		t.mu.Lock()
		t.state, t.err = stopDone, err
		t.mu.Unlock()
	}()
	return false, nil
}

// declaredPortOK checks that a probe or hook that connects to the program names a port the program has, the way the
// readiness probe always had to: the single declared port, or one of the fixed ports.
func declaredPortOK(what string, port intstr.IntOrString, c corev1.Container, fixed []int) error {
	p, ok := resolveProbePort(port, c)
	if len(fixed) > 0 {
		if !ok || !containsPort(fixed, p) {
			return invalid("%s: port is not in %s", what, FixedPortsLabel)
		}
		return nil
	}
	if len(c.Ports) != 1 {
		return invalid("%s: must target the single declared container port (declare one containerPort)", what)
	}
	only := c.Ports[0]
	if (port.StrVal != "" && port.StrVal != only.Name) || (port.StrVal == "" && port.IntVal != only.ContainerPort) {
		return invalid("%s: port does not match the declared container port", what)
	}
	return nil
}

func validateAction(what string, a action, c corev1.Container, fixed []int, count int) error {
	if count != 1 {
		return invalid("%s: exactly one of exec, httpGet, tcpSocket%s is required", what, map[bool]string{true: " or sleep", false: ""}[strings.HasPrefix(what, "lifecycle")])
	}
	switch {
	case a.http != nil:
		if a.http.Host != "" {
			return invalid("%s: httpGet cannot override the host", what)
		}
		return declaredPortOK(what, a.http.Port, c, fixed)
	case a.tcp != nil:
		if a.tcp.Host != "" {
			return invalid("%s: tcpSocket cannot override the host", what)
		}
		return declaredPortOK(what, a.tcp.Port, c, fixed)
	case a.sleep != nil:
		if *a.sleep < 0 {
			return invalid("%s: sleep seconds must not be negative", what)
		}
	}
	return nil
}

func countHandlers(a action) int {
	n := 0
	if len(a.exec) > 0 {
		n++
	}
	if a.http != nil {
		n++
	}
	if a.tcp != nil {
		n++
	}
	if a.sleep != nil {
		n++
	}
	return n
}

// validateProbesAndHooks accepts what the runtime can do and explains what it cannot in terms of the Pod's own fields.
func validateProbesAndHooks(c corev1.Container, fixed []int) error {
	for _, p := range []struct {
		what  string
		probe *corev1.Probe
	}{{"readinessProbe", c.ReadinessProbe}, {"livenessProbe", c.LivenessProbe}, {"startupProbe", c.StartupProbe}} {
		if p.probe == nil {
			continue
		}
		a := actionOfProbe(p.probe.ProbeHandler)
		count := countHandlers(a)
		if p.probe.Exec != nil && len(p.probe.Exec.Command) == 0 {
			return invalid("%s: exec.command is empty", p.what)
		}
		if err := validateAction(p.what, a, c, fixed, count); err != nil {
			return err
		}
	}
	if c.Lifecycle != nil {
		for _, h := range []struct {
			what string
			h    *corev1.LifecycleHandler
		}{{"lifecycle.postStart", c.Lifecycle.PostStart}, {"lifecycle.preStop", c.Lifecycle.PreStop}} {
			if h.h == nil {
				continue
			}
			a := actionOfHook(h.h)
			if h.h.Exec != nil && len(h.h.Exec.Command) == 0 {
				return invalid("%s: exec.command is empty", h.what)
			}
			if err := validateAction(h.what, a, c, fixed, countHandlers(a)); err != nil {
				return err
			}
		}
	}
	return nil
}
