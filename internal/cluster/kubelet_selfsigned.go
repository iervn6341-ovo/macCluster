package cluster

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// KubeletCertSelfSigned: the agent makes its own kubelet serving certificate (docs/production-enable-design.md, A1).
// The API server only needs to *reach* the kubelet endpoint; with no --kubelet-certificate-authority it does not verify
// the serving certificate, so a self-signed one works without anyone approving a CSR. If an administrator later sets
// that flag, use the "csr" mode (a kubelet-serving certificate the cluster CA signed).
const (
	KubeletCertSelfSigned = "self-signed"
	KubeletCertCSR        = "csr"

	selfSignedLife        = 365 * 24 * time.Hour
	selfSignedRenewBefore = 30 * 24 * time.Hour
	servingCertFile       = "kubelet-serving.crt"
	servingKeyFile        = "kubelet-serving.key"
	clusterCAFile         = "cluster-ca.pem"
)

// servingCert owns the self-signed certificate: it reuses a saved one that is still good for this address, makes a
// new one when the address changed or the old one is about to expire, and renews while the listener is running.
type servingCert struct {
	dir      string
	ip       net.IP
	nodeName string
	now      func() time.Time

	mu   sync.Mutex
	cert *tls.Certificate
}

func newServingCert(stateDir string, ip net.IP, nodeName string) (*servingCert, error) {
	s := &servingCert{dir: stateDir, ip: ip, nodeName: nodeName, now: time.Now}
	if _, err := s.get(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *servingCert) usable(c *tls.Certificate) bool {
	if c == nil || c.Leaf == nil {
		return false
	}
	now := s.now()
	if now.Before(c.Leaf.NotBefore) || now.Add(selfSignedRenewBefore).After(c.Leaf.NotAfter) {
		return false
	}
	return c.Leaf.VerifyHostname(s.ip.String()) == nil
}

func (s *servingCert) get() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usable(s.cert) {
		return s.cert, nil
	}
	if c, err := tls.LoadX509KeyPair(filepath.Join(s.dir, servingCertFile), filepath.Join(s.dir, servingKeyFile)); err == nil {
		if leaf, perr := x509.ParseCertificate(c.Certificate[0]); perr == nil {
			c.Leaf = leaf
			if s.usable(&c) {
				s.cert = &c
				return s.cert, nil
			}
		}
	}
	c, err := s.generate()
	if err != nil {
		return nil, err
	}
	s.cert = c
	return c, nil
}

func (s *servingCert) generate() (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	now := s.now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: s.nodeName, Organization: []string{"maccluster"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(selfSignedLife),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{s.nodeName},
		IPAddresses:           []net.IP{s.ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return nil, err
	}
	if err := writeFile0600(filepath.Join(s.dir, servingKeyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return nil, err
	}
	if err := writeFile0600(filepath.Join(s.dir, servingCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// GetCertificate is the tls.Config hook: it renews a certificate that is about to expire without a restart.
func (s *servingCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.get() }

func writeFile0600(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeClusterCA saves the cluster CA's public certificate: the kubelet endpoint only accepts client certificates it
// signed (the API server's kubelet client certificate is one).
func writeClusterCA(stateDir string, caData []byte, caFile string) (string, error) {
	if len(caData) == 0 && caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return "", fmt.Errorf("read cluster CA: %w", err)
		}
		caData = b
	}
	if len(caData) == 0 {
		return "", fmt.Errorf("the cluster credentials carry no CA certificate; set kubeletClientCA")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return "", fmt.Errorf("the cluster CA is not valid PEM")
	}
	path := filepath.Join(stateDir, clusterCAFile)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", err
	}
	if err := writeFile0600(path, caData); err != nil {
		return "", err
	}
	return path, nil
}
