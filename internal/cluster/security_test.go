package cluster

import (
	"errors"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	runtime "maccluster/internal/runtime"
)

func ptr[T any](v T) *T { return &v }

func TestDockerLimitsAreEnforcedAndNativeLimitsWarn(t *testing.T) {
	p := testPod()
	p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("64Mi")}
	s, notes, err := TranslateWithNotes(p)
	if err != nil || s.Limits == nil || s.Limits.CPUMilli != 500 || s.Limits.MemoryBytes != 64<<20 || len(notes) != 0 {
		t.Fatalf("Docker enforces limits: %+v %+v %v", s.Limits, notes, err)
	}
	p.Labels[ExecutorLabel] = "native"
	s, notes, err = TranslateWithNotes(p)
	if err != nil || s.Limits != nil {
		t.Fatalf("a native program has no cgroup, so nothing is passed on: %+v %v", s.Limits, err)
	}
	if len(notes) != 1 || notes[0].Reason != NoteLimitsNotEnforced || notes[0].Type != corev1.EventTypeWarning {
		t.Fatalf("but the user must be told: %+v", notes)
	}
}

func TestDockerSecurityContextMapping(t *testing.T) {
	p := testPod()
	p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr(int64(1000)), RunAsGroup: ptr(int64(2000)), RunAsNonRoot: ptr(true), SupplementalGroups: []int64{3000}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		RunAsUser:                ptr(int64(1500)), // the container overrides the Pod
		ReadOnlyRootFilesystem:   ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"CAP_NET_BIND_SERVICE", "CHOWN"}},
	}
	s, _, err := TranslateWithNotes(p)
	if err != nil {
		t.Fatal(err)
	}
	want := runtime.SecuritySpec{User: "1500:2000", GroupAdd: []string{"3000"}, ReadOnlyRootFS: true, NoNewPrivileges: true, CapDrop: []string{"ALL"}, CapAdd: []string{"NET_BIND_SERVICE", "CHOWN"}}
	got := *s.Security
	if got.User != want.User || got.ReadOnlyRootFS != want.ReadOnlyRootFS || got.NoNewPrivileges != want.NoNewPrivileges ||
		strings.Join(got.GroupAdd, ",") != "3000" || strings.Join(got.CapDrop, ",") != "ALL" || strings.Join(got.CapAdd, ",") != "NET_BIND_SERVICE,CHOWN" {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSecurityContextNamesWhatItCannotDo(t *testing.T) {
	p := testPod()
	p.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: ptr(int64(1)), RunAsNonRoot: ptr(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost}}
	p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr(true), RunAsGroup: ptr(int64(5))}
	_, _, err := TranslateWithNotes(p)
	var se *SpecError
	if !errors.As(err, &se) || se.Reason != ReasonUnsupportedPodSpec {
		t.Fatalf("got %v", err)
	}
	for _, want := range []string{"fsGroup", "seccompProfile Localhost", "privileged", "runAsNonRoot: also set runAsUser", "runAsGroup: also set runAsUser"} {
		found := false
		for _, f := range se.Fields {
			found = found || strings.Contains(f, want)
		}
		if !found {
			t.Errorf("must name %q: %v", want, se.Fields)
		}
	}
}

func TestNativeAcceptsOnlyWhatIsAlreadyTrue(t *testing.T) {
	p := testPod()
	p.Labels[ExecutorLabel] = "native"
	p.Spec.Containers[0].Command = []string{"/bin/sleep", "1"}
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &uid, RunAsGroup: &gid, RunAsNonRoot: ptr(true), AllowPrivilegeEscalation: ptr(false)}
	s, _, err := TranslateWithNotes(p)
	if err != nil || s.Security != nil {
		t.Fatalf("the agent's own account and an unprivileged program satisfy these: %+v %v", s.Security, err)
	}
	other := uid + 1
	p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &other, ReadOnlyRootFilesystem: ptr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	_, _, err = TranslateWithNotes(p)
	if err == nil {
		t.Fatal("a native program cannot become another user, get a read-only root or drop capabilities")
	}
	for _, want := range []string{"runAsUser", "macOS account of the agent", "readOnlyRootFilesystem", "capabilities"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("must name %q: %v", want, err)
		}
	}
}

func TestRunAsNonRootNeedsAUserForDockerButIsAlreadyTrueForNative(t *testing.T) {
	p := testPod()
	p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr(true)}
	if _, _, err := TranslateWithNotes(p); err == nil || !strings.Contains(err.Error(), "runAsNonRoot: also set runAsUser") {
		t.Fatalf("an image's default user cannot be verified for Docker: %v", err)
	}
	p.Labels[ExecutorLabel] = "native"
	p.Spec.Containers[0].Command = []string{"/bin/sleep", "1"}
	_, _, err := TranslateWithNotes(p)
	if os.Getuid() == 0 {
		if err == nil {
			t.Fatal("a native program run by a root agent is root")
		}
	} else if err != nil {
		t.Fatalf("a native program of an unprivileged account is not root: %v", err)
	}
}
