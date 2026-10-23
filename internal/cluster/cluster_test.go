package cluster

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	runtime "maccluster/internal/runtime"
)

func TestJoinParsingRejectsShellAndUnsupportedFlags(t *testing.T) {
	valid := "kubeadm join 192.0.2.1:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:" + strings.Repeat("a", 64)
	if _, err := ParseJoin(valid); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{valid + "; touch /tmp/oops", valid + " --control-plane", strings.Replace(valid, "sha256:", "sha1:", 1), strings.Replace(valid, "abcdef.0123456789abcdef", "$TOKEN", 1), valid + " --token abcdef.0123456789abcdef"} {
		if _, err := ParseJoin(input); err == nil {
			t.Errorf("accepted unsafe or unsupported join")
		}
	}
}
func testPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "test", UID: "unique", Labels: map[string]string{ManagedLabel: "true"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "nginx:alpine"}}, RestartPolicy: corev1.RestartPolicyAlways}}
}
func TestTranslateDefaultAndOptIn(t *testing.T) {
	p := testPod()
	s, err := Translate(p)
	if err != nil || s.Executor != "docker" {
		t.Fatal(s, err)
	}
	p.Labels[ExecutorLabel] = "native"
	s, err = Translate(p)
	if err != nil || s.Executor != "native" {
		t.Fatal(s, err)
	}
	p.Labels[ExecutorLabel] = "typo"
	if _, err = Translate(p); err == nil {
		t.Fatal("unknown executor accepted")
	}
}
func TestUnsupportedFeaturesAreNotSilentlyIgnored(t *testing.T) {
	p := testPod()
	p.Spec.Volumes = []corev1.Volume{{Name: "unsupported"}}
	if _, err := Translate(p); err == nil {
		t.Fatal("volume ignored")
	}
	p = testPod()
	delete(p.Labels, ManagedLabel)
	if _, err := Translate(p); err == nil {
		t.Fatal("unmanaged workload accepted")
	}
}
func TestAlwaysRestartBacksOffAsRunningPodWithCrashLoopBackOff(t *testing.T) {
	p := testPod()
	s := makeStatus(context.Background(), p, runtime.Instance{Phase: "Failed", ExitCode: 1, Restarts: 2})
	if s.Phase != corev1.PodRunning {
		t.Fatalf("a container in back-off keeps its Pod Running, got %s", s.Phase)
	}
	cs := s.ContainerStatuses[0]
	if cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" || cs.Ready {
		t.Fatalf("state: %+v", cs.State)
	}
	if cs.LastTerminationState.Terminated == nil || cs.LastTerminationState.Terminated.ExitCode != 1 || cs.LastTerminationState.Terminated.Reason != "Error" {
		t.Fatalf("last state: %+v", cs.LastTerminationState)
	}
	p.Spec.RestartPolicy = corev1.RestartPolicyNever
	s = makeStatus(context.Background(), p, runtime.Instance{Phase: "Failed", ExitCode: 1})
	if s.Phase != corev1.PodFailed || s.ContainerStatuses[0].State.Terminated.Reason != "Error" {
		t.Fatalf("a Never Pod that failed is Failed with reason Error: %s %+v", s.Phase, s.ContainerStatuses[0].State)
	}
}

func TestHealthyPodStatusLooksLikeAKubeletsOwn(t *testing.T) {
	p := testPod()
	s := makeStatus(context.Background(), p, runtime.Instance{Phase: "Running", ContainerID: "abc", ImageID: "sha256:123", StartedAt: "2026-01-01T00:00:00Z", LastExitAt: "2026-01-01T00:00:00.5Z", LastExitCode: 3, Restarts: 1})
	if s.Reason != "" {
		t.Fatalf("a healthy Pod has no status.reason (kubectl would show it as STATUS): %q", s.Reason)
	}
	cs := s.ContainerStatuses[0]
	if cs.Started == nil || !*cs.Started || cs.ImageID != "docker://sha256:123" {
		t.Fatalf("started/imageID: %+v", cs)
	}
	if cs.LastTerminationState.Terminated == nil || cs.LastTerminationState.Terminated.ExitCode != 3 {
		t.Fatalf("the previous termination must be visible after a restart: %+v", cs.LastTerminationState)
	}
}

func TestTranslateNamesEveryUnsupportedFieldAtOnce(t *testing.T) {
	p := testPod()
	p.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}}}
	p.Spec.InitContainers = []corev1.Container{{Name: "init"}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "POD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}}}
	p.Spec.Containers[0].LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 9}}}
	p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}
	_, err := Translate(p)
	var se *SpecError
	if !errors.As(err, &se) || se.Reason != ReasonUnsupportedPodSpec {
		t.Fatalf("got %v", err)
	}
	for _, want := range []string{"initContainers", `volumes: "data" (persistentVolumeClaim)`, "env POD: fieldRef status.podIP is not available: Mac Pods have no Pod IP", "livenessProbe: grpc probes are not supported", "resources.limits.ephemeral-storage"} {
		found := false
		for _, f := range se.Fields {
			found = found || strings.Contains(f, want)
		}
		if !found {
			t.Errorf("the message must name %q; got %v", want, se.Fields)
		}
	}
}

