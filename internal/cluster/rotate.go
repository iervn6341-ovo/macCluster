package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	api "k8s.io/client-go/tools/clientcmd/api"
)

const (
	// rotationFraction is how much of its lifetime a client certificate lives before renewal starts, as the
	// kubelet does (it renews between 70% and 90%); renewing early leaves time for several failed attempts.
	rotationFraction = 0.7
	rotationLifetime = 7 * 24 * time.Hour
	rotationTimeout  = 2 * time.Minute
)

// CertLife is the validity window of the node client certificate saved in a node.kubeconfig.
type CertLife struct {
	NotBefore, NotAfter time.Time
	NodeName            string
}

// NeedsRotation reports whether renewal should start: 70% of the lifetime has elapsed, or less than 24h is left.
func (l CertLife) NeedsRotation(now time.Time) bool {
	total := l.NotAfter.Sub(l.NotBefore)
	return !now.Before(l.NotBefore.Add(time.Duration(float64(total)*rotationFraction))) || l.NotAfter.Sub(now) < 24*time.Hour
}

func nodeAuth(kube *api.Config) (*api.AuthInfo, *x509.Certificate, error) {
	ctx := kube.Contexts[kube.CurrentContext]
	if ctx == nil || kube.AuthInfos[ctx.AuthInfo] == nil {
		return nil, nil, fmt.Errorf("node kubeconfig has no current user")
	}
	auth := kube.AuthInfos[ctx.AuthInfo]
	block, _ := pem.Decode(auth.ClientCertificateData)
	if block == nil || len(auth.ClientKeyData) == 0 {
		return nil, nil, fmt.Errorf("node kubeconfig has no embedded client certificate and key")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse client certificate: %w", err)
	}
	return auth, cert, nil
}

// ClientCertLife reads the node client certificate validity from a node kubeconfig file.
func ClientCertLife(path string) (CertLife, error) {
	kube, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return CertLife{}, err
	}
	_, cert, err := nodeAuth(kube)
	if err != nil {
		return CertLife{}, err
	}
	return CertLife{NotBefore: cert.NotBefore, NotAfter: cert.NotAfter, NodeName: cert.Subject.CommonName}, nil
}

// RotateClientCert requests a new client certificate for the Node with the current one and, once issued,
// replaces the credentials in the kubeconfig at path. The CSR is created as the node itself, so the cluster's
// certificate-rotation auto-approval applies and no bootstrap token or administrator credential is involved.
// The previous file is kept as path+".prev"; the old certificate stays valid until it expires.
func RotateClientCert(ctx context.Context, path string, progress func(string)) error {
	kube, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return err
	}
	cfg, err := clientcmd.NewDefaultClientConfig(*kube, nil).ClientConfig()
	if err != nil {
		return err
	}
	cfg.Timeout = 15 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	return rotateClientCert(ctx, client, kube, path, time.Now, 2*time.Second, progress)
}

func rotateClientCert(ctx context.Context, client kubernetes.Interface, kube *api.Config, path string, now func() time.Time, poll time.Duration, progress func(string)) error {
	_, current, err := nodeAuth(kube)
	if err != nil {
		return err
	}
	name := current.Subject.CommonName
	if len(current.Subject.Organization) != 1 || current.Subject.Organization[0] != "system:nodes" || len(name) <= len("system:node:") || name[:len("system:node:")] != "system:node:" {
		return fmt.Errorf("refusing to renew a certificate that is not a node client certificate")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: current.Subject}, key)
	if err != nil {
		return err
	}
	expiration := int32(rotationLifetime / time.Second)
	csrs := client.CertificatesV1().CertificateSigningRequests()
	csr, err := csrs.Create(ctx, &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name[len("system:node:"):] + "-renew-"},
		Spec: certv1.CertificateSigningRequestSpec{
			Request:           pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
			SignerName:        certv1.KubeAPIServerClientKubeletSignerName,
			Usages:            []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageClientAuth},
			ExpirationSeconds: &expiration,
		}}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create renewal CSR: %w", err)
	}
	progress("Waiting for renewal CSR: " + csr.Name)
	waitCtx, cancel := context.WithTimeout(ctx, rotationTimeout)
	defer cancel()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for len(csr.Status.Certificate) == 0 {
		for _, c := range csr.Status.Conditions {
			if c.Type == certv1.CertificateDenied || c.Type == certv1.CertificateFailed {
				return fmt.Errorf("renewal CSR %s was %s", csr.Name, c.Type)
			}
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("renewal CSR %s was not issued in time", csr.Name)
		case <-ticker.C:
		}
		if csr, err = csrs.Get(waitCtx, csr.Name, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("read renewal CSR: %w", err)
		}
	}
	block, _ := pem.Decode(csr.Status.Certificate)
	if block == nil {
		return fmt.Errorf("issued certificate is not PEM")
	}
	issued, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse issued certificate: %w", err)
	}
	pub, ok := issued.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return fmt.Errorf("issued certificate does not match the generated key")
	}
	if issued.Subject.CommonName != name || !issued.NotAfter.After(now().Add(time.Hour)) {
		return fmt.Errorf("issued certificate has an unexpected subject or lifetime")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	old, err := clientcmd.Write(*kube)
	if err != nil {
		return err
	}
	next := kube.DeepCopy()
	nextAuth, _, _ := nodeAuth(next)
	nextAuth.ClientCertificateData = csr.Status.Certificate
	nextAuth.ClientKeyData = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	data, err := clientcmd.Write(*next)
	if err != nil {
		return err
	}
	if err = writePrivate(path+".prev", old); err != nil {
		return fmt.Errorf("keep previous credentials: %w", err)
	}
	if err = writePrivate(path, data); err != nil {
		return err
	}
	progress("Client certificate renewed; valid until " + issued.NotAfter.UTC().Format(time.RFC3339))
	return nil
}
