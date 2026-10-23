package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fakeSource struct {
	secrets    map[string]map[string]string
	configMaps map[string]map[string]string
}

func (f fakeSource) Secret(_ context.Context, ns, name string) (*corev1.Secret, error) {
	d, ok := f.secrets[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	s := &corev1.Secret{Data: map[string][]byte{}}
	for k, v := range d {
		s.Data[k] = []byte(v)
	}
	return s, nil
}

func (f fakeSource) ConfigMap(_ context.Context, ns, name string) (*corev1.ConfigMap, error) {
	d, ok := f.configMaps[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return &corev1.ConfigMap{Data: d}, nil
}

func envPod() *corev1.Pod {
	p := testPod()
	p.Labels["app"] = "web"
	p.Annotations = map[string]string{"team": "infra"}
	p.Spec.NodeName = "node-1"
	p.Spec.ServiceAccountName = "robot"
	return p
}

func opts() TranslateOptions {
	return TranslateOptions{HostIP: "192.0.2.9", NodeName: "node-1", Source: fakeSource{
		secrets:    map[string]map[string]string{"creds": {"TOKEN": "s3cret", "KEY": "-----BEGIN-----\nline2\n-----END-----"}},
		configMaps: map[string]map[string]string{"settings": {"MODE": "fast", "bad key": "x", "9digit": "y"}},
	}}
}

func TestExpandRefsFollowsKubelet(t *testing.T) {
	vars := map[string]string{"A": "1", "B": "two"}
	lookup := func(n string) (string, bool) { v, ok := vars[n]; return v, ok }
	for in, want := range map[string]string{
		"plain":                     "plain",
		"$(A)-$(B)":                 "1-two",
		"$(MISSING)":                "$(MISSING)",
		"$$(A)":                     "$(A)",
		"price $5":                  "price $5",
		"unterminated $(A":          "unterminated $(A",
		"--port=$(MACCLUSTER_PORT)": "--port=$(MACCLUSTER_PORT)",
		"trailing $":                "trailing $",
	} {
		if got := expandRefs(in, lookup); got != want {
			t.Errorf("expand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildEnvSourcesOrderAndClassification(t *testing.T) {
	p := envPod()
	c := &p.Spec.Containers[0]
	c.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}
	c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}
	c.EnvFrom = []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}, Prefix: "CFG_"},
		{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "creds"}}},
	}
	c.Env = []corev1.EnvVar{
		{Name: "LIT", Value: "literal"},
		{Name: "TOKEN", Value: "overridden by env"},
		{Name: "POD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "NS", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "APP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['app']"}}},
		{Name: "TEAM", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations['team']"}}},
		{Name: "NODE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "SA", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.serviceAccountName"}}},
		{Name: "HOST", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
		{Name: "CPU_REQ", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "requests.cpu", Divisor: resource.MustParse("1m")}}},
		{Name: "CPU_CORES", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "requests.cpu"}}},
		{Name: "MEM_MI", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.memory", Divisor: resource.MustParse("1Mi")}}},
		{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "creds"}, Key: "TOKEN"}}},
		{Name: "FROM_CM", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}, Key: "MODE"}}},
		{Name: "URL", Value: "http://$(POD).$(NS):$(MACCLUSTER_PORT)/"},
		{Name: "PLAIN_REF", Value: "mode=$(LIT)"},
	}
	c.Command = []string{"/bin/run", "--name=$(POD)"}
	c.Args = []string{"--lit=$(LIT)", "--port=$(MACCLUSTER_PORT)"}
	o := opts()
	s, notes, err := TranslateWithOptions(p, o)
	if err != nil {
		t.Fatal(err)
	}
	wantResolved := map[string]string{"POD": "test", "NS": "test", "UID": "unique", "APP": "web", "TEAM": "infra", "NODE": "node-1", "SA": "robot", "HOST": "192.0.2.9",
		"CPU_REQ": "250", "CPU_CORES": "1", "MEM_MI": "64", "FROM_SECRET": "s3cret", "FROM_CM": "fast", "CFG_MODE": "fast", "KEY": "-----BEGIN-----\nline2\n-----END-----",
		"URL": "http://test.test:$(MACCLUSTER_PORT)/"}
	for k, v := range wantResolved {
		if s.EnvResolved[k] != v {
			t.Errorf("EnvResolved[%s] = %q, want %q", k, s.EnvResolved[k], v)
		}
	}
	if s.Env["TOKEN"] != "overridden by env" || s.Env["LIT"] != "literal" || s.Env["PLAIN_REF"] != "mode=literal" {
		t.Errorf("plain values (env wins over envFrom, $(LIT) expands) stay in Env: %v", s.Env)
	}
	if _, secret := s.EnvResolved["TOKEN"]; secret {
		t.Error("a name set explicitly in env replaces the envFrom value")
	}
	if got := s.Command; got[1] != "--name=test" {
		t.Errorf("command expands against the environment: %v", got)
	}
	if got := s.Args; got[0] != "--lit=literal" || got[1] != "--port=$(MACCLUSTER_PORT)" {
		t.Errorf("args: %v (the runtime's own $(MACCLUSTER_PORT) must survive)", got)
	}
	found := false
	for _, n := range notes {
		if n.Reason == NoteInvalidEnvNames && strings.Contains(n.Message, "CFG_bad key") && strings.Contains(n.Message, "CFG_9digit") == false {
			found = true
		}
	}
	if !found {
		t.Errorf("keys that are not environment variable names are skipped with a warning: %+v", notes)
	}
	raw, _ := json.Marshal(s)
	for _, leak := range []string{"s3cret", "BEGIN", "192.0.2.9", "infra"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("values from the cluster must not be part of the spec's JSON form (hash, persistence): found %q", leak)
		}
	}
}

