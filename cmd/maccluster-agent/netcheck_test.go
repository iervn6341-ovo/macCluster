package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func probes(dial func(string) error, ping bool) netProbes {
	return netProbes{
		Dial: func(_ context.Context, a string) error { return dial(a) },
		Ping: func(context.Context, string) bool { return ping },
		Now:  func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) },
	}
}

func TestNoRouteWithAWorkingPingPointsAtTheLocalNetworkSetting(t *testing.T) {
	// The situation seen on macOS: nc and ping reach the control plane, the agent gets EHOSTUNREACH.
	c := checkReachable(context.Background(), probes(func(string) error { return syscall.EHOSTUNREACH }, true), netTarget{Name: "c", Endpoint: "https://192.0.2.10:6443"})
	if c.Status != checkFail || c.Action != "open-local-network-settings" || !strings.Contains(c.Hint, "區域網路") {
		t.Fatalf("expected the local-network hint with its button: %+v", c)
	}
}

func TestNoRouteWithoutPingIsAnOrdinaryNetworkProblem(t *testing.T) {
	c := checkReachable(context.Background(), probes(func(string) error { return syscall.EHOSTUNREACH }, false), netTarget{Name: "c", Endpoint: "https://192.0.2.10:6443"})
	if c.Status != checkFail || c.Action != "" || strings.Contains(c.Hint, "區域網路") {
		t.Fatalf("a host that does not answer ping must not be blamed on the privacy setting: %+v", c)
	}
}

func TestReachableRefusedAndTimeoutAreToldApart(t *testing.T) {
	target := netTarget{Name: "c", Endpoint: "https://192.0.2.10:6443"}
	if c := checkReachable(context.Background(), probes(func(a string) error {
		if a != "192.0.2.10:6443" {
			t.Fatalf("dialed %q", a)
		}
		return nil
	}, false), target); c.Status != checkOK {
		t.Fatalf("%+v", c)
	}
	if c := checkReachable(context.Background(), probes(func(string) error { return syscall.ECONNREFUSED }, false), target); c.Status != checkFail || !strings.Contains(c.Detail, "拒絕") {
		t.Fatalf("%+v", c)
	}
	if c := checkReachable(context.Background(), probes(func(string) error { return context.DeadlineExceeded }, false), target); c.Status != checkFail || !strings.Contains(c.Detail, "逾時") {
		t.Fatalf("%+v", c)
	}
	if c := checkReachable(context.Background(), probes(func(string) error { return nil }, false), netTarget{Name: "c", Endpoint: "::bad"}); c.Status != checkFail {
		t.Fatalf("a bad endpoint is a failure, not a crash: %+v", c)
	}
}

func writeKubeconfig(t *testing.T, proxy string, notAfter time.Time) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "system:node:test"}, NotBefore: notAfter.Add(-48 * time.Hour), NotAfter: notAfter}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	enc := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	proxyLine := ""
	if proxy != "" {
		proxyLine = "\n    proxy-url: " + proxy
	}
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://192.0.2.10:6443%s
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
users:
- name: u
  user:
    %s: %s
    %s: %s
`, proxyLine, "client-certificate-"+"data", enc(cert), "client-key-"+"data", enc(keyPEM))
	path := filepath.Join(t.TempDir(), "node.kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAProxyThatIsNotRunningIsReported(t *testing.T) {
	path := writeKubeconfig(t, "socks5://127.0.0.1:18080", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	target := netTarget{Name: "c", Endpoint: "https://192.0.2.10:6443", Kubeconfig: path}
	down := probes(func(a string) error {
		if a != "127.0.0.1:18080" {
			t.Fatalf("the proxy is what is dialed: %q", a)
		}
		return syscall.ECONNREFUSED
	}, false)
	c := checkProxy(context.Background(), down, target)
	if c.Status != checkFail || !strings.Contains(c.Detail, "127.0.0.1:18080") || !strings.Contains(c.Hint, "proxy-url") {
		t.Fatalf("%+v", c)
	}
	if c := checkProxy(context.Background(), probes(func(string) error { return nil }, false), target); c.Status != checkOK {
		t.Fatalf("%+v", c)
	}
	direct := writeKubeconfig(t, "", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if c := checkProxy(context.Background(), down, netTarget{Name: "c", Kubeconfig: direct}); c.Status != checkOK || !strings.Contains(c.Detail, "直接") {
		t.Fatalf("no proxy configured means nothing to dial: %+v", c)
	}
	if c := checkProxy(context.Background(), down, netTarget{Name: "c"}); c.Status != checkSkip {
		t.Fatalf("%+v", c)
	}
}

func TestNodeCertificateExpiry(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	p := probes(func(string) error { return nil }, false)
	for _, tc := range []struct {
		notAfter time.Time
		want     string
	}{
		{now.Add(-time.Hour), checkFail},
		{now.Add(3 * 24 * time.Hour), checkWarn},
		{now.Add(60 * 24 * time.Hour), checkOK},
	} {
		c := checkCertificate(p, netTarget{Name: "c", Kubeconfig: writeKubeconfig(t, "", tc.notAfter)})
		if c.Status != tc.want {
			t.Fatalf("expiring %s: %+v", tc.notAfter, c)
		}
	}
}

func TestRunNetChecksWithNoClusterAndWithOne(t *testing.T) {
	p := probes(func(string) error { return errors.New("unused") }, false)
	r := runNetChecks(context.Background(), p, nil)
	if len(r.Checks) != 1 || r.Checks[0].Status != checkSkip {
		t.Fatalf("nothing joined is a skip, not an error: %+v", r)
	}
	path := writeKubeconfig(t, "", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	r = runNetChecks(context.Background(), probes(func(string) error { return nil }, false), []netTarget{{Name: "c", Endpoint: "https://192.0.2.10:6443", Kubeconfig: path}})
	if len(r.Checks) != 3 || r.CheckedAt != "2026-10-22T12:00:00Z" {
		t.Fatalf("one cluster has a reachability, a proxy and a certificate check: %+v", r)
	}
}
