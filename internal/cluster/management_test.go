package cluster

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestDeleteAssignedOwnershipAndPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name, mode, assigned, rsUID, ownerKind, wantPath string
		wantError                                        bool
	}{
		{"pod", "pod", "mac", "rs-uid", "ReplicaSet", "/api/v1/namespaces/test/pods/pod", false},
		{"deployment", "workload", "mac", "rs-uid", "ReplicaSet", "/apis/apps/v1/namespaces/test/deployments/deploy", false},
		{"replacement-owner", "workload", "mac", "replaced", "ReplicaSet", "", true},
		{"wrong-node", "workload", "other", "rs-uid", "ReplicaSet", "", true},
		{"unknown-owner", "workload", "mac", "rs-uid", "Mystery", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deleted := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "DELETE" {
					deleted = r.URL.Path
					var opts metav1.DeleteOptions
					if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
						t.Error(err)
					}
					wantUID := "dep-uid"
					if tc.mode == "pod" {
						wantUID = "pod-uid"
					}
					if opts.Preconditions == nil || opts.Preconditions.UID == nil || string(*opts.Preconditions.UID) != wantUID || opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground {
						t.Errorf("unsafe delete options: %+v", opts)
					}
					w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
					return
				}
				switch r.URL.Path {
				case "/api/v1/pods":
					if r.URL.Query().Get("fieldSelector") != "spec.nodeName=mac" {
						t.Error("missing assigned node selector")
					}
					json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "PodList", "items": []any{map[string]any{"metadata": map[string]any{"name": "pod", "namespace": "test", "uid": "pod-uid", "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": tc.ownerKind, "name": "rs", "uid": "rs-uid", "controller": true}}}, "spec": map[string]any{"nodeName": tc.assigned}}}})
				case "/apis/apps/v1/namespaces/test/replicasets/rs":
					json.NewEncoder(w).Encode(map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": map[string]any{"name": "rs", "namespace": "test", "uid": tc.rsUID, "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "deploy", "uid": "dep-uid", "controller": true}}}})
				case "/apis/apps/v1/namespaces/test/deployments/deploy":
					w.Write([]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"deploy","namespace":"test","uid":"dep-uid"}}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			err := DeleteAssigned(context.Background(), &rest.Config{Host: srv.URL}, "mac", "pod-uid", tc.mode)
			if (err != nil) != tc.wantError || deleted != tc.wantPath {
				t.Fatalf("err=%v delete=%s", err, deleted)
			}
		})
	}
}

func TestLeaveNodeOwnership(t *testing.T) {
	for _, owner := range []string{"mine", "foreign", "missing"} {
		t.Run(owner, func(t *testing.T) {
			deleted := false
			cordoned := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "PUT" {
					var node map[string]any
					if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
						t.Error(err)
					}
					spec, _ := node["spec"].(map[string]any)
					metadata, _ := node["metadata"].(map[string]any)
					if spec["unschedulable"] != true || metadata["uid"] != "node-uid" {
						t.Error("unsafe cordon")
					}
					cordoned = true
					json.NewEncoder(w).Encode(node)
					return
				}
				if r.Method == "DELETE" {
					if !cordoned {
						t.Error("node deleted before cordon")
					}
					deleted = true
					var opts metav1.DeleteOptions
					json.NewDecoder(r.Body).Decode(&opts)
					if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "node-uid" {
						t.Error("missing Node UID precondition")
					}
					w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`))
					return
				}
				if owner == "missing" {
					w.WriteHeader(404)
					w.Write([]byte(`{"apiVersion":"v1","kind":"Status","reason":"NotFound","code":404}`))
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "mac", "uid": "node-uid", "annotations": map[string]string{"maccluster.example.com/installation-id": owner}}})
			}))
			defer srv.Close()
			err := LeaveNode(context.Background(), &rest.Config{Host: srv.URL}, "mac", "mine")
			if (err != nil) != (owner == "foreign") || deleted != (owner == "mine") {
				t.Fatalf("err=%v deleted=%v", err, deleted)
			}
		})
	}
}

func TestManagementConfigTrustAndAuth(t *testing.T) {
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/version" {
			t.Errorf("unexpected API path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"gitVersion":"v1.29.1"}`))
	}))
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	node := &rest.Config{Host: srv.URL, TLSClientConfig: rest.TLSClientConfig{CAData: ca}, Proxy: http.ProxyFromEnvironment}
	for _, kind := range []string{"valid", "endpoint", "ca", "insecure", "exec", "relative"} {
		t.Run(kind, func(t *testing.T) {
			cfg := clientapi.NewConfig()
			cfg.CurrentContext = "test"
			cfg.Contexts["test"] = &clientapi.Context{Cluster: "test", AuthInfo: "test"}
			cfg.Clusters["test"] = &clientapi.Cluster{Server: srv.URL, CertificateAuthorityData: ca}
			cfg.AuthInfos["test"] = &clientapi.AuthInfo{Token: "private-test-token"}
			switch kind {
			case "endpoint":
				cfg.Clusters["test"].Server = "https://other.invalid"
			case "ca":
				cfg.Clusters["test"].CertificateAuthorityData = []byte("invalid")
			case "insecure":
				cfg.Clusters["test"].InsecureSkipTLSVerify = true
			case "exec":
				cfg.AuthInfos["test"].Exec = &clientapi.ExecConfig{Command: "/bin/false", APIVersion: "client.authentication.k8s.io/v1beta1"}
			}
			path := filepath.Join(t.TempDir(), "config")
			if err := clientcmd.WriteToFile(*cfg, path); err != nil {
				t.Fatal(err)
			}
			os.Chmod(path, 0600)
			if kind == "relative" {
				path = "relative"
			}
			before := requests
			got, err := ManagementConfig(context.Background(), node, path)
			if kind == "valid" {
				if err != nil || got.Proxy == nil || requests != before+1 {
					t.Fatalf("valid config: %+v %v", got, err)
				}
			} else if err == nil || requests != before {
				t.Fatalf("invalid config accepted/contacted: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-test-token") {
				t.Fatal("credential leaked")
			}
		})
	}
}
