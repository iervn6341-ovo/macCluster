package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	authz "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"maccluster/internal/runtime"
)

func startKubelet(ctx context.Context, cfg *rest.Config, nodeName string, engine runtime.Engine, o NodeOptions) (int, func(), error) {
	noop := func() {}
	selfSigned := o.KubeletCertMode == KubeletCertSelfSigned
	if o.KubeletCertMode != "" && o.KubeletCertMode != KubeletCertSelfSigned && o.KubeletCertMode != KubeletCertCSR {
		return 0, noop, fmt.Errorf("kubeletCertMode must be %q or %q", KubeletCertSelfSigned, KubeletCertCSR)
	}
	if !selfSigned && o.KubeletCert == "" && o.KubeletKey == "" && o.KubeletClientCA == "" {
		return 0, noop, nil
	}
	if !selfSigned && (o.KubeletCert == "" || o.KubeletKey == "" || o.KubeletClientCA == "") {
		return 0, noop, fmt.Errorf("kubelet requires serving certificate, key and client CA together")
	}
	if cfg == nil || engine == nil || nodeName == "" {
		return 0, noop, fmt.Errorf("kubelet requires a Kubernetes client, node name and engine")
	}
	ip := net.ParseIP(o.AdvertiseIP)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return 0, noop, fmt.Errorf("kubelet requires a specific advertise IP")
	}
	port := o.KubeletPort
	if port == 0 {
		port = 10250
	}
	if port < 1 || port > 65535 {
		return 0, noop, fmt.Errorf("invalid kubelet port")
	}
	var tlsConfig *tls.Config
	var err error
	if selfSigned {
		if o.StateDir == "" {
			return 0, noop, fmt.Errorf("self-signed kubelet certificate needs a state directory")
		}
		if o.KubeletClientCA == "" {
			if o.KubeletClientCA, err = writeClusterCA(o.StateDir, cfg.CAData, cfg.CAFile); err != nil {
				return 0, noop, err
			}
		}
		serving, serr := newServingCert(o.StateDir, ip, nodeName)
		if serr != nil {
			return 0, noop, fmt.Errorf("kubelet serving certificate: %w", serr)
		}
		if tlsConfig, err = kubeletTLSWith(o.KubeletClientCA, nil, serving.GetCertificate); err != nil {
			return 0, noop, err
		}
	} else {
		if tlsConfig, err = kubeletTLS(o); err != nil {
			return 0, noop, err
		}
		if err := tlsConfig.Certificates[0].Leaf.VerifyHostname(ip.String()); err != nil {
			return 0, noop, fmt.Errorf("kubelet serving certificate does not cover advertise IP")
		}
	}
	clientConfig := rest.CopyConfig(cfg)
	if clientConfig.Timeout == 0 {
		clientConfig.Timeout = 10 * time.Second
	}
	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return 0, noop, fmt.Errorf("create kubelet authorization client: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, noop, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	if err != nil {
		return 0, noop, fmt.Errorf("listen for kubelet: %w", err)
	}
	serverCtx, cancel := context.WithCancel(ctx)
	h := &kubeletAPI{client: client, nodeName: nodeName, engine: engine, streams: make(chan struct{}, 128), tracker: newUsageTracker(), stateDirPath: o.StateDir}
	server := &http.Server{Handler: h, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return serverCtx }}
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); server.Close() }) }
	go func() { <-serverCtx.Done(); stop() }()
	go func() { defer stop(); _ = server.Serve(tls.NewListener(listener, tlsConfig)) }()
	return listener.Addr().(*net.TCPAddr).Port, stop, nil
}

