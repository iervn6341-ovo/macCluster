package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	api "k8s.io/client-go/tools/clientcmd/api"
	runtime "maccluster/internal/runtime"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestForeignNodeOwnershipStopsBeforeExecution(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			w.WriteHeader(409)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","code":409}`))
			return
		}
		w.Write([]byte(`{"kind":"Node","apiVersion":"v1","metadata":{"name":"maccluster-test","annotations":{"maccluster.example.com/installation-id":"other-installation"}}}`))
	}))
	defer server.Close()
	err := Run(context.Background(), &rest.Config{Host: server.URL}, "maccluster-test", "this-installation", nil, func(string, string) { t.Error("foreign node started") })
	if err == nil || !strings.Contains(err.Error(), "different installation") {
		t.Fatalf("expected ownership rejection, got %v", err)
	}
}

func TestDiscoveryPinsCAWithoutSendingToken(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pin := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	cfg := api.NewConfig()
	cfg.Clusters["test"] = &api.Cluster{CertificateAuthorityData: ca}
	data, _ := clientcmd.Write(*cfg)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("bootstrap token exposed before pin verification")
		}
		json.NewEncoder(w).Encode(corev1.ConfigMap{Data: map[string]string{"kubeconfig": string(data)}})
	}))
	defer server.Close()
	info := JoinInfo{Endpoint: server.URL, Hash: hex.EncodeToString(pin[:]), Token: "must-not-be-sent"}
	if _, err := Discover(context.Background(), info); err != nil {
		t.Fatal(err)
	}
	info.Hash = strings.Repeat("0", 64)
	if _, err := Discover(context.Background(), info); err == nil {
		t.Fatal("wrong pin accepted")
	}
}

func TestReadinessPortAndThresholds(t *testing.T) {
	p := testPod()
	p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8000}}
	p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(9000)}}}
	if _, err := Translate(p); err == nil {
		t.Fatal("mismatched readiness port accepted")
	}
	var good atomic.Bool
	good.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !good.Load() {
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt(1234)}}, SuccessThreshold: 2, FailureThreshold: 2}
	state := &probeState{}
	inst := runtime.Instance{Phase: "Running", Port: port, StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	pod := testPod()
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 1234}}
	pod.Spec.Containers[0].ReadinessProbe = probe
	ready := func() bool {
		state.readiness.next = time.Time{}
		return state.evaluate(context.Background(), pod, inst).ready
	}
	if ready() {
		t.Fatal("became ready too soon")
	}
	if !ready() {
		t.Fatal("failed success threshold")
	}
	good.Store(false)
	if !ready() {
		t.Fatal("failed before failure threshold")
	}
	if ready() {
		t.Fatal("ignored failure threshold")
	}
}
