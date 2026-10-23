package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	authz "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"maccluster/internal/runtime"
)

type kubeletTestEngine struct {
	runtime.Engine
	instances []runtime.Instance
	logCalls  int
	text      string
}

func (e *kubeletTestEngine) List(context.Context) []runtime.Instance { return e.instances }
func (e *kubeletTestEngine) Logs(context.Context, string) (string, error) {
	e.logCalls++
	return e.text, nil
}

func kubeletFixture(t *testing.T) (*kubeletAPI, *fake.Clientset, *kubeletTestEngine, *core.Pod) {
	t.Helper()
	p := testPod()
	p.Spec.NodeName = "node-test"
	p.Spec.Containers[0].Ports = []core.ContainerPort{{ContainerPort: 8080, Protocol: core.ProtocolTCP}}
	client := fake.NewSimpleClientset(p)
	client.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, &authz.SubjectAccessReview{Status: authz.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	engine := &kubeletTestEngine{instances: []runtime.Instance{{UID: string(p.UID), Namespace: p.Namespace, Name: p.Name, Phase: "Running", Port: 31001}}, text: "first\nsecond\nthird\n"}
	return &kubeletAPI{client: client, nodeName: p.Spec.NodeName, engine: engine, streams: make(chan struct{}, 8)}, client, engine, p
}

func kubeletRequest(path, method string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{Subject: pkix.Name{CommonName: "api-client", Organization: []string{"kubelet-users"}}}}}}
	return r
}

func TestKubeletAuthorizationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		review    authz.SubjectAccessReviewStatus
		err       error
		anonymous bool
	}{
		{name: "anonymous", anonymous: true},
		{name: "denied", review: authz.SubjectAccessReviewStatus{Denied: true}},
		{name: "not allowed"},
		{name: "conflicting decision", review: authz.SubjectAccessReviewStatus{Allowed: true, Denied: true}},
		{name: "evaluation failed", review: authz.SubjectAccessReviewStatus{Allowed: true, EvaluationError: "evaluation failed"}},
		{name: "API unavailable", err: fmt.Errorf("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, engine, _ := kubeletFixture(t)
			client.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, &authz.SubjectAccessReview{Status: tc.review}, tc.err
			})
			r := kubeletRequest("/containerLogs/test/test/server", http.MethodGet)
			if tc.anonymous {
				r.TLS = nil
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := http.StatusForbidden
			if tc.anonymous {
				want = http.StatusUnauthorized
			}
			if w.Code != want || engine.logCalls != 0 {
				t.Fatalf("got HTTP %d, log calls %d", w.Code, engine.logCalls)
			}
			for _, a := range client.Actions() {
				if a.GetResource().Resource == "pods" {
					t.Fatal("queried Pod before authorization")
				}
			}
		})
	}
}

func TestKubeletSARUsesCertificateIdentityAndNodeProxyVerb(t *testing.T) {
	for _, tc := range []struct{ method, path, verb string }{{"GET", "/containerLogs/test/test/server", "get"}, {"POST", "/portForward/test/test", "create"}, {"GET", "/portForward/test/test", "get"}} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			h, client, _, _ := kubeletFixture(t)
			var spec authz.SubjectAccessReviewSpec
			client.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				spec = a.(k8stesting.CreateAction).GetObject().(*authz.SubjectAccessReview).Spec
				return true, &authz.SubjectAccessReview{Status: authz.SubjectAccessReviewStatus{Allowed: true}}, nil
			})
			r := kubeletRequest(tc.path, tc.method)
			r.Header.Set("Impersonate-User", "attacker")
			r.Header.Set("X-Remote-User", "attacker")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := &authz.ResourceAttributes{Verb: tc.verb, Group: "", Version: "v1", Resource: "nodes", Subresource: "proxy", Name: "node-test"}
			if spec.User != "api-client" || !reflect.DeepEqual(spec.Groups, []string{"kubelet-users", "system:authenticated"}) || !reflect.DeepEqual(spec.ResourceAttributes, want) {
				t.Fatalf("incorrect SAR: %+v", spec)
			}
		})
	}
}

