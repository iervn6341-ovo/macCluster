package cluster

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Every Deployment shipped in examples/ must be accepted by Translate and be
// schedulable on the prototype node, so documented YAML cannot silently rot.
func TestExampleManifestsAreAccepted(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	// Templates bundled with the deploy-to-mac-node skill carry placeholders but
	// must satisfy the same rules.
	templates, err := filepath.Glob("../../.claude/skills/deploy-to-mac-node/assets/*.yaml")
	if err != nil || len(templates) != 3 {
		t.Fatal(templates, err)
	}
	files = append(files, templates...)
	want := map[string]struct{ executor, environment string }{
		"faq-docker.yaml":             {"docker", ""},
		"faq-docker-volumes.yaml":     {"docker", ""},
		"faq-native.yaml":             {"native", ""},
		"faq-native-environment.yaml": {"native", "my-env"},
		"faq-native-fixed-ports.yaml": {"native", "vllm-metal"},
		"docker.yaml":                 {"docker", ""},
		"native.yaml":                 {"native", ""},
		"native-environment.yaml":     {"native", "env-name"},
	}
	seen := map[string]bool{}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		deployments := 0
		for {
			var doc map[string]any
			if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if doc["kind"] != "Deployment" {
				continue
			}
			raw, _ := json.Marshal(doc)
			var d appsv1.Deployment
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			deployments++
			pod := &corev1.Pod{}
			pod.Name, pod.Namespace, pod.UID = d.Name, d.Namespace, "uid"
			pod.Labels = d.Spec.Template.Labels
			pod.Spec = d.Spec.Template.Spec
			spec, _, err := TranslateWithOptions(pod, TranslateOptions{Volumes: volumeSettings{root: t.TempDir()}})
			if err != nil {
				t.Errorf("%s (%s): Translate rejected it: %v", path, d.Name, err)
				continue
			}
			// The test node carries this NoSchedule taint; without the toleration
			// and nodeSelector a documented manifest would never get scheduled.
			tolerated := false
			for _, tol := range pod.Spec.Tolerations {
				if tol.Key == "maccluster.example.com/prototype" && tol.Effect == corev1.TaintEffectNoSchedule {
					tolerated = true
				}
			}
			if !tolerated || pod.Spec.NodeSelector["maccluster.example.com/host"] == "" {
				t.Errorf("%s (%s): missing prototype toleration or host nodeSelector", path, d.Name)
			}
			if sel := d.Spec.Selector.MatchLabels; len(sel) == 0 {
				t.Errorf("%s (%s): empty selector", path, d.Name)
			} else {
				for k, v := range sel {
					if d.Spec.Template.Labels[k] != v {
						t.Errorf("%s (%s): selector %s=%s does not match template labels", path, d.Name, k, v)
					}
				}
			}
			if w, ok := want[filepath.Base(path)]; ok {
				seen[filepath.Base(path)] = true
				if spec.Executor != w.executor || spec.Environment != w.environment {
					t.Errorf("%s: executor=%q environment=%q, want %+v", path, spec.Executor, spec.Environment, w)
				}
				if v := pod.Spec.NodeSelector["env.maccluster.example.com/"+w.environment]; w.environment != "" && v != "true" {
					t.Errorf("%s: environment example must also select nodes advertising it", path)
				}
			}
		}
		f.Close()
		if deployments == 0 {
			t.Errorf("%s: no Deployment found", path)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("expected example %s was not checked", name)
		}
	}
}
