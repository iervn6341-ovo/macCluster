package servicenet

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const digest = "registry.example.com/maccluster-network-controller@sha256:0123456789012345678901234567890123456789012345678901234567890123"

func goodOptions() ManifestOptions {
	return ManifestOptions{Namespace: "maccluster-system", ManagedNamespaces: []string{"team-b", "team-a"}, Image: digest, Replicas: 2}
}

func TestManifestsAreRejectedWhenUnsafe(t *testing.T) {
	for name, mutate := range map[string]func(*ManifestOptions){
		"image without a digest":    func(o *ManifestOptions) { o.Image = "registry.example.com/x:latest" },
		"no image":                  func(o *ManifestOptions) { o.Image = "" },
		"no managed namespace":      func(o *ManifestOptions) { o.ManagedNamespaces = nil },
		"managed kube-system":       func(o *ManifestOptions) { o.ManagedNamespaces = []string{"kube-system"} },
		"managed kube-node-lease":   func(o *ManifestOptions) { o.ManagedNamespaces = []string{"kube-node-lease"} },
		"runs in kube-system":       func(o *ManifestOptions) { o.Namespace = "kube-system" },
		"duplicate namespace":       func(o *ManifestOptions) { o.ManagedNamespaces = []string{"a", "a"} },
		"bad namespace name":        func(o *ManifestOptions) { o.ManagedNamespaces = []string{"Not_Valid"} },
		"zero replicas":             func(o *ManifestOptions) { o.Replicas = 0 },
		"too many replicas":         func(o *ManifestOptions) { o.Replicas = 9 },
		"namespace with whitespace": func(o *ManifestOptions) { o.Namespace = "a b" },
	} {
		o := goodOptions()
		mutate(&o)
		if _, err := Manifests(o); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	o := goodOptions()
	o.Image, o.AllowTag = "registry.example.com/x:1.2", true
	if _, err := Manifests(o); err != nil {
		t.Fatalf("an explicitly allowed tag: %v", err)
	}
}

func decodeAll(t *testing.T, doc string) []map[string]any {
	t.Helper()
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(doc), 4096)
	var out []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}

func TestRightsAreAsNarrowAsTheWorkAllows(t *testing.T) {
	doc, err := ManifestsYAML(goodOptions())
	if err != nil {
		t.Fatal(err)
	}
	objs := decodeAll(t, doc)
	kinds := map[string]int{}
	var bindings []map[string]any
	for _, o := range objs {
		kinds[o["kind"].(string)]++
		if o["kind"] == "RoleBinding" {
			bindings = append(bindings, o)
		}
	}
	want := map[string]int{"Namespace": 1, "ServiceAccount": 1, "ClusterRole": 2, "ClusterRoleBinding": 1, "Role": 2, "RoleBinding": 4, "Deployment": 1, "PodDisruptionBudget": 1}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%s: %d, want %d (%v)", k, kinds[k], n, kinds)
		}
	}
	// A RoleBinding to the namespaced ClusterRole exists in each managed namespace and in no other.
	got := map[string]bool{}
	for _, b := range bindings {
		meta := b["metadata"].(map[string]any)
		ref := b["roleRef"].(map[string]any)
		if ref["name"] == namespacedRole {
			got[meta["namespace"].(string)] = true
		}
	}
	if len(got) != 2 || !got["team-a"] || !got["team-b"] {
		t.Fatalf("namespaced rights are bound in %v, want exactly team-a and team-b", got)
	}
	// Decode the rules and check them one by one.
	for _, o := range objs {
		if o["kind"] != "ClusterRole" && o["kind"] != "Role" {
			continue
		}
		var role rbacv1.ClusterRole
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o, &role); err != nil {
			t.Fatal(err)
		}
		for _, r := range role.Rules {
			for _, v := range append(append([]string{}, r.Verbs...), append(r.Resources, r.APIGroups...)...) {
				if v == "*" {
					t.Errorf("%s has a wildcard: %+v", role.Name, r)
				}
			}
			for _, res := range r.Resources {
				if res == "secrets" || res == "configmaps" {
					t.Errorf("%s can read %s", role.Name, res)
				}
			}
		}
		switch role.Name {
		case namespacedRole:
			if len(role.Rules) != 2 || strings.Join(role.Rules[1].Verbs, ",") != "get,list,watch,create,update,delete" || role.Rules[1].Resources[0] != "endpointslices" {
				t.Errorf("namespaced rules: %+v", role.Rules)
			}
			if strings.Join(role.Rules[0].Verbs, ",") != "get,list,watch" {
				t.Errorf("services and pods are read-only: %+v", role.Rules[0])
			}
		case nodesRole:
			if len(role.Rules) != 1 || role.Rules[0].Resources[0] != "nodes" || strings.Join(role.Rules[0].Verbs, ",") != "get,list,watch" {
				t.Errorf("nodes: %+v", role.Rules)
			}
		case electionRole:
			if strings.Join(role.Rules[0].Verbs, ",") != "get,create,update" {
				t.Errorf("election: %+v", role.Rules)
			}
		case nodeLeaseRole:
			if strings.Join(role.Rules[0].Verbs, ",") != "get,list,watch" {
				t.Errorf("node leases are read-only: %+v", role.Rules)
			}
		}
	}
}

func TestDeploymentIsHardenedAndHealthChecked(t *testing.T) {
	doc, _ := ManifestsYAML(goodOptions())
	var dep appsv1.Deployment
	for _, o := range decodeAll(t, doc) {
		if o["kind"] == "Deployment" {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o, &dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if *dep.Spec.Replicas != 2 || c.Image != digest {
		t.Fatalf("replicas %d image %s", *dep.Spec.Replicas, c.Image)
	}
	if strings.Join(c.Args, " ") != "--namespaces team-a,team-b --health-addr :8081" {
		t.Fatalf("args (namespaces sorted, so the output is stable): %v", c.Args)
	}
	sc := c.SecurityContext
	if !*sc.RunAsNonRoot || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("security context: %+v", sc)
	}
	if c.LivenessProbe == nil || c.ReadinessProbe == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatal("probes")
	}
	envs := map[string]string{}
	for _, e := range c.Env {
		envs[e.Name] = e.ValueFrom.FieldRef.FieldPath
	}
	if envs["POD_NAME"] != "metadata.name" || envs["POD_NAMESPACE"] != "metadata.namespace" {
		t.Fatalf("identity from the downward API: %v", envs)
	}
	ro := dep.Spec.Strategy.RollingUpdate
	if ro.MaxUnavailable.IntValue() != 0 || ro.MaxSurge.IntValue() != 1 {
		t.Fatalf("rolling update must hand the leadership over without a gap: %+v", ro)
	}
	if !*dep.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("the controller needs its token")
	}
}

func TestSingleReplicaHasNoDisruptionBudget(t *testing.T) {
	o := goodOptions()
	o.Replicas = 1
	doc, _ := ManifestsYAML(o)
	if strings.Contains(doc, "PodDisruptionBudget") {
		t.Fatal("a budget that requires one available replica would block every drain of a single replica")
	}
	if strings.Contains(doc, "creationTimestamp") || strings.Contains(doc, "status: {}") {
		t.Fatal("generated noise left in the YAML")
	}
	again, _ := ManifestsYAML(o)
	if again != doc {
		t.Fatal("the output must be deterministic")
	}
}
