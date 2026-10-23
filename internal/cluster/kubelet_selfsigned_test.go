package cluster

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelfSignedServingCertificateCoversTheAddressAndIsReused(t *testing.T) {
	dir := t.TempDir()
	ip := net.ParseIP("192.0.2.10")
	s, err := newServingCert(dir, ip, "mac-node")
	if err != nil {
		t.Fatal(err)
	}
	leaf := s.cert.Leaf
	if leaf.VerifyHostname("192.0.2.10") != nil || leaf.VerifyHostname("mac-node") != nil {
		t.Fatal("the certificate must cover the address and the node name")
	}
	if leaf.IsCA || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("not a plain server leaf: %+v", leaf)
	}
	if life := leaf.NotAfter.Sub(leaf.NotBefore); life < 360*24*time.Hour || life > 370*24*time.Hour {
		t.Fatalf("lifetime %v", life)
	}
	for _, f := range []string{servingCertFile, servingKeyFile} {
		if info, err := os.Stat(filepath.Join(dir, f)); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s: %v %v", f, info, err)
		}
	}
	// A restart reuses the saved certificate.
	again, err := newServingCert(dir, ip, "mac-node")
	if err != nil || !bytes.Equal(again.cert.Certificate[0], s.cert.Certificate[0]) {
		t.Fatalf("a valid certificate was replaced: %v", err)
	}
}

func TestSelfSignedCertificateFollowsAddressChangeAndExpiry(t *testing.T) {
	dir := t.TempDir()
	first, err := newServingCert(dir, net.ParseIP("192.0.2.10"), "n")
	if err != nil {
		t.Fatal(err)
	}
	// The address changed (DHCP): the saved certificate no longer covers it.
	moved, err := newServingCert(dir, net.ParseIP("192.0.2.77"), "n")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(moved.cert.Certificate[0], first.cert.Certificate[0]) || moved.cert.Leaf.VerifyHostname("192.0.2.77") != nil {
		t.Fatal("a certificate for the old address was kept")
	}
	// Within the renewal window the next handshake gets a new certificate, with no restart.
	old := moved.cert.Certificate[0]
	moved.now = func() time.Time { return time.Now().Add(selfSignedLife - 10*24*time.Hour) }
	got, err := moved.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got.Certificate[0], old) {
		t.Fatal("an expiring certificate was not renewed")
	}
	if !got.Leaf.NotAfter.After(moved.now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("renewed certificate is short-lived: %v", got.Leaf.NotAfter)
	}
}

func selfTestCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func clientCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kube-apiserver-kubelet-client"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestSelfSignedEndpointAcceptsOnlyClientsOfTheClusterCA(t *testing.T) {
	dir := t.TempDir()
	ca, caKey, caPEM := selfTestCA(t, "cluster-ca")
	otherCA, otherKey, _ := selfTestCA(t, "other-ca")
	caPath, err := writeClusterCA(dir, caPEM, "")
	if err != nil {
		t.Fatal(err)
	}
	serving, err := newServingCert(dir, net.ParseIP("127.0.0.1"), "n")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := kubeletTLSWith(caPath, nil, serving.GetCertificate)
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if tc, ok := c.(*tls.Conn); ok && tc.Handshake() == nil {
					c.Write([]byte("ok"))
				}
			}()
		}
	}()
	dial := func(cert *tls.Certificate) (string, error) {
		// Like the API server without --kubelet-certificate-authority: it does not verify the serving certificate.
		conf := &tls.Config{InsecureSkipVerify: true}
		if cert != nil {
			conf.Certificates = []tls.Certificate{*cert}
		}
		c, err := tls.Dial("tcp", l.Addr().String(), conf)
		if err != nil {
			return "", err
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 2)
		_, err = c.Read(b)
		return string(b), err
	}
	good := clientCert(t, ca, caKey)
	if got, err := dial(&good); err != nil || got != "ok" {
		t.Fatalf("a client of the cluster CA was refused: %q %v", got, err)
	}
	if _, err := dial(nil); err == nil {
		t.Fatal("a client without a certificate was served")
	}
	bad := clientCert(t, otherCA, otherKey)
	if _, err := dial(&bad); err == nil {
		t.Fatal("a client certificate from another CA was served")
	}
}

func TestClusterCAAndModeValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeClusterCA(dir, nil, ""); err == nil {
		t.Fatal("no CA was accepted")
	}
	if _, err := writeClusterCA(dir, []byte("not pem"), ""); err == nil {
		t.Fatal("garbage was accepted as a CA")
	}
	_, _, caPEM := selfTestCA(t, "c")
	path, err := writeClusterCA(dir, caPEM, "")
	if err != nil || path != filepath.Join(dir, clusterCAFile) {
		t.Fatalf("%q %v", path, err)
	}
	if _, _, err := startKubelet(nil, nil, "n", nil, NodeOptions{KubeletCertMode: "bogus"}); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if _, stop, err := startKubelet(nil, nil, "n", nil, NodeOptions{}); err != nil {
		t.Fatalf("no options must leave the endpoint off: %v", err)
	} else {
		stop()
	}
}
