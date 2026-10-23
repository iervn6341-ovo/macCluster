package cluster

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	runtime "maccluster/internal/runtime"
)

func statusPod() *corev1.Pod {
	p := &corev1.Pod{}
	p.Spec.RestartPolicy = corev1.RestartPolicyAlways
	p.Spec.Containers = []corev1.Container{{Name: "c"}}
	return p
}

func podReady(s corev1.PodStatus) bool {
	for _, c := range s.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func TestAProgramThatJustStartedIsNotReadyYet(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name      string
		startedAt string
		want      bool
	}{
		{"started a moment ago", now.Format(time.RFC3339), false},
		{"started a moment ago (nanoseconds)", now.Format(time.RFC3339Nano), false},
		{"running for a while", now.Add(-10 * time.Second).Format(time.RFC3339), true},
		{"adopted program, long running", now.Add(-time.Hour).Format(time.RFC3339), true},
		{"start unknown: judged by probes alone", "", true},
	} {
		st := makeStatus(context.Background(), statusPod(), runtime.Instance{Phase: "Running", StartedAt: tc.startedAt})
		if got := podReady(st); got != tc.want {
			t.Errorf("%s: ready=%v want %v", tc.name, got, tc.want)
		}
		if tc.startedAt != "" && len(st.ContainerStatuses) == 1 && st.ContainerStatuses[0].Started != nil && !*st.ContainerStatuses[0].Started {
			t.Errorf("%s: a running program has started even when it is not ready", tc.name)
		}
	}
}

func TestACrashLoopingProgramIsNeverReady(t *testing.T) {
	// Every restart begins with a program that has just started; in between it waits in back-off.
	fresh := runtime.Instance{Phase: "Running", StartedAt: time.Now().UTC().Format(time.RFC3339), Restarts: 4}
	waiting := runtime.Instance{Phase: "Failed", ExitCode: 1, Restarts: 4}
	for _, i := range []runtime.Instance{fresh, waiting} {
		if podReady(makeStatus(context.Background(), statusPod(), i)) {
			t.Fatalf("a crash-looping program was Ready: %+v", i)
		}
	}
}