func TestMissingSecretIsAConfigErrorThatRecovers(t *testing.T) {
	p := envPod()
	p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "later"}, Key: "K"}}}}
	o := TranslateOptions{Source: fakeSource{}}
	_, _, err := TranslateWithOptions(p, o)
	var se *SpecError
	if !errors.As(err, &se) || se.Reason != ReasonConfigError || !strings.Contains(err.Error(), `secret "later": not found`) {
		t.Fatalf("got %v", err)
	}
	o.Source = fakeSource{secrets: map[string]map[string]string{"later": {"OTHER": "v"}}}
	if _, _, err = TranslateWithOptions(p, o); err == nil || !strings.Contains(err.Error(), "couldn't find key K in Secret test/later") {
		t.Fatalf("got %v", err)
	}
	o.Source = fakeSource{secrets: map[string]map[string]string{"later": {"K": "v"}}}
	s, _, err := TranslateWithOptions(p, o)
	if err != nil || s.EnvResolved["X"] != "v" {
		t.Fatalf("once the Secret exists the Pod starts: %v %v", s.EnvResolved, err)
	}
	yes := true
	p.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Optional = &yes
	o.Source = fakeSource{}
	s, _, err = TranslateWithOptions(p, o)
	if err != nil || len(s.EnvResolved) != 0 {
		t.Fatalf("optional and absent means unset: %v %v", s.EnvResolved, err)
	}
}

func TestUnavailableSourcesAreNamedAsUnsupported(t *testing.T) {
	p := envPod()
	p.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "A", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "B", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.deletionTimestamp"}}},
		{Name: "C", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.ephemeral-storage"}}},
	}
	_, _, err := TranslateWithOptions(p, opts())
	var se *SpecError
	if !errors.As(err, &se) || se.Reason != ReasonUnsupportedPodSpec || len(se.Fields) != 3 {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(se.Fields[0], "no Pod IP") || !strings.Contains(se.Fields[1], "metadata.deletionTimestamp") || !strings.Contains(se.Fields[2], "limits.ephemeral-storage") {
		t.Fatalf("each message names its own problem: %v", se.Fields)
	}
	p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "D", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.memory"}}}}
	if _, _, err = TranslateWithOptions(p, opts()); err == nil || !strings.Contains(err.Error(), "no limit is set") {
		t.Fatalf("an unset limit is explained, not guessed: %v", err)
	}
}

func TestPodEnvSourceReadsOnceAndRetriesFailures(t *testing.T) {
	client := fake.NewSimpleClientset()
	gets := 0
	client.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, apiruntime.Object, error) {
		gets++
		if gets == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "s")
		}
		return true, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}, Data: map[string][]byte{"k": []byte("v")}}, nil
	})
	src := &podEnvSource{client: client, state: &probeState{}}
	if _, err := src.Secret(context.Background(), "ns", "s"); !apierrors.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
	for i := 0; i < 3; i++ {
		v, err := src.Secret(context.Background(), "ns", "s")
		if err != nil || string(v.Data["k"]) != "v" {
			t.Fatalf("%v", err)
		}
	}
	if gets != 2 {
		t.Fatalf("a failed read is retried, a successful one is remembered: %d API reads", gets)
	}
}
