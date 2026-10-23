package cluster

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestEgressEnvForOptedInPods(t *testing.T) {
	v, err := egressEnv("http://mac:p%40ss@192.0.2.5:31080", "192.0.2.9")
	if err != nil {
		t.Fatal(err)
	}
	if v["HTTP_PROXY"] != "http://mac:p%40ss@192.0.2.5:31080" || v["https_proxy"] != v["HTTP_PROXY"] {
		t.Fatalf("http proxy vars: %v", v)
	}
	if v["ALL_PROXY"] != "socks5h://mac:p%40ss@192.0.2.5:31080" {
		t.Fatalf("socks var: %v", v["ALL_PROXY"])
	}
	if !strings.Contains(v["NO_PROXY"], "127.0.0.1") || !strings.Contains(v["NO_PROXY"], "192.0.2.9") || !strings.Contains(v["no_proxy"], "host.docker.internal") {
		t.Fatalf("no_proxy: %v", v["NO_PROXY"])
	}
	for _, bad := range []string{"", "ftp://u:p@h:1", "http://h:1", "http://u:p@h", "::bad"} {
		if _, err := egressEnv(bad, ""); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func egressPod(annotated bool) *corev1.Pod {
	p := &corev1.Pod{}
	p.UID, p.Name, p.Namespace = "u1", "p", "ns"
	p.Labels = map[string]string{ExecutorLabel: "native", ManagedLabel: "true"}
	p.Spec.Containers = []corev1.Container{{Name: "c", Command: []string{"/bin/echo"}, Env: []corev1.EnvVar{{Name: "HTTPS_PROXY", Value: "http://mine:1"}}}}
	if annotated {
		p.Annotations = map[string]string{EgressAnnotation: "true"}
	}
	return p
}

func TestTranslateEgressIsOptInAndKeepsThePodsOwnVariables(t *testing.T) {
	const proxy = "http://mac:secret@192.0.2.5:31080"
	// Not annotated: nothing is added.
	s, _, err := TranslateWithOptions(egressPod(false), TranslateOptions{EgressProxy: proxy})
	if err != nil || s.EnvResolved["HTTP_PROXY"] != "" {
		t.Fatalf("a Pod that did not ask got proxy variables: %+v %v", s.EnvResolved, err)
	}
	// Annotated: added to the environment only the program sees, and the Pod's own value wins.
	s, _, err = TranslateWithOptions(egressPod(true), TranslateOptions{EgressProxy: proxy, HostIP: "192.0.2.9"})
	if err != nil {
		t.Fatal(err)
	}
	if s.EnvResolved["HTTP_PROXY"] == "" || s.EnvResolved["ALL_PROXY"] == "" {
		t.Fatalf("missing proxy variables: %v", s.EnvResolved)
	}
	if _, inLiteral := s.Env["HTTP_PROXY"]; inLiteral {
		t.Fatal("the credentials must not be in the persisted environment")
	}
	if s.EnvResolved["HTTPS_PROXY"] != "" || s.Env["HTTPS_PROXY"] != "http://mine:1" {
		t.Fatalf("the Pod's own HTTPS_PROXY was overridden: %v / %v", s.EnvResolved["HTTPS_PROXY"], s.Env["HTTPS_PROXY"])
	}
	// Annotated but the node has no proxy: a warning, no variables.
	s, notes, err := TranslateWithOptions(egressPod(true), TranslateOptions{})
	if err != nil || s.EnvResolved["HTTP_PROXY"] != "" {
		t.Fatalf("%v %v", s.EnvResolved, err)
	}
	found := false
	for _, n := range notes {
		found = found || n.Reason == NoteEgressNotConfigured
	}
	if !found {
		t.Fatalf("no warning: %+v", notes)
	}
}
