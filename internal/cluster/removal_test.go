package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestNodeRemovalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		body            string
		removed, failed bool
	}{
		{"same identity", 200, `{"metadata":{"uid":"original"}}`, false, false},
		{"deleted", 404, `{"kind":"Status","reason":"NotFound","code":404}`, true, true},
		{"replaced", 200, `{"metadata":{"uid":"replacement"}}`, true, true},
		{"forbidden", 403, `{"kind":"Status","reason":"Forbidden","code":403}`, false, true},
		{"server error", 500, `{"kind":"Status","reason":"InternalError","code":500}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client, _ := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
			err := checkNode(context.Background(), client, "test", "original")
			if errors.Is(err, ErrNodeRemoved) != tc.removed || (err != nil) != tc.failed {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	client, _ := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err := checkNode(context.Background(), client, "test", "original"); err == nil || errors.Is(err, ErrNodeRemoved) {
		t.Fatalf("transport failure treated as removal: %v", err)
	}
}

func TestSavedNodeNeverRecreated(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Errorf("unexpected mutation %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		w.Write([]byte(`{"kind":"Status","reason":"NotFound","code":404}`))
	}))
	defer srv.Close()
	err := Run(context.Background(), &rest.Config{Host: srv.URL}, "test", "owner", nil, func(string, string) {}, NodeIdentity{UID: "original"})
	if !errors.Is(err, ErrNodeRemoved) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