func TestTranslateSeparatesMisuseFromMissingSupport(t *testing.T) {
	p := testPod()
	p.Labels[ExecutorLabel] = "docker"
	p.Labels[FixedPortsLabel] = "8100"
	_, err := Translate(p)
	var se *SpecError
	if !errors.As(err, &se) || se.Reason != ReasonInvalidPodSpec {
		t.Fatalf("a label that does not apply to this executor is InvalidPodSpec: %v", err)
	}
	delete(p.Labels, ManagedLabel)
	delete(p.Labels, FixedPortsLabel)
	_, err = Translate(p)
	if !errors.As(err, &se) || se.Reason != ReasonNotManaged {
		t.Fatalf("got %v", err)
	}
}

func TestHostNamespacesAreAcceptedForNativeAndRejectedForDocker(t *testing.T) {
	p := testPod()
	p.Spec.HostNetwork = true
	if _, err := Translate(p); err == nil || !strings.Contains(err.Error(), "hostNetwork") {
		t.Fatalf("Docker Desktop cannot share the host's network: %v", err)
	}
	p.Labels[ExecutorLabel] = "native"
	_, notes, err := TranslateWithNotes(p)
	if err != nil {
		t.Fatalf("a native program already runs in the host's namespaces: %v", err)
	}
	if len(notes) != 1 || notes[0].Reason != NoteHostNamespacesIgnored {
		t.Fatalf("notes: %+v", notes)
	}
}

func TestDefaultServiceAccountTokenVolumeIsIgnoredWithAWarning(t *testing.T) {
	p := testPod()
	p.Spec.Volumes = []corev1.Volume{{Name: "kube-api-access-abcde", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "kube-api-access-abcde", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"}}
	s, notes, err := TranslateWithNotes(p)
	if err != nil || s.Image != "nginx:alpine" {
		t.Fatalf("a Pod that only has the injected token volume must run: %+v %v", s, err)
	}
	if len(notes) != 1 || notes[0].Reason != NoteServiceAccountTokenNotMounted || notes[0].Type != corev1.EventTypeWarning {
		t.Fatalf("the user must be told the token is not there: %+v", notes)
	}
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}})
	if _, err := Translate(p); err == nil {
		t.Fatal("other volumes are still unsupported")
	}
}

func TestEachEventIsSentOnce(t *testing.T) {
	client := fake.NewSimpleClientset()
	p := testPod()
	state := &probeState{}
	for i := 0; i < 3; i++ {
		state.notify(context.Background(), client, p, "node-1", corev1.EventTypeWarning, ReasonUnsupportedPodSpec, "volumes: \"x\"")
	}
	state.notify(context.Background(), client, p, "node-1", corev1.EventTypeWarning, ReasonUnsupportedPodSpec, "something else")
	list, err := client.CoreV1().Events("test").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(list.Items) != 2 {
		t.Fatalf("one event per distinct message, got %d: %v", len(list.Items), err)
	}
	ev := list.Items[0]
	if ev.Type != corev1.EventTypeWarning || ev.Reason != ReasonUnsupportedPodSpec || ev.InvolvedObject.Kind != "Pod" || ev.InvolvedObject.UID != "unique" || ev.Source.Host != "node-1" {
		t.Fatalf("event: %+v", ev)
	}
}

func TestTranslateEnvironmentLabel(t *testing.T) {
	p := testPod()
	p.Labels[EnvironmentLabel] = "vllm-metal"
	if _, err := Translate(p); err == nil {
		t.Fatal("environment accepted for default docker executor")
	}
	p.Labels[ExecutorLabel] = "native"
	s, err := Translate(p)
	if err != nil || s.Environment != "vllm-metal" {
		t.Fatal(s, err)
	}
	p.Labels[EnvironmentLabel] = "Bad/Name"
	if _, err = Translate(p); err == nil {
		t.Fatal("invalid environment name accepted")
	}
}

type fakeEnvEngine struct {
	runtime.Engine
	names []string
}

func (f *fakeEnvEngine) Environments() []string { return f.names }

func TestSyncEnvLabelsAddsAndRemoves(t *testing.T) {
	ctx := context.Background()
	var patches []string
	patch := func(_ context.Context, b []byte) error { patches = append(patches, string(b)); return nil }
	eng := &fakeEnvEngine{names: []string{"py"}}
	applied, err := syncEnvLabels(ctx, patch, eng, map[string]bool{"old": true})
	want := `{"metadata":{"labels":{"env.maccluster.example.com/old":null,"env.maccluster.example.com/py":"true"}}}`
	if err != nil || !applied["py"] || applied["old"] || len(patches) != 1 || patches[0] != want {
		t.Fatal(applied, err, patches)
	}
	// Unchanged set sends nothing; a failed patch keeps the previous set to retry.
	if _, err = syncEnvLabels(ctx, patch, eng, applied); err != nil || len(patches) != 1 {
		t.Fatal(err, patches)
	}
	eng.names = nil
	failing := func(context.Context, []byte) error { return context.DeadlineExceeded }
	kept, err := syncEnvLabels(ctx, failing, eng, applied)
	if err == nil || !kept["py"] {
		t.Fatal(kept, err)
	}
}