func TestKubeletRejectsWrongPodContainerAndEngineUID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*core.Pod, *kubeletTestEngine)
		path   string
	}{
		{name: "wrong container", path: "/containerLogs/test/test/other"},
		{name: "wrong node", change: func(p *core.Pod, e *kubeletTestEngine) { p.Spec.NodeName = "other" }},
		{name: "deleted Pod", change: func(p *core.Pod, e *kubeletTestEngine) { p.DeletionTimestamp = &meta.Time{Time: time.Now()} }},
		{name: "different UID", change: func(p *core.Pod, e *kubeletTestEngine) { p.UID = "replacement" }},
		{name: "unknown engine UID", change: func(p *core.Pod, e *kubeletTestEngine) { e.instances = nil }},
		{name: "wrong engine namespace", change: func(p *core.Pod, e *kubeletTestEngine) { e.instances[0].Namespace = "other" }},
		{name: "multiple containers", change: func(p *core.Pod, e *kubeletTestEngine) {
			p.Spec.Containers = append(p.Spec.Containers, core.Container{Name: "other"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, engine, p := kubeletFixture(t)
			if tc.change != nil {
				tc.change(p, engine)
				if _, err := client.CoreV1().Pods(p.Namespace).Update(context.Background(), p, meta.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			path := tc.path
			if path == "" {
				path = "/containerLogs/test/test/server"
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, kubeletRequest(path, http.MethodGet))
			if w.Code != http.StatusNotFound || engine.logCalls != 0 {
				t.Fatalf("got HTTP %d, log calls %d", w.Code, engine.logCalls)
			}
		})
	}
}

func TestKubeletSnapshotOptionsAndExplicitUnsupportedFeatures(t *testing.T) {
	for _, tc := range []struct {
		query, want string
		code        int
	}{
		{"", "first\nsecond\nthird\n", 200}, {"tailLines=2", "second\nthird\n", 200}, {"tailLines=0", "", 200}, {"limitBytes=3", "fir", 200}, {"tailLines=1&limitBytes=2", "th", 200},
		{"follow=true", "", 501}, {"timestamps=true", "", 501}, {"previous=true", "", 501}, {"sinceSeconds=1", "", 501}, {"sinceTime=2026-10-18T00:00:00Z", "", 501}, {"tailLines=-1", "", 400}, {"limitBytes=0", "", 400},
	} {
		t.Run(tc.query, func(t *testing.T) {
			h, _, engine, _ := kubeletFixture(t)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, kubeletRequest("/containerLogs/test/test/server?"+tc.query, http.MethodGet))
			if w.Code != tc.code {
				t.Fatalf("got HTTP %d: %s", w.Code, w.Body.String())
			}
			if tc.code == 200 && w.Body.String() != tc.want {
				t.Fatalf("got %q, want %q", w.Body.String(), tc.want)
			}
			if tc.code != 200 && engine.logCalls != 0 {
				t.Fatal("unsupported request read logs")
			}
		})
	}
	for _, tc := range []struct {
		text string
		tail int
		want string
	}{{"a\nb", 1, "b"}, {"a\n", 20, "a\n"}, {"a\n\n", 1, "\n"}, {"", 1, ""}} {
		if got := kubeletLogSnapshot(tc.text, tc.tail, true, 0); got != tc.want {
			t.Errorf("tail %q got %q want %q", tc.text, got, tc.want)
		}
	}
}

func TestKubeletPortMappingCannotReachArbitraryHostPort(t *testing.T) {
	_, _, engine, p := kubeletFixture(t)
	if port, err := kubeletTargetPort(p, engine.instances[0], 8080); err != nil || port != 31001 {
		t.Fatalf("allocated mapping: %d %v", port, err)
	}
	for _, port := range []int32{22, 31001, 0, 65536} {
		if _, err := kubeletTargetPort(p, engine.instances[0], port); err == nil {
			t.Fatalf("accepted undeclared host port %d", port)
		}
	}
	i := engine.instances[0]
	i.Ports = []int{8080, 9090}
	p.Spec.Containers[0].Ports = append(p.Spec.Containers[0].Ports, core.ContainerPort{ContainerPort: 9090})
	if port, err := kubeletTargetPort(p, i, 9090); err != nil || port != 9090 {
		t.Fatalf("fixed mapping: %d %v", port, err)
	}
	i.Ports = []int{8080}
	if _, err := kubeletTargetPort(p, i, 9090); err == nil {
		t.Fatal("accepted declared but unowned fixed port")
	}
	i.Phase = "Failed"
	if _, err := kubeletTargetPort(p, i, 8080); err == nil {
		t.Fatal("forwarded stopped workload")
	}
}

func TestKubeletPortSessionCannotFollowReplacementPod(t *testing.T) {
	h, client, engine, p := kubeletFixture(t)
	old := p.UID
	p.UID = "replacement"
	engine.instances[0].UID = string(p.UID)
	if _, err := client.CoreV1().Pods(p.Namespace).Update(context.Background(), p, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ownedPod(context.Background(), p.Namespace, p.Name, "", old); err == nil {
		t.Fatal("old session attached to replacement Pod")
	}
}

func TestKubeletSPDYForwardsOnlyDeclaredWorkloadPort(t *testing.T) {
	h, _, engine, _ := kubeletFixture(t)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	engine.instances[0].Port = target.Addr().(*net.TCPAddr).Port
	targetDone := make(chan struct{})
	go func() {
		defer close(targetDone)
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		io.Copy(conn, conn)
	}()
	// TLS authentication is tested separately; this fixture supplies an already
	// verified identity so this test isolates the real Kubernetes SPDY protocol.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.TLS = kubeletRequest("/", http.MethodGet).TLS
		h.ServeHTTP(w, r)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL + "/portForward/test/test")
	if err != nil {
		t.Fatal(err)
	}
	transport, upgrader, err := spdy.RoundTripperFor(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, endpoint)
	stop, ready := make(chan struct{}), make(chan struct{})
	defer close(stop)
	forwarder, err := portforward.New(dialer, []string{"0:8080"}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- forwarder.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("SPDY forwarding failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("SPDY setup timed out")
	}
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) != 1 {
		t.Fatalf("forwarded ports: %v %v", ports, err)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(ports[0].Local)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	message := "SPDY workload echo"
	if _, err := io.WriteString(conn, message); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	got := make([]byte, len(message))
	_, err = io.ReadFull(conn, got)
	conn.Close()
	if err != nil || string(got) != message {
		t.Fatalf("SPDY returned %q: %v", got, err)
	}
	forwarder.Close()
	target.Close()
	select {
	case <-targetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("workload connection was not closed")
	}
}

func TestKubeletDisabledAndPartialTLSConfiguration(t *testing.T) {
	port, stop, err := startKubelet(context.Background(), nil, "", nil, NodeOptions{})
	if err != nil || port != 0 || stop == nil {
		t.Fatalf("disabled listener: %d %v", port, err)
	}
	stop()
	for _, o := range []NodeOptions{{KubeletCert: "certificate"}, {KubeletKey: "key"}, {KubeletClientCA: "ca"}, {KubeletCert: "certificate", KubeletKey: "key"}} {
		if _, _, err := startKubelet(context.Background(), &rest.Config{}, "node", &kubeletTestEngine{}, o); err == nil {
			t.Fatal("accepted incomplete TLS configuration")
		}
	}
}

func kubeletTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return parsed, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func kubeletTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, client bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "api-client", Organization: []string{"kubelet-users"}}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	if client {
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certPEM, keyPEM
}

func TestKubeletTLSAcceptsOnlyConfiguredClientCA(t *testing.T) {
	h, _, _, _ := kubeletFixture(t)
	ca, caKey, caPEM := kubeletTestCA(t)
	_, serverPEM, serverKey := kubeletTestLeaf(t, ca, caKey, false)
	trusted, _, _ := kubeletTestLeaf(t, ca, caKey, true)
	otherCA, otherKey, _ := kubeletTestCA(t)
	untrusted, _, _ := kubeletTestLeaf(t, otherCA, otherKey, true)
	dir := t.TempDir()
	o := NodeOptions{KubeletCert: filepath.Join(dir, "server.crt"), KubeletKey: filepath.Join(dir, "server.key"), KubeletClientCA: filepath.Join(dir, "clients.crt")}
	for path, data := range map[string][]byte{o.KubeletCert: serverPEM, o.KubeletKey: serverKey, o.KubeletClientCA: caPEM} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	config, err := kubeletTLS(o)
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion != tls.VersionTLS12 || config.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("TLS policy weakened")
	}
	server := httptest.NewUnstartedServer(h)
	server.TLS = config
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	for _, tc := range []struct {
		name    string
		certs   []tls.Certificate
		success bool
	}{{"trusted", []tls.Certificate{trusted}, true}, {"no certificate", nil, false}, {"other CA", []tls.Certificate{untrusted}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: tc.certs, MinVersion: tls.VersionTLS12}}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
			res, err := client.Get(server.URL + "/containerLogs/test/test/server")
			if !tc.success {
				if err == nil {
					res.Body.Close()
					t.Fatal("untrusted TLS client accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != 200 || !strings.Contains(string(body), "first") {
				t.Fatalf("HTTP %d: %s", res.StatusCode, body)
			}
		})
	}
	// Explicitly exercise the configured listener, using a local fake API server.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "subjectaccessreviews") {
			io.WriteString(w, `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","status":{"allowed":false}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o.KubeletPort = reservation.Addr().(*net.TCPAddr).Port
	reservation.Close()
	o.AdvertiseIP = "127.0.0.1"
	port, stop, err := startKubelet(context.Background(), &rest.Config{Host: api.URL}, "node-test", h.engine, o)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{trusted}}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	res, err := client.Get((&url.URL{Scheme: "https", Host: net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), Path: "/containerLogs/test/test/server"}).String())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("live listener bypassed SAR: HTTP %d", res.StatusCode)
	}
	stop()
	stop()
}

type kubeletStreamingEngine struct {
	*kubeletTestEngine
	opts runtime.LogOptions
	uid  string
}

func (e *kubeletStreamingEngine) StreamLogs(ctx context.Context, uid string, opts runtime.LogOptions) (io.ReadCloser, error) {
	e.uid, e.opts = uid, opts
	return io.NopCloser(strings.NewReader("streamed\n")), nil
}
func TestKubeletStreamsLogOptions(t *testing.T) {
	h, _, base, p := kubeletFixture(t)
	base.instances[0].Executor = "docker"
	engine := &kubeletStreamingEngine{kubeletTestEngine: base}
	h.engine = engine
	w := httptest.NewRecorder()
	h.ServeHTTP(w, kubeletRequest("/containerLogs/test/test/server?follow=true&tailLines=0&timestamps=true&sinceSeconds=5&limitBytes=20", "GET"))
	if w.Code != 200 || w.Body.String() != "streamed\n" || engine.uid != string(p.UID) || engine.opts.Tail == nil || *engine.opts.Tail != 0 || !engine.opts.Follow || !engine.opts.Timestamps || engine.opts.SinceSeconds != 5 || engine.opts.LimitBytes != 20 {
		t.Fatalf("stream mapping: status=%d opts=%+v body=%q", w.Code, engine.opts, w.Body.String())
	}
	if base.logCalls != 0 || len(h.streams) != 0 {
		t.Fatal("snapshot called or stream slot leaked")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, kubeletRequest("/containerLogs/test/test/server", "GET"))
	if w.Code != 200 || engine.opts.Tail != nil {
		t.Fatal("absent tail changed into tail=0")
	}
	// The options are the runtime's to honour, for native programs too: they are passed on, not refused here.
	base.instances[0].Executor = "native"
	for _, query := range []string{"timestamps=true", "sinceSeconds=5", "sinceTime=2026-01-01T00:00:00Z", "previous=true"} {
		w = httptest.NewRecorder()
		engine.opts = runtime.LogOptions{}
		h.ServeHTTP(w, kubeletRequest("/containerLogs/test/test/server?"+query, "GET"))
		if w.Code != 200 {
			t.Fatalf("%s must reach the runtime for a native workload: %d", query, w.Code)
		}
	}
	if !engine.opts.Previous {
		t.Fatalf("previous=true must be passed on: %+v", engine.opts)
	}
}
