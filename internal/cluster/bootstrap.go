package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	api "k8s.io/client-go/tools/clientcmd/api"
)

type JoinRequest struct {
	Command  string `json:"command"`
	NodeName string `json:"nodeName"`
	ProxyURL string `json:"proxyURL,omitempty"`
}
type JoinInfo struct{ Endpoint, Token, Hash, ProxyURL string }

func proxyFor(raw string) (func(*http.Request) (*url.URL, error), error) {
	if raw == "" {
		return http.ProxyFromEnvironment, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "socks5" && u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("proxy must be an http, https or socks5 URL without embedded credentials")
	}
	return http.ProxyURL(u), nil
}

func ParseJoin(command string) (JoinInfo, error) {
	var out JoinInfo
	fields := strings.Fields(strings.ReplaceAll(command, "\\\n", " "))
	if len(fields) < 7 || fields[0] != "kubeadm" || fields[1] != "join" {
		return out, fmt.Errorf("expected kubeadm join endpoint --token TOKEN --discovery-token-ca-cert-hash sha256:HASH")
	}
	if _, _, err := net.SplitHostPort(fields[2]); err != nil {
		return out, fmt.Errorf("invalid API server host:port")
	}
	out.Endpoint = "https://" + fields[2]
	for i := 3; i < len(fields); i++ {
		pair := strings.SplitN(fields[i], "=", 2)
		key := pair[0]
		var value string
		if len(pair) == 2 {
			value = pair[1]
		} else {
			i++
			if i >= len(fields) {
				return out, fmt.Errorf("missing join option value")
			}
			value = fields[i]
		}
		switch key {
		case "--token":
			if out.Token != "" {
				return out, fmt.Errorf("duplicate token")
			}
			out.Token = value
		case "--discovery-token-ca-cert-hash":
			if out.Hash != "" {
				return out, fmt.Errorf("duplicate CA hash")
			}
			out.Hash = strings.TrimPrefix(value, "sha256:")
			if !strings.HasPrefix(value, "sha256:") {
				return out, fmt.Errorf("SHA256 CA pin required")
			}
		default:
			return out, fmt.Errorf("unsupported join option %q", key)
		}
	}
	if !regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`).MatchString(out.Token) {
		return out, fmt.Errorf("invalid bootstrap token format")
	}
	decoded, err := hex.DecodeString(out.Hash)
	if err != nil || len(decoded) != 32 {
		return out, fmt.Errorf("invalid CA hash")
	}
	return out, nil
}

// Discover initially retrieves only public CA material without authentication.
// No credential is sent until its SPKI pin has matched the user-provided hash.
func Discover(ctx context.Context, info JoinInfo) ([]byte, error) {
	proxy, err := proxyFor(info.ProxyURL)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{Proxy: proxy, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // public discovery only, followed by explicit pin
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", info.Endpoint+"/api/v1/namespaces/kube-public/configmaps/cluster-info", nil)
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("public discovery connection failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("public discovery returned HTTP %d", resp.StatusCode)
	}
	var cm corev1.ConfigMap
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cm); err != nil {
		return nil, err
	}
	cfg, err := clientcmd.Load([]byte(cm.Data["kubeconfig"]))
	if err != nil {
		return nil, fmt.Errorf("invalid discovery kubeconfig")
	}
	for _, cluster := range cfg.Clusters {
		remaining := cluster.CertificateAuthorityData
		for len(remaining) > 0 {
			block, rest := pem.Decode(remaining)
			if block == nil {
				break
			}
			remaining = rest
			cert, e := x509.ParseCertificate(block.Bytes)
			if e != nil || !cert.IsCA {
				continue
			}
			hash := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
			if hex.EncodeToString(hash[:]) == info.Hash {
				return pem.EncodeToMemory(block), nil
			}
		}
	}
	return nil, fmt.Errorf("CA pin mismatch: refusing to send bootstrap token")
}

func ValidateNodeName(name string) error {
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return fmt.Errorf("node name must be DNS-compatible")
	}
	return nil
}

func Bootstrap(ctx context.Context, dir string, req JoinRequest, progress func(string)) (*rest.Config, error) {
	if err := ValidateNodeName(req.NodeName); err != nil {
		return nil, err
	}
	info, err := ParseJoin(req.Command)
	if err != nil {
		return nil, err
	}
	info.ProxyURL = req.ProxyURL
	if info.ProxyURL == "" {
		info.ProxyURL = os.Getenv("HTTPS_PROXY")
	}
	proxy, err := proxyFor(info.ProxyURL)
	if err != nil {
		return nil, err
	}
	ca, err := Discover(ctx, info)
	if err != nil {
		return nil, err
	}
	cfg := &rest.Config{Host: info.Endpoint, TLSClientConfig: rest.TLSClientConfig{CAData: ca}, BearerToken: info.Token, Timeout: 15 * time.Second, Proxy: proxy}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "system:node:" + req.NodeName, Organization: []string{"system:nodes"}}}, key)
	if err != nil {
		return nil, err
	}
	expiration := int32(86400 * 7)
	csr, err := client.CertificatesV1().CertificateSigningRequests().Create(ctx, &certv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{GenerateName: req.NodeName + "-"}, Spec: certv1.CertificateSigningRequestSpec{Request: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), SignerName: "kubernetes.io/kube-apiserver-client-kubelet", Usages: []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageClientAuth}, ExpirationSeconds: &expiration}}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create bootstrap CSR: %w", err)
	}
	progress("Waiting for CSR approval: " + csr.Name)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		csr, err = client.CertificatesV1().CertificateSigningRequests().Get(ctx, csr.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("read CSR: %w", err)
		}
		for _, c := range csr.Status.Conditions {
			if c.Type == certv1.CertificateDenied || c.Type == certv1.CertificateFailed {
				return nil, fmt.Errorf("CSR %s denied or failed", csr.Name)
			}
		}
		if len(csr.Status.Certificate) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("CSR %s not issued before timeout; ask cluster admin to inspect it", csr.Name)
		case <-ticker.C:
		}
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	kube := api.NewConfig()
	kube.Clusters["cluster"] = &api.Cluster{Server: info.Endpoint, CertificateAuthorityData: ca, ProxyURL: info.ProxyURL}
	kube.AuthInfos["node"] = &api.AuthInfo{ClientCertificateData: csr.Status.Certificate, ClientKeyData: keyPEM}
	kube.Contexts["node"] = &api.Context{Cluster: "cluster", AuthInfo: "node"}
	kube.CurrentContext = "node"
	data, err := clientcmd.Write(*kube)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = writePrivate(filepath.Join(dir, "node.kubeconfig"), data); err != nil {
		return nil, err
	}
	if err = writePrivate(filepath.Join(dir, "node-name"), []byte(req.NodeName)); err != nil {
		return nil, err
	}
	return clientcmd.RESTConfigFromKubeConfig(data)
}

func writePrivate(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}
