package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"maccluster/internal/envs"
	runtime "maccluster/internal/runtime"
	"sync"
)

type managementEngine struct {
	instances []runtime.Instance
	ensured   []runtime.Spec
	stopped   []string
	stopErr   error
	closed    bool
}

func (e *managementEngine) Ensure(_ context.Context, s runtime.Spec) (runtime.Instance, error) {
	e.ensured = append(e.ensured, s)
	i := runtime.Instance{UID: s.UID, Name: s.Name, Namespace: s.Namespace, Executor: s.Executor, Phase: "Running"}
	e.instances = append(e.instances, i)
	return i, nil
}
func (e *managementEngine) Stop(_ context.Context, uid string) error {
	e.stopped = append(e.stopped, uid)
	if e.stopErr != nil {
		return e.stopErr
	}
	for i, v := range e.instances {
		if v.UID == uid {
			e.instances = append(e.instances[:i], e.instances[i+1:]...)
			break
		}
	}
	return nil
}
func (e *managementEngine) List(context.Context) []runtime.Instance {
	return append([]runtime.Instance{}, e.instances...)
}
func (e *managementEngine) Logs(context.Context, string) (string, error) { return "", nil }
func (e *managementEngine) Close(context.Context) error                  { e.closed = true; return nil }
func managementServer(t *testing.T) (*server, *managementEngine) {
	t.Helper()
	e := &managementEngine{}
	return &server{ctx: context.Background(), dir: t.TempDir(), sessions: map[string]*clusterSession{}, local: e, definitions: map[string]runtime.Spec{}}, e
}