func fixedPod(label string, ports []int32, probePort intstr.IntOrString) *corev1.Pod {
	p := testPod()
	p.Labels[ExecutorLabel] = "native"
	if label != "" {
		p.Labels[FixedPortsLabel] = label
	}
	c := &p.Spec.Containers[0]
	c.Command = []string{"/bin/true"}
	for i, port := range ports {
		c.Ports = append(c.Ports, corev1.ContainerPort{ContainerPort: port, Name: []string{"api", "metrics", "admin"}[i%3]})
	}
	if probePort.Type != 0 || probePort.IntVal != 0 || probePort.StrVal != "" {
		c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: probePort}}}
	}
	return p
}

func TestTranslateFixedPorts(t *testing.T) {
	s, err := Translate(fixedPod("8100_9100", []int32{8100, 9100}, intstr.FromInt32(8100)))
	if err != nil || len(s.FixedPorts) != 2 || s.FixedPorts[0] != 8100 || s.FixedPorts[1] != 9100 || s.ContainerPort != 8100 {
		t.Fatalf("%+v %v", s, err)
	}
	// A named probe port resolves through the declared container ports.
	if _, err := Translate(fixedPod("8100_9100", []int32{8100, 9100}, intstr.FromString("metrics"))); err != nil {
		t.Fatal(err)
	}
	// The probe may target a fixed port that is not declared as containerPort.
	if _, err := Translate(fixedPod("8100_9100", []int32{8100}, intstr.FromInt32(9100))); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*corev1.Pod{
		"not a number":          fixedPod("abc", nil, intstr.IntOrString{}),
		"zero":                  fixedPod("0", nil, intstr.IntOrString{}),
		"too large":             fixedPod("65536", nil, intstr.IntOrString{}),
		"leading zero":          fixedPod("08100", nil, intstr.IntOrString{}),
		"comma separator":       fixedPod("8100,9100", nil, intstr.IntOrString{}),
		"duplicate":             fixedPod("8100_8100", nil, intstr.IntOrString{}),
		"empty element":         fixedPod("8100_", nil, intstr.IntOrString{}),
		"nine ports":            fixedPod("1_2_3_4_5_6_7_8_9", nil, intstr.IntOrString{}),
		"containerPort missing": fixedPod("8100", []int32{8100, 9100}, intstr.IntOrString{}),
		"probe port missing":    fixedPod("8100", []int32{8100}, intstr.FromInt32(9100)),
		"unknown named probe":   fixedPod("8100", []int32{8100}, intstr.FromString("nope")),
	} {
		if _, err := Translate(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	docker := fixedPod("8100", []int32{8100}, intstr.FromInt32(8100))
	delete(docker.Labels, ExecutorLabel)
	if _, err := Translate(docker); err == nil {
		t.Error("fixed-ports accepted for the docker executor")
	}
	// Without the label several ports are still rejected, now with a pointer to it.
	multi := fixedPod("", []int32{8100, 9100}, intstr.IntOrString{})
	if _, err := Translate(multi); err == nil || !strings.Contains(err.Error(), FixedPortsLabel) {
		t.Errorf("multi-port without label: %v", err)
	}
}

func TestFixedPortReadinessProbesTheDeclaredPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	good, _ := strconv.Atoi(portStr)
	// First fixed port has nothing listening; the probe names the second one.
	pod := fixedPod("", []int32{int32(good)}, intstr.FromInt32(int32(good)))
	pod.Labels[FixedPortsLabel] = "1_" + portStr
	inst := runtime.Instance{Phase: "Running", Port: 1, Ports: []int{1, good}, StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	state := &probeState{}
	var st corev1.PodStatus
	for i := 0; i < 2; i++ {
		state.readiness.next = time.Time{}
		st = makeStatus(context.Background(), pod, inst, state)
	}
	if st.Phase != corev1.PodRunning || st.ContainerStatuses[0].Ready != true {
		t.Fatalf("probe did not use the declared port: %+v", st.ContainerStatuses)
	}
	// Dynamic workloads keep probing the allocated port.
	dyn := runtime.Instance{Phase: "Running", Port: good, StartedAt: inst.StartedAt}
	pod2 := testPod()
	pod2.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8100}}
	pod2.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(8100)}}}
	st2 := makeStatus(context.Background(), pod2, dyn, &probeState{})
	if !st2.ContainerStatuses[0].Ready {
		t.Fatalf("dynamic probing regressed: %+v", st2.ContainerStatuses)
	}
}
