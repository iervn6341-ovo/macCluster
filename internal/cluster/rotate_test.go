package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	api "k8s.io/client-go/tools/clientcmd/api"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour * 365), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert, key}
}

func (c *testCA) issue(t *testing.T, subject pkix.Name, pub *ecdsa.PublicKey, notBefore, notAfter time.Time) []byte {
	t.Helper()
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: subject, NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, pub, c.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func nodeKubeconfig(t *testing.T, ca *testCA, subject pkix.Name, notBefore, notAfter time.Time) (*api.Config, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	kube := api.NewConfig()
	kube.Clusters["cluster"] = &api.Cluster{Server: "https://192.0.2.1:6443", CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})}
	kube.AuthInfos["node"] = &api.AuthInfo{ClientCertificateData: ca.issue(t, subject, &key.PublicKey, notBefore, notAfter), ClientKeyData: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})}
	kube.Contexts["node"] = &api.Context{Cluster: "cluster", AuthInfo: "node"}
	kube.CurrentContext = "node"
	path := filepath.Join(t.TempDir(), "node.kubeconfig")
	data, err := clientcmd.Write(*kube)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return kube, path
}

var nodeSubject = pkix.Name{CommonName: "system:node:maccluster-test", Organization: []string{"system:nodes"}}

func TestCertLifeNeedsRotation(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	life := CertLife{NotBefore: start, NotAfter: start.Add(7 * 24 * time.Hour)}
	for _, tc := range []struct {
		after time.Duration
		want  bool
	}{{time.Hour, false}, {4 * 24 * time.Hour, false}, {5 * 24 * time.Hour, true}, {6*24*time.Hour + 2*time.Hour, true}, {8 * 24 * time.Hour, true}} {
		if got := life.NeedsRotation(start.Add(tc.after)); got != tc.want {
			t.Errorf("after %v: NeedsRotation=%v, want %v", tc.after, got, tc.want)
		}
	}
	short := CertLife{NotBefore: start, NotAfter: start.Add(20 * time.Hour)}
	if !short.NeedsRotation(start) {
		t.Error("less than 24h left must renew immediately")
	}
}

func signingClient(t *testing.T, ca *testCA, mutate func(*certv1.CertificateSigningRequest, *x509.CertificateRequest)) *fake.Clientset {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "certificatesigningrequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		csr := action.(k8stesting.CreateAction).GetObject().(*certv1.CertificateSigningRequest)
		csr.Name = csr.GenerateName + "abcde"
		block, _ := pem.Decode(csr.Spec.Request)
		req, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if csr.Spec.SignerName != certv1.KubeAPIServerClientKubeletSignerName || csr.Spec.ExpirationSeconds == nil || *csr.Spec.ExpirationSeconds != 7*24*3600 {
			t.Errorf("unexpected signer or expiration: %s %v", csr.Spec.SignerName, csr.Spec.ExpirationSeconds)
		}
		csr.Status.Certificate = ca.issue(t, req.Subject, req.PublicKey.(*ecdsa.PublicKey), time.Now().Add(-time.Minute), time.Now().Add(7*24*time.Hour))
		if mutate != nil {
			mutate(csr, req)
		}
		return false, csr, nil
	})
	return client
}

func TestRotateClientCertReplacesCredentialsAndKeepsPrevious(t *testing.T) {
	ca := newTestCA(t)
	kube, path := nodeKubeconfig(t, ca, nodeSubject, time.Now().Add(-6*24*time.Hour), time.Now().Add(24*time.Hour))
	before, _ := ClientCertLife(path)
	client := signingClient(t, ca, nil)
	// Make the client reactor persist the object so Get after Create works.
	var progress []string
	if err := rotateClientCert(context.Background(), client, kube, path, time.Now, time.Millisecond, func(s string) { progress = append(progress, s) }); err != nil {
		t.Fatal(err)
	}
	after, err := ClientCertLife(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.NotAfter.After(before.NotAfter.Add(5*24*time.Hour)) || after.NodeName != "system:node:maccluster-test" {
		t.Fatalf("certificate not renewed: before %v after %v", before, after)
	}
	prev, err := ClientCertLife(path + ".prev")
	if err != nil || !prev.NotAfter.Equal(before.NotAfter) {
		t.Fatalf("previous credentials not kept: %v %v", prev, err)
	}
	for _, p := range []string{path, path + ".prev"} {
		if info, _ := os.Stat(p); info.Mode().Perm() != 0600 {
			t.Errorf("%s permissions %v", p, info.Mode().Perm())
		}
	}
	if _, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: path}, nil).ClientConfig(); err != nil {
		t.Fatalf("renewed kubeconfig does not load: %v", err)
	}
	if len(progress) == 0 || !strings.Contains(progress[len(progress)-1], "renewed") {
		t.Errorf("progress: %v", progress)
	}
}

func TestRotateClientCertRefusals(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	t.Run("not a node certificate", func(t *testing.T) {
		kube, path := nodeKubeconfig(t, ca, pkix.Name{CommonName: "kubernetes-admin", Organization: []string{"system:masters"}}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		err := rotateClientCert(context.Background(), fake.NewSimpleClientset(), kube, path, time.Now, time.Millisecond, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "not a node client certificate") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("issued certificate for another key", func(t *testing.T) {
		kube, path := nodeKubeconfig(t, ca, nodeSubject, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		wrong, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		client := signingClient(t, ca, func(csr *certv1.CertificateSigningRequest, _ *x509.CertificateRequest) {
			csr.Status.Certificate = other.issue(t, nodeSubject, &wrong.PublicKey, time.Now(), time.Now().Add(7*24*time.Hour))
		})
		orig, _ := os.ReadFile(path)
		err := rotateClientCert(context.Background(), client, kube, path, time.Now, time.Millisecond, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("got %v", err)
		}
		if now, _ := os.ReadFile(path); string(now) != string(orig) {
			t.Fatal("credentials must be untouched after a refused certificate")
		}
	})
	t.Run("denied", func(t *testing.T) {
		kube, path := nodeKubeconfig(t, ca, nodeSubject, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		client := signingClient(t, ca, func(csr *certv1.CertificateSigningRequest, _ *x509.CertificateRequest) {
			csr.Status.Certificate = nil
			csr.Status.Conditions = []certv1.CertificateSigningRequestCondition{{Type: certv1.CertificateDenied, Status: "True"}}
		})
		err := rotateClientCert(context.Background(), client, kube, path, time.Now, time.Millisecond, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "Denied") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("issued with a different subject", func(t *testing.T) {
		kube, path := nodeKubeconfig(t, ca, nodeSubject, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		client := signingClient(t, ca, func(csr *certv1.CertificateSigningRequest, req *x509.CertificateRequest) {
			csr.Status.Certificate = ca.issue(t, pkix.Name{CommonName: "system:node:someone-else", Organization: []string{"system:nodes"}}, req.PublicKey.(*ecdsa.PublicKey), time.Now(), time.Now().Add(7*24*time.Hour))
		})
		err := rotateClientCert(context.Background(), client, kube, path, time.Now, time.Millisecond, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "unexpected subject") {
			t.Fatalf("got %v", err)
		}
	})
}