func TestJoinInvalidNodeNameRejectedBeforeAcceptingOrSavingProfile(t *testing.T) {
	for _, name := range []string{"my_macbook_pro", "maccluster-UPPER", "maccluster-"} {
		t.Run(name, func(t *testing.T) {
			s, _ := managementServer(t)
			body, err := json.Marshal(map[string]string{
				"nodeName": name,
				"command":  "kubeadm join 192.0.2.10:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:" + strings.Repeat("a", 64),
			})
			if err != nil {
				t.Fatal(err)
			}
			response := managementRequest(s.join, string(body))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "DNS-compatible") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if len(s.sessions) != 0 {
				t.Fatal("invalid join created a session")
			}
			for _, name := range []string{"profiles.json", "clusters"} {
				if _, err := os.Stat(filepath.Join(s.dir, name)); !os.IsNotExist(err) {
					t.Fatalf("invalid join created %s", name)
				}
			}
		})
	}
}
func managementRequest(handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return w
}
func persistedDefinitions(t *testing.T, s *server) map[string]runtime.Spec {
	t.Helper()
	path := filepath.Join(s.dir, "local-services.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("definitions permissions = %o", info.Mode().Perm())
	}
	var defs map[string]runtime.Spec
	if err = json.Unmarshal(b, &defs); err != nil {
		t.Fatal(err)
	}
	return defs
}
func TestManagementStatusGroupsClustersAndLocal(t *testing.T) {
	s, e := managementServer(t)
	e.instances = []runtime.Instance{{UID: "local", Name: "local"}}
	s.sessions["b"] = &clusterSession{profile: profile{ID: "b", Name: "B", NodeName: "mac-b", ManagementKubeconfig: "/private/admin"}, phase: "Error", message: "disconnected", engine: &managementEngine{instances: []runtime.Instance{{UID: "pod-b", Name: "B"}}}}
	s.sessions["a"] = &clusterSession{profile: profile{ID: "a", Name: "A", NodeName: "mac-a"}, phase: "Ready", engine: &managementEngine{instances: []runtime.Instance{{UID: "pod-a", Name: "A"}}}}
	w := managementRequest(s.getStatus, "")
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	var got status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Clusters) != 2 || got.Clusters[0].ID != "a" || got.Clusters[1].ID != "b" {
		t.Fatalf("clusters: %+v", got.Clusters)
	}
	for i, uid := range []string{"pod-a", "pod-b"} {
		if len(got.Clusters[i].Workloads) != 1 || got.Clusters[i].Workloads[0].UID != uid {
			t.Fatalf("cluster workloads: %+v", got.Clusters)
		}
	}
	if got.Clusters[1].Phase != "Error" || got.Clusters[1].Message != "disconnected" || !got.Clusters[1].ManagementConfigured {
		t.Fatalf("disconnected metadata lost: %+v", got.Clusters[1])
	}
	if len(got.LocalWorkloads) != 1 || got.LocalWorkloads[0].UID != "local" {
		t.Fatalf("locals: %+v", got.LocalWorkloads)
	}
	if len(got.Workloads) != 2 {
		t.Fatalf("legacy workloads should only include assigned pods: %+v", got.Workloads)
	}
}
func TestManagementLocalCreateDefaultsPersistAndDeleteSelected(t *testing.T) {
	s, e := managementServer(t)
	ids := []string{}
	for _, name := range []string{"one", "two"} {
		w := managementRequest(s.create, `{"name":"`+name+`","image":"test-image"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		var instance runtime.Instance
		if err := json.Unmarshal(w.Body.Bytes(), &instance); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(instance.UID, "local-") || instance.Namespace != "local" {
			t.Fatalf("assigned identity: %+v", instance)
		}
		ids = append(ids, instance.UID)
	}
	if ids[0] == ids[1] {
		t.Fatal("duplicate generated UID")
	}
	defs := persistedDefinitions(t, s)
	if len(defs) != 2 || len(e.ensured) != 2 {
		t.Fatalf("definitions=%d ensure calls=%d", len(defs), len(e.ensured))
	}
	for _, id := range ids {
		if defs[id].Executor != "docker" || defs[id].RestartPolicy != "Never" {
			t.Fatalf("defaults: %+v", defs[id])
		}
	}
	clusterEngine := &managementEngine{instances: []runtime.Instance{{UID: "cluster-pod"}}}
	s.sessions["cluster"] = &clusterSession{profile: profile{ID: "cluster"}, engine: clusterEngine}
	w := managementRequest(s.delete, `{"uid":"`+ids[0]+`","clusterID":"","mode":"pod"}`)
	if w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	defs = persistedDefinitions(t, s)
	if len(defs) != 1 || defs[ids[1]].UID != ids[1] || len(s.definitions) != 1 {
		t.Fatalf("remaining definitions: %+v", defs)
	}
	if len(e.stopped) != 1 || e.stopped[0] != ids[0] || len(e.instances) != 1 || e.instances[0].UID != ids[1] {
		t.Fatalf("wrong local resource removed: %+v", e)
	}
	if len(clusterEngine.stopped) != 0 || clusterEngine.closed || len(clusterEngine.instances) != 1 {
		t.Fatal("local deletion affected cluster engine")
	}
}
func TestManagementLocalDeleteStopFailureRestoresDefinition(t *testing.T) {
	s, e := managementServer(t)
	spec := runtime.Spec{UID: "retained", Name: "keep", Executor: "docker", RestartPolicy: "Never"}
	s.definitions[spec.UID] = spec
	e.instances = []runtime.Instance{{UID: spec.UID}}
	e.stopErr = errors.New("runtime unavailable")
	w := managementRequest(s.delete, `{"uid":"retained","clusterID":"","mode":"pod"}`)
	if w.Code != 500 || !strings.Contains(w.Body.String(), "runtime unavailable") {
		t.Fatalf("delete failure: %d %s", w.Code, w.Body.String())
	}
	defs := persistedDefinitions(t, s)
	if defs[spec.UID].Name != "keep" || s.definitions[spec.UID].Name != "keep" || len(e.instances) != 1 {
		t.Fatal("failed stop did not retain desired state and resource")
	}
}
func TestManagementUnknownClusterCannotDeleteLocalOrOtherCluster(t *testing.T) {
	s, e := managementServer(t)
	s.definitions["same-uid"] = runtime.Spec{UID: "same-uid", Name: "local"}
	e.instances = []runtime.Instance{{UID: "same-uid"}}
	other := &managementEngine{instances: []runtime.Instance{{UID: "same-uid"}}}
	s.sessions["other"] = &clusterSession{profile: profile{ID: "other"}, engine: other}
	for _, mode := range []string{"pod", "workload"} {
		w := managementRequest(s.delete, `{"uid":"same-uid","clusterID":"missing","mode":"`+mode+`"}`)
		if w.Code != 404 {
			t.Fatalf("unknown cluster: %d %s", w.Code, w.Body.String())
		}
	}
	if len(s.definitions) != 1 || len(s.sessions) != 1 || len(e.stopped) != 0 || len(other.stopped) != 0 || e.closed || other.closed {
		t.Fatal("unknown cluster operation modified another scope")
	}
	if len(e.instances) != 1 || len(other.instances) != 1 {
		t.Fatal("unknown cluster removed runtime resource")
	}
}

func TestManagementLeaveScopesCleanupAndRetainsFailedProfile(t *testing.T) {
	for _, remoteFailure := range []bool{false, true} {
		name := "success"
		if remoteFailure {
			name = "remote-failure"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/api/v1/nodes/mac-target" {
					t.Errorf("unexpected target: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				node := `{"apiVersion":"v1","kind":"Node","metadata":{"name":"mac-target","uid":"owned-node","resourceVersion":"1","annotations":{"maccluster.example.com/installation-id":"install-test"}},"spec":{"unschedulable":true}}`
				switch r.Method {
				case http.MethodGet:
					// Begin schedulable so successful leave must cordon before deleting.
					w.Write([]byte(strings.Replace(node, `"unschedulable":true`, `"unschedulable":false`, 1)))
				case http.MethodPut:
					var body struct {
						Spec struct {
							Unschedulable bool `json:"unschedulable"`
						} `json:"spec"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Spec.Unschedulable {
						t.Errorf("cordon update invalid: %v %+v", err, body)
					}
					w.Write([]byte(node))
				case http.MethodDelete:
					var options struct {
						Preconditions struct {
							UID string `json:"uid"`
						} `json:"preconditions"`
					}
					if err := json.NewDecoder(r.Body).Decode(&options); err != nil || options.Preconditions.UID != "owned-node" {
						t.Errorf("delete ownership precondition invalid: %v %+v", err, options)
					}
					if remoteFailure {
						w.WriteHeader(http.StatusForbidden)
						w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","message":"node deletion forbidden","code":403}`))
						return
					}
					w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`))
				default:
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(405)
				}
			}))
			defer api.Close()
			s, local := managementServer(t)
			s.installationID = "install-test"
			local.instances = []runtime.Instance{{UID: "local-stays"}}
			s.definitions["local-stays"] = runtime.Spec{UID: "local-stays", Name: "local"}
			target := &managementEngine{instances: []runtime.Instance{{UID: "target-pod"}}}
			other := &managementEngine{instances: []runtime.Instance{{UID: "other-pod"}}}
			s.sessions["target"] = &clusterSession{profile: profile{ID: "target", NodeName: "mac-target", Endpoint: api.URL}, phase: "Ready", cfg: &rest.Config{Host: api.URL}, engine: target}
			s.sessions["other"] = &clusterSession{profile: profile{ID: "other", NodeName: "mac-other"}, phase: "Ready", engine: other}
			dir := s.profileDir("target")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"node.kubeconfig", "node-name"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("saved-test-data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			w := managementRequest(s.leave, `{"clusterID":"target"}`)
			expected := 200
			if remoteFailure {
				expected = 409
			}
			if w.Code != expected {
				t.Fatalf("leave: %d %s", w.Code, w.Body.String())
			}
			if !target.closed || other.closed || local.closed || len(other.instances) != 1 || len(local.instances) != 1 || len(s.definitions) != 1 || s.sessions["other"] == nil {
				t.Fatal("leave did not isolate targeted cluster")
			}
			var profiles []profile
			if err := readJSON(filepath.Join(s.dir, "profiles.json"), &profiles); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			observed := append([]string{}, calls...)
			mu.Unlock()
			if strings.Join(observed, ",") != "GET /api/v1/nodes/mac-target,PUT /api/v1/nodes/mac-target,DELETE /api/v1/nodes/mac-target" {
				t.Fatalf("remote cleanup sequence: %v", observed)
			}
			if remoteFailure {
				c := s.sessions["target"]
				if c == nil || !c.profile.Leaving || c.phase != "Error" || !strings.Contains(c.message, "forbidden") {
					t.Fatalf("failed profile not retained for retry: %+v", c)
				}
				if len(profiles) != 2 {
					t.Fatalf("persisted profiles: %+v", profiles)
				}
				found := false
				for _, p := range profiles {
					if p.ID == "target" && p.Leaving {
						found = true
					}
				}
				if !found {
					t.Fatal("retry marker not persisted")
				}
				if _, err := os.Stat(filepath.Join(dir, "node.kubeconfig")); err != nil {
					t.Fatal("retry credentials removed")
				}
			} else {
				if s.sessions["target"] != nil || len(profiles) != 1 || profiles[0].ID != "other" {
					t.Fatalf("target not removed: %+v", profiles)
				}
				for _, file := range []string{"node.kubeconfig", "node-name"} {
					if _, err := os.Stat(filepath.Join(dir, file)); !os.IsNotExist(err) {
						t.Fatalf("target credential still present: %s %v", file, err)
					}
				}
			}
		})
	}
}

func TestRemovedClusterPersistsAndStopsOnlyItsWorkloads(t *testing.T) {
	s, local := managementServer(t)
	target := &managementEngine{instances: []runtime.Instance{{UID: "target-pod"}}}
	other := &managementEngine{instances: []runtime.Instance{{UID: "other-pod"}}}
	c := &clusterSession{profile: profile{ID: "legacy", NodeUID: "original"}, engine: target}
	s.sessions["legacy"] = c
	s.sessions["other"] = &clusterSession{profile: profile{ID: "other"}, engine: other}
	s.markRemoved(c, "node deleted")
	if c.phase != "Removed" || len(target.instances) != 0 || len(other.instances) != 1 || len(local.stopped) != 0 {
		t.Fatal("wrong cleanup scope")
	}
	var profiles []profile
	if err := readJSON(filepath.Join(s.dir, "profiles.json"), &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatal("lost other profile")
	}
	for _, p := range profiles {
		if p.ID == "legacy" && (p.RemovedAt == "" || p.NodeUID != "original" || p.RemovalReason != "node deleted") {
			t.Fatal("missing tombstone")
		}
	}
}

func TestRemovedClusterReportsCleanupFailure(t *testing.T) {
	s, _ := managementServer(t)
	c := &clusterSession{profile: profile{ID: "legacy"}, engine: &managementEngine{instances: []runtime.Instance{{UID: "pod"}}, stopErr: errors.New("stop failed")}}
	s.sessions["legacy"] = c
	s.markRemoved(c, "deleted")
	if c.phase != "Removed" || !strings.Contains(c.message, "清理尚未完成") {
		t.Fatal(c.message)
	}
}

func TestRemovedProfileDoesNotRegisterAfterRestart(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("removed profile contacted API: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	defer api.Close()
	dir := t.TempDir()
	cfg := "apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: " + api.URL + "\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "node.kubeconfig"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	if err := privateJSON(filepath.Join(dir, "profiles.json"), []profile{{ID: "legacy", NodeName: "test", NodeUID: "old", RemovedAt: "2026-10-15T00:00:00Z", RemovalReason: "deleted"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := newServer(ctx, dir, "installation")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	defer s.close(context.Background())
	c := s.sessions["legacy"]
	if c.phase != "Removed" || c.done != nil {
		t.Fatal("removed cluster relaunched")
	}
}

func TestManagementCreateWithEnvironment(t *testing.T) {
	s, e := managementServer(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0755)
	if err := envs.Add(filepath.Join(s.dir, "environments"), envs.Env{Name: "tool", Type: envs.TypeLink, Root: root}); err != nil {
		t.Fatal(err)
	}
	body := func(env, cmd, exec string) string {
		return `{"name":"svc-` + env + cmd + `","executor":"` + exec + `","image":"x","command":["` + cmd + `"],"environment":"` + env + `"}`
	}
	if w := managementRequest(s.create, body("tool", "serve", "native")); w.Code != 201 {
		t.Fatalf("bare command with environment: %d %s", w.Code, w.Body.String())
	}
	if len(e.ensured) != 1 || e.ensured[0].Environment != "tool" {
		t.Fatalf("%+v", e.ensured)
	}
	for name, b := range map[string]string{
		"missing environment":  body("nope", "serve", "native"),
		"path in bare command": body("tool", "sub/serve", "native"),
		"docker environment":   body("tool", "serve", "docker"),
		"no environment":       `{"name":"plain","executor":"native","command":["serve"]}`,
	} {
		if w := managementRequest(s.create, b); w.Code != 400 {
			t.Errorf("%s accepted: %d %s", name, w.Code, w.Body.String())
		}
	}
}