// kubeletTLSWith builds the listener's TLS config: the serving certificate is either fixed or supplied by a callback
// (a self-signed one that renews itself); clients must present a certificate the given CA signed.
func kubeletTLSWith(clientCA string, fixed *tls.Certificate, get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (*tls.Config, error) {
	ca, err := os.ReadFile(clientCA)
	if err != nil {
		return nil, fmt.Errorf("read kubelet client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("kubelet client CA contains no certificates")
	}
	c := &tls.Config{MinVersion: tls.VersionTLS12, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"}, GetCertificate: get}
	if fixed != nil {
		c.Certificates = []tls.Certificate{*fixed}
	}
	return c, nil
}

func kubeletTLS(o NodeOptions) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(o.KubeletCert, o.KubeletKey)
	if err != nil {
		return nil, fmt.Errorf("load kubelet serving certificate/key: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse kubelet serving certificate: %w", err)
	}
	if leaf.IsCA || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, fmt.Errorf("kubelet serving certificate must be a currently valid leaf")
	}
	cert.Leaf = leaf
	ca, err := os.ReadFile(o.KubeletClientCA)
	if err != nil {
		return nil, fmt.Errorf("read kubelet client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("kubelet client CA contains no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"}}, nil
}

type kubeletAPI struct {
	client       kubernetes.Interface
	nodeName     string
	engine       runtime.Engine
	streams      chan struct{}
	tracker      *usageTracker
	trackerOnce  sync.Once
	stateDirPath string
}

func (k *kubeletAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}
	identity := r.TLS.VerifiedChains[0][0].Subject
	if identity.CommonName == "" || identity.CommonName == "system:anonymous" {
		http.Error(w, "client identity required", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	isLogs := len(parts) == 4 && parts[0] == "containerLogs"
	isForward := len(parts) == 3 && parts[0] == "portForward"
	isExec := len(parts) == 4 && parts[0] == "exec"
	isAttach := len(parts) == 4 && parts[0] == "attach"
	// Node-level endpoints. The API server authorizes them as subresources of the node: stats and metrics have their
	// own, everything else is "proxy".
	subresource := "proxy"
	nodeLevel := false
	switch {
	case len(parts) == 1 && (parts[0] == "healthz" || parts[0] == "pods"):
		nodeLevel = true
	case len(parts) == 2 && parts[0] == "stats" && parts[1] == "summary":
		nodeLevel, subresource = true, "stats"
	case len(parts) == 2 && parts[0] == "metrics" && parts[1] == "resource":
		nodeLevel, subresource = true, "metrics"
	}
	if nodeLevel {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
	} else if (!isLogs && !isForward && !isExec && !isAttach) || parts[1] == "" || parts[2] == "" || ((isLogs || isExec || isAttach) && parts[3] == "") {
		http.NotFound(w, r)
		return
	} else if (isLogs && r.Method != http.MethodGet) || ((isForward || isExec || isAttach) && r.Method != http.MethodGet && r.Method != http.MethodPost) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	verb := "get"
	if r.Method == http.MethodPost {
		verb = "create"
	}
	groups := append([]string{}, identity.Organization...)
	groups = append(groups, "system:authenticated")
	reviewCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	review, err := k.client.AuthorizationV1().SubjectAccessReviews().Create(reviewCtx, &authz.SubjectAccessReview{Spec: authz.SubjectAccessReviewSpec{User: identity.CommonName, Groups: groups, ResourceAttributes: &authz.ResourceAttributes{Verb: verb, Group: "", Version: "v1", Resource: "nodes", Subresource: subresource, Name: k.nodeName}}}, meta.CreateOptions{})
	cancel()
	if err != nil || review == nil || !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
		http.Error(w, "node proxy access denied", http.StatusForbidden)
		return
	}
	if nodeLevel {
		if parts[0] == "healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("ok"))
			return
		}
		vkapi.PodHandler(vkapi.PodHandlerConfig{GetPodsFromKubernetes: k.nodePods, GetStatsSummary: k.statsSummary, GetMetricsResource: k.metricsResource}, false).ServeHTTP(w, r)
		return
	}
	container := ""
	if isLogs || isExec || isAttach {
		container = parts[3]
	}
	pod, _, err := k.ownedPod(r.Context(), parts[1], parts[2], container, "")
	if err != nil {
		http.Error(w, "Pod or container is not owned by this node", http.StatusNotFound)
		return
	}
	streamer, canStream := k.engine.(interface {
		StreamLogs(context.Context, string, runtime.LogOptions) (io.ReadCloser, error)
	})
	if isLogs {
		select {
		case k.streams <- struct{}{}:
			defer func() { <-k.streams }()
		default:
			http.Error(w, "too many active streams", http.StatusTooManyRequests)
			return
		}
		for _, name := range []string{"follow", "timestamps", "previous"} {
			if raw := r.URL.Query().Get(name); raw != "" {
				enabled, e := strconv.ParseBool(raw)
				if e != nil {
					http.Error(w, "invalid log option", http.StatusBadRequest)
					return
				}
				if enabled && !canStream {
					http.Error(w, name+" is not supported by this executor", http.StatusNotImplemented)
					return
				}
			}
		}
		if (r.URL.Query().Get("sinceTime") != "" || r.URL.Query().Get("sinceSeconds") != "") && !canStream {
			http.Error(w, "since filters are not supported for log snapshots", http.StatusNotImplemented)
			return
		}
	}
	// Bind the upgraded session to this UID. VK's SPDY callbacks use a new
	// background context, so explicitly retain the authenticated request context.
	uid := pod.UID
	handlers := vkapi.PodHandlerConfig{
		GetContainerLogs: func(ctx context.Context, ns, name, cn string, opts vkapi.ContainerLogOpts) (io.ReadCloser, error) {
			p, _, err := k.ownedPod(ctx, ns, name, cn, uid)
			if err != nil {
				return nil, err
			}
			if canStream {
				options := runtime.LogOptions{Follow: opts.Follow, LimitBytes: opts.LimitBytes, Timestamps: opts.Timestamps, SinceTime: opts.SinceTime, SinceSeconds: opts.SinceSeconds, Previous: opts.Previous}
				if r.URL.Query().Get("tailLines") != "" {
					options.Tail = &opts.Tail
				}
				rc, err := streamer.StreamLogs(ctx, string(p.UID), options)
				if errors.Is(err, runtime.ErrNoPreviousLogs) {
					return nil, errdefs.AsInvalidInput(fmt.Errorf("previous terminated container %q in pod %q not found", cn, name))
				}
				return rc, err
			}
			text, err := k.engine.Logs(ctx, string(p.UID))
			if err != nil {
				return nil, fmt.Errorf("read workload log snapshot: %w", err)
			}
			_, hasTail := r.URL.Query()["tailLines"]
			return io.NopCloser(strings.NewReader(kubeletLogSnapshot(text, opts.Tail, hasTail, opts.LimitBytes))), nil
		},
		RunInContainer: func(ctx context.Context, ns, name, cn string, argv []string, attach vkapi.AttachIO) error {
			return k.runInContainer(ctx, uid, ns, name, cn, argv, attach)
		},
		AttachToContainer: func(ctx context.Context, ns, name, cn string, attach vkapi.AttachIO) error {
			return k.attachToContainer(ctx, uid, ns, name, cn, attach)
		},
		PortForward: func(_ context.Context, ns, name string, port int32, stream io.ReadWriteCloser) error {
			return k.forward(r.Context(), ns, name, uid, port, stream)
		},
		StreamIdleTimeout: 30 * time.Second, StreamCreationTimeout: 30 * time.Second,
	}
	vkapi.PodHandler(handlers, false).ServeHTTP(w, r)
}

func (k *kubeletAPI) ownedPod(ctx context.Context, namespace, name, container string, expected types.UID) (*core.Pod, runtime.Instance, error) {
	p, err := k.client.CoreV1().Pods(namespace).Get(ctx, name, meta.GetOptions{})
	if err != nil {
		return nil, runtime.Instance{}, errdefs.NotFound("Pod unavailable")
	}
	if p.UID == "" || p.Spec.NodeName != k.nodeName || p.DeletionTimestamp != nil || (expected != "" && p.UID != expected) || len(p.Spec.Containers) != 1 || len(p.Spec.InitContainers) != 0 || len(p.Spec.EphemeralContainers) != 0 || (container != "" && p.Spec.Containers[0].Name != container) {
		return nil, runtime.Instance{}, errdefs.NotFound("Pod/container is not owned by this node")
	}
	for _, instance := range k.engine.List(ctx) {
		if instance.UID == string(p.UID) && instance.Namespace == namespace && instance.Name == name {
			return p, instance, nil
		}
	}
	return nil, runtime.Instance{}, errdefs.NotFound("Pod UID is not managed by this engine")
}

func kubeletLogSnapshot(text string, tail int, hasTail bool, limit int) string {
	if hasTail {
		if tail == 0 {
			return ""
		}
		end := len(text)
		if end > 0 && text[end-1] == '\n' {
			end--
		}
		for lines := 1; lines < tail && end > 0; lines++ {
			i := strings.LastIndexByte(text[:end], '\n')
			if i < 0 {
				end = 0
				break
			}
			end = i
		}
		if end > 0 {
			start := strings.LastIndexByte(text[:end], '\n')
			text = text[start+1:]
		}
	}
	if limit > 0 && len(text) > limit {
		text = text[:limit]
	}
	return text
}

func kubeletTargetPort(p *core.Pod, instance runtime.Instance, requested int32) (int, error) {
	if requested < 1 || requested > 65535 || instance.Phase != "Running" {
		return 0, fmt.Errorf("port-forward requires a running workload and a valid port")
	}
	declared := false
	for _, port := range p.Spec.Containers[0].Ports {
		if port.ContainerPort == requested && (port.Protocol == "" || port.Protocol == core.ProtocolTCP) {
			declared = true
		}
	}
	if !declared {
		return 0, fmt.Errorf("TCP port is not declared by this Pod")
	}
	if len(instance.Ports) > 0 {
		for _, port := range instance.Ports {
			if port == int(requested) {
				return port, nil
			}
		}
		return 0, fmt.Errorf("port is not owned by this workload")
	}
	if len(p.Spec.Containers[0].Ports) != 1 || instance.Port < 1 || instance.Port > 65535 {
		return 0, fmt.Errorf("workload port mapping is unavailable")
	}
	return instance.Port, nil
}

func (k *kubeletAPI) forward(ctx context.Context, namespace, name string, uid types.UID, port int32, stream io.ReadWriteCloser) error {
	p, instance, err := k.ownedPod(ctx, namespace, name, "", uid)
	if err != nil {
		return err
	}
	target, err := kubeletTargetPort(p, instance, port)
	if err != nil {
		return err
	}
	select {
	case k.streams <- struct{}{}:
		defer func() { <-k.streams }()
	default:
		return fmt.Errorf("port-forward connection limit reached")
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(target)))
	if err != nil {
		return fmt.Errorf("connect workload port: %w", err)
	}
	defer conn.Close()
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close(); stream.Close() })
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(conn, stream)
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
	}()
	_, err = io.Copy(stream, conn)
	conn.Close()
	stream.Close()
	<-done
	return err
}
