package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maccluster/internal/servicenet"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"maccluster/internal/envs"
	runtime "maccluster/internal/runtime"
)

const ExecutorLabel = "maccluster.example.com/executor"
const ManagedLabel = "maccluster.example.com/managed"

// FixedPortsLabel declares ports the native program binds itself, "_" separated
// (label values cannot contain commas), e.g. "8100" or "8100_9100".
const FixedPortsLabel = "maccluster.example.com/fixed-ports"

// EnvironmentLabel selects a host-local environment for the native executor.
// Nodes advertise usable ones as env.maccluster.example.com/<name>=true.
const EnvironmentLabel = "maccluster.example.com/environment"
const EnvNodeLabelPrefix = "env.maccluster.example.com/"

type environmentLister interface{ Environments() []string }

// syncEnvLabels makes the Node's env labels match the usable environments.
// last is the set currently applied; only labels under our prefix are touched.
func syncEnvLabels(ctx context.Context, patch func(context.Context, []byte) error, engine runtime.Engine, last map[string]bool) (map[string]bool, error) {
	lister, ok := engine.(environmentLister)
	if !ok {
		return last, nil
	}
	want := map[string]bool{}
	for _, n := range lister.Environments() {
		want[n] = true
	}
	labels := map[string]any{}
	for n := range want {
		if !last[n] {
			labels[EnvNodeLabelPrefix+n] = "true"
		}
	}
	for n := range last {
		if !want[n] {
			labels[EnvNodeLabelPrefix+n] = nil
		}
	}
	if len(labels) == 0 {
		return last, nil
	}
	b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err := patch(ctx, b); err != nil {
		return last, err
	}
	return want, nil
}

// Translate converts a Pod into a runtime spec, or explains everything it cannot run (see SpecError).
func Translate(p *corev1.Pod) (runtime.Spec, error) {
	s, _, err := TranslateWithNotes(p)
	return s, err
}

// TranslateWithNotes is Translate plus the non-fatal things worth telling the user about.
func TranslateWithNotes(p *corev1.Pod) (runtime.Spec, []Note, error) {
	return TranslateWithOptions(p, TranslateOptions{})
}

// TranslateWithOptions is TranslateWithNotes with what it needs to read Secrets and ConfigMaps and to answer fieldRefs.
func TranslateWithOptions(p *corev1.Pod, o TranslateOptions) (runtime.Spec, []Note, error) {
	var notes []Note
	s := runtime.Spec{UID: string(p.UID), Name: p.Name, Namespace: p.Namespace, Executor: p.Labels[ExecutorLabel], RestartPolicy: string(p.Spec.RestartPolicy), Env: map[string]string{}}
	if s.Executor == "" {
		s.Executor = "docker"
	}
	if s.Executor != "docker" && s.Executor != "native" {
		return s, nil, invalid("label %s: unsupported executor %q (use docker or native)", ExecutorLabel, s.Executor)
	}
	if p.Labels[ManagedLabel] != "true" {
		return s, nil, &SpecError{Reason: ReasonNotManaged, Fields: []string{fmt.Sprintf("label %s=true is required during prototype testing", ManagedLabel)}}
	}
	var unsupported []string
	add := func(format string, args ...any) { unsupported = append(unsupported, fmt.Sprintf(format, args...)) }

	if n := len(p.Spec.Containers); n != 1 {
		add("containers: %d containers; one container per Pod is supported", n)
	}
	if n := len(p.Spec.InitContainers); n > 0 {
		add("initContainers: %d init containers", n)
	}
	if len(p.Spec.EphemeralContainers) > 0 {
		add("ephemeralContainers")
	}
	if p.Spec.HostNetwork || p.Spec.HostPID || p.Spec.HostIPC {
		if s.Executor == "native" {
			notes = append(notes, Note{corev1.EventTypeNormal, NoteHostNamespacesIgnored, "hostNetwork/hostPID/hostIPC are accepted: a native program already runs in the host's namespaces"})
		} else {
			add("hostNetwork/hostPID/hostIPC: Docker Desktop on macOS cannot share the host's namespaces; use the native executor (%s: native)", ExecutorLabel)
		}
	}
	sawTokenVolume := false
	mountsToken := tokenIsMounted(p, s.Executor)
	for _, v := range p.Spec.Volumes {
		if isServiceAccountVolume(v) && !mountsToken {
			sawTokenVolume = true
			continue
		}
		if s.Executor == "docker" && volumeKindSupported(v) {
			continue // checked when the container's mounts are planned
		}
		add("volumes: %q (%s) is not supported%s", v.Name, volumeKind(v), unsupportedVolumeHint(v))
	}
	if sawTokenVolume {
		notes = append(notes, Note{corev1.EventTypeWarning, NoteServiceAccountTokenNotMounted, "the ServiceAccount token is not mounted: this node cannot mount volumes yet, so the program will not find /var/run/secrets/kubernetes.io/serviceaccount. Set automountServiceAccountToken: false to silence this"})
	}
	if len(p.Spec.ImagePullSecrets) > 0 {
		add("imagePullSecrets")
	}
	if len(p.Spec.ReadinessGates) > 0 {
		add("readinessGates")
	}
	if len(p.Spec.Containers) == 0 {
		return s, notes, &SpecError{Reason: ReasonUnsupportedPodSpec, Fields: unsupported}
	}
	c := p.Spec.Containers[0]
	if s.Executor == "docker" {
		vr, verr := planVolumes(p, c, o, add)
		if verr != nil {
			return s, notes, verr
		}
		s.Mounts = vr.mounts
		notes = append(notes, vr.notes...)
		if o.Plans != nil {
			*o.Plans = vr.plans
		}
	} else {
		for _, m := range c.VolumeMounts {
			if sawTokenVolume && strings.HasPrefix(m.Name, "kube-api-access-") {
				continue
			}
			add("volumeMounts: %q at %s", m.Name, m.MountPath)
		}
	}
	if len(c.VolumeDevices) > 0 {
		add("volumeDevices")
	}
	checkEnvSupport(c, add)
	sec := securityFor(p, c, s.Executor, add)
	for name := range c.Resources.Limits {
		if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
			add("resources.limits.%s (only cpu and memory)", name)
		}
	}
	for kind, probe := range map[string]*corev1.Probe{"readinessProbe": c.ReadinessProbe, "livenessProbe": c.LivenessProbe, "startupProbe": c.StartupProbe} {
		if probe != nil && probe.GRPC != nil {
			add("%s: grpc probes are not supported (use httpGet, tcpSocket or exec)", kind)
		}
	}
	for _, port := range c.Ports {
		if port.HostPort != 0 {
			add("ports: hostPort %d", port.HostPort)
		}
		if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
			add("ports: %s port %d (only TCP)", port.Protocol, port.ContainerPort)
		}
	}
	if len(unsupported) > 0 {
		return s, notes, &SpecError{Reason: ReasonUnsupportedPodSpec, Fields: unsupported}
	}

	fixed, err := parseFixedPorts(p.Labels[FixedPortsLabel], s.Executor, c)
	if err != nil {
		return s, notes, invalid("%v", err)
	}
	s.FixedPorts = fixed
	if len(fixed) == 0 && len(c.Ports) > 1 {
		return s, notes, &SpecError{Reason: ReasonUnsupportedPodSpec, Fields: []string{fmt.Sprintf("ports: %d ports; declare several with the %s label (native only)", len(c.Ports), FixedPortsLabel)}}
	}
	if err := validateProbesAndHooks(c, s.FixedPorts); err != nil {
		return s, notes, err
	}
	if env, ok := p.Labels[EnvironmentLabel]; ok {
		if s.Executor != "native" {
			return s, notes, invalid("label %s requires %s=native", EnvironmentLabel, ExecutorLabel)
		}
		if !envs.ValidName(env) {
			return s, notes, invalid("label %s: invalid value", EnvironmentLabel)
		}
		s.Environment = env
	}
	s.Image = c.Image
	s.WorkingDir = c.WorkingDir
	if len(s.FixedPorts) > 0 {
		s.ContainerPort = s.FixedPorts[0]
	} else if len(c.Ports) == 1 {
		s.ContainerPort = int(c.Ports[0].ContainerPort)
	}
	env, err := buildEnv(o, p, c)
	if err != nil {
		return s, notes, err
	}
	notes = append(notes, env.notes...)
	s.Env, s.Command, s.Args = env.literal, env.command, env.args
	if len(env.resolved) > 0 {
		s.EnvResolved = env.resolved
	}
	if p.Annotations[EgressAnnotation] == "true" {
		if o.EgressProxy == "" {
			notes = append(notes, Note{corev1.EventTypeWarning, NoteEgressNotConfigured, EgressAnnotation + " is set but this node has no egressProxy configured: the workload cannot reach the cluster network"})
		} else if vars, verr := egressEnv(o.EgressProxy, o.HostIP); verr != nil {
			notes = append(notes, Note{corev1.EventTypeWarning, NoteEgressNotConfigured, "node option egressProxy is invalid: " + verr.Error()})
		} else {
			s.EnvResolved = applyEgress(s.Env, s.EnvResolved, vars)
		}
	}
	if len(c.Resources.Limits) > 0 {
		if s.Executor == "docker" {
			s.Limits = &runtime.ResourceSpec{CPUMilli: c.Resources.Limits.Cpu().MilliValue(), MemoryBytes: c.Resources.Limits.Memory().Value()}
		} else {
			notes = append(notes, Note{corev1.EventTypeWarning, NoteLimitsNotEnforced, "resources.limits are accepted but not enforced: macOS has no cgroups for a native program (use the Docker executor for hard limits)"})
		}
	}
	if tokenIsMounted(p, s.Executor) && podMountsAToken(p, c) {
		if vars, note := kubernetesServiceEnv(o.APIServer); note != nil {
			notes = append(notes, *note)
		} else {
			s.EnvResolved = applyEgress(s.Env, s.EnvResolved, vars)
		}
	}
	s.Security = sec
	return s, notes, nil
}

func volumeKind(v corev1.Volume) string {
	switch {
	case v.EmptyDir != nil:
		return "emptyDir"
	case v.ConfigMap != nil:
		return "configMap"
	case v.Secret != nil:
		return "secret"
	case v.HostPath != nil:
		return "hostPath"
	case v.PersistentVolumeClaim != nil:
		return "persistentVolumeClaim"
	case v.Projected != nil:
		return "projected"
	case v.DownwardAPI != nil:
		return "downwardAPI"
	}
	return "other"
}

func valueFromKind(v *corev1.EnvVarSource) string {
	switch {
	case v.FieldRef != nil:
		return "fieldRef " + v.FieldRef.FieldPath
	case v.ResourceFieldRef != nil:
		return "resourceFieldRef " + v.ResourceFieldRef.Resource
	case v.SecretKeyRef != nil:
		return "secretKeyRef " + v.SecretKeyRef.Name
	case v.ConfigMapKeyRef != nil:
		return "configMapKeyRef " + v.ConfigMapKeyRef.Name
	}
	return "unknown source"
}

// parseFixedPorts validates the fixed-ports label against the executor and the
// declared container ports. An empty label means the default dynamic port.
func parseFixedPorts(value, executor string, c corev1.Container) ([]int, error) {
	if value == "" {
		return nil, nil
	}
	if executor != "native" {
		return nil, fmt.Errorf("%s requires %s=native", FixedPortsLabel, ExecutorLabel)
	}
	var ports []int
	for _, part := range strings.Split(value, "_") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != part {
			return nil, fmt.Errorf("invalid %s value %q: use ports 1-65535 separated by \"_\"", FixedPortsLabel, value)
		}
		if containsPort(ports, n) {
			return nil, fmt.Errorf("invalid %s value %q: duplicate port %d", FixedPortsLabel, value, n)
		}
		ports = append(ports, n)
	}
	if len(ports) > 8 {
		return nil, fmt.Errorf("at most 8 ports are supported in %s", FixedPortsLabel)
	}
	for _, cp := range c.Ports {
		if !containsPort(ports, int(cp.ContainerPort)) {
			return nil, fmt.Errorf("containerPort %d is not in %s", cp.ContainerPort, FixedPortsLabel)
		}
	}
	return ports, nil
}

func containsPort(list []int, p int) bool {
	for _, v := range list {
		if v == p {
			return true
		}
	}
	return false
}

// resolveProbePort turns a numeric or named probe port into a number.
func resolveProbePort(p intstr.IntOrString, c corev1.Container) (int, bool) {
	if p.Type == intstr.Int || p.StrVal == "" {
		return int(p.IntVal), p.IntVal > 0
	}
	for _, cp := range c.Ports {
		if cp.Name == p.StrVal {
			return int(cp.ContainerPort), true
		}
	}
	return 0, false
}

// Run uses Virtual Kubelet's node lifecycle controller. The initial Pod adapter
// polls only this node's assigned Pods; it is deliberately a bounded subset of
// kubelet semantics, not a claim of conformance or a complete VK provider.
var ErrNodeRemoved = errors.New("node removed from cluster")

// Registration is owned by Run. The VK startup ensure must never recreate a
// Node deleted between our GET and its first status update.
type registeredNodeClient struct{ typedcorev1.NodeInterface }

func (registeredNodeClient) Create(context.Context, *corev1.Node, metav1.CreateOptions) (*corev1.Node, error) {
	return nil, ErrNodeRemoved
}

// PodObservation contains only resource identity and execution phase, never runtime errors or secrets.
type PodObservation struct {
	UID       string
	Namespace string
	Name      string
	Phase     string
}

type NodeIdentity struct {
	ObservePods func([]PodObservation)
	UID         string
	Save        func(string) error
	Options     NodeOptions
	ShareCount  func() int
}

// Only an authenticated NotFound or a different UID proves removal. Transport,
// authorization and server failures must never trigger local cleanup.
func checkNode(ctx context.Context, client kubernetes.Interface, name, uid string) error {
	n, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s", ErrNodeRemoved, name)
	}
	if err != nil {
		return err
	}
	if uid != "" && string(n.UID) != uid {
		return fmt.Errorf("%w: node identity changed", ErrNodeRemoved)
	}
	return nil
}

// VerifyNode is also used to recognize a remotely completed, interrupted exit.
func VerifyNode(ctx context.Context, cfg *rest.Config, name, uid string) error {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	return checkNode(ctx, client, name, uid)
}

func Run(ctx context.Context, cfg *rest.Config, name, installationID string, engine runtime.Engine, report func(string, string), identities ...NodeIdentity) error {
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = 15 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/hostname": name, "kubernetes.io/os": "darwin", "kubernetes.io/arch": "arm64", "maccluster.example.com/host": name, "capability.maccluster.example.com/docker": "true", "capability.maccluster.example.com/native": "true"}, Annotations: map[string]string{"maccluster.example.com/installation-id": installationID, "maccluster.example.com/network": "local-only; no Pod IP, ClusterIP, DNS or NetworkPolicy integration"}}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "maccluster.example.com/prototype", Value: "true", Effect: corev1.TaintEffectNoSchedule}}}, Status: corev1.NodeStatus{Capacity: corev1.ResourceList{}, NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "darwin", Architecture: "arm64", KubeletVersion: "v1.28.0-maccluster", ContainerRuntimeVersion: "maccluster://0.1"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "LocalExecutionOnly", Message: "Local executors available; cluster Pod networking is not implemented", LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now()}}}}
	node.Status.Allocatable = node.Status.Capacity.DeepCopy()
	var identity NodeIdentity
	if len(identities) > 0 {
		identity = identities[0]
	}
	if identity.UID != "" {
		if err := checkNode(ctx, client, name, identity.UID); err != nil {
			return err
		}
	}
	var createErr error
	if identity.UID == "" {
		_, createErr = client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	}
	if createErr != nil && !apierrors.IsAlreadyExists(createErr) {
		return fmt.Errorf("register node: %w", createErr)
	}
	existing, getErr := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	if identity.UID != "" && string(existing.UID) != identity.UID {
		return fmt.Errorf("%w: node identity changed during startup", ErrNodeRemoved)
	}
	if existing.Annotations["maccluster.example.com/installation-id"] != installationID {
		return fmt.Errorf("node %s belongs to a different installation; choose another name", name)
	}
	if identity.Save != nil {
		if err := identity.Save(string(existing.UID)); err != nil {
			return fmt.Errorf("persist node identity: %w", err)
		}
	}
	node.UID = existing.UID
	options := identity.Options
	address, addressErr := advertisedIP(ctx, cfg.Host, options.AdvertiseIP)
	if addressErr != nil && (options.ServiceNetwork || options.AdvertiseIP != "") {
		return addressErr
	}
	if address != "" {
		node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}, {Type: corev1.NodeHostName, Address: name}}
	}
	shares := 1
	if identity.ShareCount != nil {
		shares = identity.ShareCount()
	}
	if err := updateHostStatus(ctx, node, options, shares); err != nil {
		return fmt.Errorf("host capacity: %w", err)
	}
	network := &serviceNetwork{ip: address, enabled: options.ServiceNetwork, forwarding: servicenet.NewForwarder()}
	defer network.forwarding.Close()
	allowFrom, allowErr := servicenet.ParseAllowFrom(options.ServiceNetworkAllowFrom)
	if allowErr != nil {
		return allowErr
	}
	network.forwarding.SetAllowFrom(allowFrom)
	network.forwarding.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	options.AdvertiseIP = address
	kubeletPort, stopKubelet, err := startKubelet(ctx, cfg, name, engine, options)
	if err != nil {
		return err
	}
	defer stopKubelet()
	node.Status.DaemonEndpoints.KubeletEndpoint.Port = int32(kubeletPort)
	provider := vknode.NewNaiveNodeProvider()
	ctl, err := vknode.NewNodeController(provider, node, registeredNodeClient{client.CoreV1().Nodes()}, vknode.WithNodeEnableLeaseV1(client.CoordinationV1().Leases("kube-node-lease"), 40))
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- ctl.Run(runCtx) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	case <-ctl.Ready():
	}
	readyMessage := "Node registered; Docker/native execution active. Service forwarding and authenticated kubelet endpoints are opt-in; no Pod overlay."
	if options.ServiceNetwork && len(allowFrom) == 0 {
		readyMessage += " Service entry ports are open to every host that can reach this Mac: set serviceNetworkAllowFrom to the cluster's nodes."
	}
	report("Ready", readyMessage)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	probes := map[string]*probeState{}
	store := newProbeStore(options.StateDir)
	store.load(probes)
	settings := nodeSettings{observePods: identity.ObservePods, egressProxy: options.EgressProxy, cleaner: &volumeCleaner{}, volumes: volumeSettings{allowedHostPaths: options.AllowedHostPaths}}
	settings.apiServer = options.APIServerAddress
	if settings.apiServer == "" {
		if u, perr := url.Parse(cfg.Host); perr == nil && u.Host != "" {
			settings.apiServer = u.Host
			if u.Port() == "" {
				settings.apiServer = net.JoinHostPort(u.Hostname(), "443")
			}
		}
	}
	if options.StateDir != "" {
		// Absolute: Docker needs absolute host paths, and the agent may have been started with a relative --state-dir.
		if abs, aerr := filepath.Abs(options.StateDir); aerr == nil {
			settings.volumes.root = filepath.Join(abs, "volumes")
		}
	}
	// Start from what the Node already advertises so stale labels get removed.
	applied := map[string]bool{}
	for k := range existing.Labels {
		if strings.HasPrefix(k, EnvNodeLabelPrefix) {
			applied[strings.TrimPrefix(k, EnvNodeLabelPrefix)] = true
		}
	}
	for {
		var tickErr error
		applied, tickErr = tick(runCtx, client, name, string(existing.UID), engine, probes, applied, report, settings, network)
		store.save(probes)
		if identity.ShareCount != nil {
			shares = identity.ShareCount()
		}
		next := node.DeepCopy()
		if hostErr := updateHostStatus(runCtx, next, options, shares); hostErr == nil {
			node = next
			_ = provider.UpdateStatus(runCtx, node.DeepCopy())
		} else {
			report("Error", "Host sample failed: "+hostErr.Error())
		}
		if errors.Is(tickErr, ErrNodeRemoved) {
			return tickErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-ticker.C:
		}
	}
}

// tick runs one verify/label/reconcile round and reports the node status.
func tick(ctx context.Context, client kubernetes.Interface, name, uid string, engine runtime.Engine, probes map[string]*probeState, applied map[string]bool, report func(string, string), settings nodeSettings, networks ...*serviceNetwork) (map[string]bool, error) {
	if err := checkNode(ctx, client, name, uid); err != nil {
		if errors.Is(err, ErrNodeRemoved) {
			return applied, err
		}
		report("Error", "Cannot verify node: "+err.Error())
		return applied, nil
	}
	patchNode := func(ctx context.Context, b []byte) error {
		_, err := client.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, b, metav1.PatchOptions{})
		return err
	}
	applied, labelErr := syncEnvLabels(ctx, patchNode, engine, applied)
	if err := reconcile(ctx, client, name, engine, probes, settings, networks...); err != nil {
		report("Error", err.Error())
	} else if labelErr != nil {
		report("Error", "Cannot update environment labels: "+labelErr.Error())
	} else {
		report("Ready", "Local executors ready. Service forwarding requires explicit opt-in and its controller; no Pod overlay. The agent renews its Node client certificate automatically before it expires.")
	}
	return applied, nil
}

func reconcile(ctx context.Context, client kubernetes.Interface, name string, engine runtime.Engine, probes map[string]*probeState, settings nodeSettings, networks ...*serviceNetwork) error {
	list, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + name})
	if err != nil {
		return fmt.Errorf("list assigned Pods: %w", err)
	}
	observations := make([]PodObservation, len(list.Items))
	for i, pod := range list.Items {
		observations[i] = PodObservation{UID: string(pod.UID), Namespace: pod.Namespace, Name: pod.Name, Phase: string(pod.Status.Phase)}
	}
	if settings.observePods != nil {
		defer func() { settings.observePods(observations) }()
	}
	wantedRoutes := map[string]bool{}
	var network *serviceNetwork
	if len(networks) > 0 {
		network = networks[0]
		defer func() { network.forwarding.Retain(wantedRoutes) }()
	}
	desired := map[string]bool{}
	var failures []string
	for i := range list.Items {
		pod := &list.Items[i]
		uid := string(pod.UID)
		desired[uid] = true
		if probes[uid] == nil {
			probes[uid] = &probeState{}
		}
		state := probes[uid]
		if ex, ok := engine.(runtime.Execer); ok {
			state.exec = ex
		}
		if pod.DeletionTimestamp != nil {
			observations[i].Phase = "Terminating"
			if _, graceful := engine.(runtime.GraceStopper); graceful || (len(pod.Spec.Containers) > 0 && preStop(pod.Spec.Containers[0]) != nil) {
				// The preStop hook and the SIGTERM-then-wait stop take as long as the grace period: do them in the
				// background and keep serving the other Pods.
				finished, err := state.stop.beginStop(ctx, pod, engine, state.exec, func() runtime.Instance { return instanceOf(ctx, engine, uid) }, func(n Note) {
					state.notify(ctx, client, pod, name, n.Type, n.Reason, n.Message)
				})
				if !finished {
					continue
				}
				if err != nil {
					failures = append(failures, err.Error())
					continue
				}
			} else if err := engine.Stop(ctx, uid); err != nil {
				failures = append(failures, err.Error())
				continue
			}
			// Finish only an already-deleting Pod assigned to this node, after its
			// owned runtime has stopped. UID precondition prevents name-reuse races.
			zero := int64(0)
			err := client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero, Preconditions: &metav1.Preconditions{UID: &pod.UID}})
			if err != nil && !apierrors.IsNotFound(err) {
				failures = append(failures, err.Error())
			}
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		hostIP := ""
		if network != nil {
			hostIP = network.ip
		}
		var plans []*volumePlan
		spec, notes, e := TranslateWithOptions(pod, TranslateOptions{Ctx: ctx, Source: &podEnvSource{client: client, state: probes[uid]}, NodeName: name, HostIP: hostIP, EgressProxy: settings.egressProxy, Volumes: settings.volumes, Plans: &plans, APIServer: settings.apiServer})
		instance := runtime.Instance{UID: uid, Phase: "Pending"}
		if e == nil && len(plans) > 0 {
			// The volumes must exist, with their content, before the container starts; later calls keep it current.
			if perr := prepareVolumes(ctx, client, uid, &state.vol, settings.volumes.root, plans, time.Now()); perr != nil {
				e = &SpecError{Reason: ReasonFailedMount, Fields: []string{perr.Error()}}
			}
		}
		if e == nil {
			instance, e = engine.Ensure(ctx, spec)
			if instance.AdoptNote != "" {
				probes[uid].notify(ctx, client, pod, name, corev1.EventTypeWarning, ReasonSpecChangedSinceStart, instance.AdoptNote)
			}
		}
		var specErr *SpecError
		if e != nil {
			instance.Phase = "Pending"
			instance.Message = e.Error()
			errors.As(e, &specErr)
		}
		observations[i].Phase = instance.Phase
		status := makeStatus(ctx, pod, instance, probes[uid])
		// A Pod that cannot be run stays Pending (a Failed Pod would be replaced by its controller in a loop),
		// but says exactly why: in the container's waiting reason, the message and a Warning event.
		if specErr != nil {
			if len(status.ContainerStatuses) > 0 && status.ContainerStatuses[0].State.Waiting != nil {
				status.ContainerStatuses[0].State.Waiting.Reason = specErr.Reason
				status.ContainerStatuses[0].State.Waiting.Message = instance.Message
			}
			probes[uid].notify(ctx, client, pod, name, corev1.EventTypeWarning, specErr.Reason, instance.Message)
		}
		for _, n := range notes {
			probes[uid].notify(ctx, client, pod, name, n.Type, n.Reason, n.Message)
		}
		if network != nil {
			if err := network.publish(ctx, client, pod, instance, &status, wantedRoutes); err != nil {
				failures = append(failures, err.Error())
				status.Message = "Service endpoint unavailable: " + err.Error()
				for i := range status.Conditions {
					if status.Conditions[i].Type == corev1.PodReady || status.Conditions[i].Type == corev1.ContainersReady {
						status.Conditions[i].Status = corev1.ConditionFalse
					}
				}
			}
		}
		if err := patchStatus(ctx, client, pod, status); err != nil {
			failures = append(failures, err.Error())
		}
		state.act(ctx, client, pod, name, engine, instance)
	}
	for uid := range probes {
		if !desired[uid] {
			delete(probes, uid)
		}
	}
	instances := engine.List(ctx)
	for _, inst := range instances {
		if !desired[inst.UID] {
			if err := engine.Stop(ctx, inst.UID); err != nil {
				failures = append(failures, err.Error())
			}
		}
	}
	// A Pod's volumes are removed when the Pod has finished (Succeeded or Failed, nothing running) or is gone: this
	// list of Pods was read successfully, which is the only time anything is removed.
	if settings.cleaner != nil && settings.volumes.root != "" {
		keep := map[string]bool{}
		for i := range list.Items {
			if ph := list.Items[i].Status.Phase; ph != corev1.PodSucceeded && ph != corev1.PodFailed {
				keep[string(list.Items[i].UID)] = true
			}
		}
		active := map[string]bool{}
		for _, inst := range instances {
			if inst.Phase != "Succeeded" && inst.Phase != "Failed" {
				active[inst.UID] = true
			}
		}
		settings.cleaner.sweep(settings.volumes.root, func(uid string) bool { return keep[uid] }, func(uid string) bool { return active[uid] }, time.Now())
	}
	if len(failures) > 0 {
		return fmt.Errorf("reconcile: %s", strings.Join(failures, "; "))
	}
	return nil
}

func makeStatus(ctx context.Context, p *corev1.Pod, i runtime.Instance, probes ...*probeState) corev1.PodStatus {
	// A container that exited while its restart policy will start it again is in back-off: the Pod keeps
	// running and the container waits in CrashLoopBackOff, as kubectl and controllers expect.
	backoff := (i.Phase == "Failed" || i.Phase == "Succeeded") && (p.Spec.RestartPolicy == corev1.RestartPolicyAlways || (p.Spec.RestartPolicy == corev1.RestartPolicyOnFailure && i.ExitCode != 0))
	exited := i
	now := metav1.Now()
	ready := i.Phase == "Running"
	started := ready
	message := i.Message
	state := &probeState{}
	if len(probes) > 0 {
		state = probes[0]
	}
	if len(p.Spec.Containers) > 0 {
		out := state.evaluate(ctx, p, i)
		state.mu.Lock()
		state.last = out
		state.mu.Unlock()
		if ready {
			ready, started = out.ready, out.started
			if out.message != "" {
				message = out.message
			}
			// A program that has only just started has not earned "Ready" yet. kubelet only sees a container
			// that lives long enough to be noticed, so a program that exits at once never counts as ready; without
			// this a crash-looping update would show Ready for an instant on every restart, and a Deployment would
			// take that as a healthy new replica and remove an old one.
			if ready && ranLessThan(i.StartedAt, minRunBeforeReady) {
				ready = false
			}
		}
	}

	status := corev1.PodStatus{Phase: corev1.PodPhase(i.Phase), Message: message, StartTime: p.Status.StartTime}
	if backoff {
		status.Phase = corev1.PodRunning
	}
	if status.StartTime == nil {
		status.StartTime = &now
	}
	condition := corev1.ConditionFalse
	if ready {
		condition = corev1.ConditionTrue
	}
	status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, {Type: corev1.PodInitialized, Status: corev1.ConditionTrue}, {Type: corev1.ContainersReady, Status: condition}, {Type: corev1.PodReady, Status: condition}}
	for idx := range status.Conditions {
		status.Conditions[idx].LastTransitionTime = now
		for _, old := range p.Status.Conditions {
			if old.Type == status.Conditions[idx].Type && old.Status == status.Conditions[idx].Status {
				status.Conditions[idx].LastTransitionTime = old.LastTransitionTime
			}
		}
	}
	if len(p.Spec.Containers) == 0 {
		return status
	}
	c := p.Spec.Containers[0]
	cs := corev1.ContainerStatus{Name: c.Name, Image: c.Image, Ready: ready, RestartCount: int32(i.Restarts), Started: &started}
	if i.ContainerID != "" {
		cs.ContainerID = "docker://" + i.ContainerID
		if i.ImageID != "" {
			cs.ImageID = "docker://" + i.ImageID
		}
	} else if i.PID > 0 {
		cs.ContainerID = fmt.Sprintf("native://%d", i.PID)
	}
	switch {
	case backoff:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off before restarting the failed container"}
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: int32(exited.ExitCode), Reason: terminationReason(exited.ExitCode, exited.LastExitReason), FinishedAt: now}
	case i.Phase == "Running":
		running := now
		if t, e := time.Parse(time.RFC3339, i.StartedAt); e == nil {
			running = metav1.NewTime(t)
		}
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: running}
		if i.LastExitAt != "" {
			finished := now
			if t, e := time.Parse(time.RFC3339Nano, i.LastExitAt); e == nil {
				finished = metav1.NewTime(t)
			}
			cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: int32(i.LastExitCode), Reason: terminationReason(i.LastExitCode, i.LastExitReason), FinishedAt: finished}
		}
	case i.Phase == "Succeeded" || i.Phase == "Failed":
		cs.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: int32(i.ExitCode), Reason: terminationReason(i.ExitCode, i.LastExitReason), FinishedAt: now}
	default:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "RuntimePending", Message: message}
		status.Phase = corev1.PodPending
	}
	status.ContainerStatuses = []corev1.ContainerStatus{cs}
	return status
}

// minRunBeforeReady is how long a program must have been running before the Pod can be Ready.
const minRunBeforeReady = 2 * time.Second

// ranLessThan reports whether the run that began at startedAt (RFC 3339) is younger than d. An unknown start counts
// as old enough: the Pod is then judged by its probes alone.
func ranLessThan(startedAt string, d time.Duration) bool {
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	return err == nil && time.Since(t) < d
}

// terminationReason uses the words kubelet uses for a container that exited. The runtime's own reason wins for
// the one the kernel decides, a container killed for exceeding its memory limit.
func terminationReason(code int, reason string) string {
	if reason == "OOMKilled" {
		return reason
	}
	if code == 0 {
		return "Completed"
	}
	return "Error"
}

func patchStatus(ctx context.Context, c kubernetes.Interface, p *corev1.Pod, s corev1.PodStatus) error {
	statusJSON, _ := json.Marshal(s)
	var statusFields map[string]any
	json.Unmarshal(statusJSON, &statusFields)
	statusFields["message"] = s.Message
	b, _ := json.Marshal(map[string]any{"metadata": map[string]string{"uid": string(p.UID), "resourceVersion": p.ResourceVersion}, "status": statusFields})
	_, err := c.CoreV1().Pods(p.Namespace).Patch(ctx, p.Name, types.MergePatchType, b, metav1.PatchOptions{}, "status")
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

// probeState is everything the agent remembers about one Pod between reconciles.
type probeState struct {
	mu         sync.Mutex      // the background hook and stop goroutines write to it
	notified   map[string]bool // events already sent for this Pod, so each is sent once
	secrets    map[string]*corev1.Secret
	configMaps map[string]*corev1.ConfigMap

	vol                          volumeState
	exec                         runtime.Execer // refreshed every reconcile
	startup, liveness, readiness probeRun
	post                         hookRun
	last                         probeOutcome // the latest evaluation, read by reconcile after makeStatus
	killedFor                    string       // the container start a restart was already requested for
	stop                         stopTracker
}

func instanceOf(ctx context.Context, engine runtime.Engine, uid string) runtime.Instance {
	for _, i := range engine.List(ctx) {
		if i.UID == uid {
			return i
		}
	}
	return runtime.Instance{}
}

// act sends the events the latest probe evaluation produced and, once per container start, asks the runtime to
// restart a container whose liveness or startup probe or postStart hook failed (its restart policy then decides).
func (s *probeState) act(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, node string, engine runtime.Engine, i runtime.Instance) {
	s.mu.Lock()
	out := s.last
	s.last = probeOutcome{}
	again := out.kill != "" && s.killedFor == i.StartedAt
	if out.kill != "" && !again {
		s.killedFor = i.StartedAt
	}
	s.mu.Unlock()
	for _, n := range out.notes {
		if again && n.Reason == ReasonKilling {
			continue
		}
		s.notify(ctx, client, pod, node, n.Type, n.Reason, n.Message)
	}
	if out.kill == "" || again {
		return
	}
	killer, ok := engine.(runtime.Killer)
	if !ok || len(pod.Spec.Containers) == 0 {
		return
	}
	grace := podGrace(pod)
	if probe := killingProbe(pod.Spec.Containers[0], out.kill); probe != nil && probe.TerminationGracePeriodSeconds != nil {
		grace = time.Duration(*probe.TerminationGracePeriodSeconds) * time.Second
	}
	go killer.Kill(context.WithoutCancel(ctx), string(pod.UID), out.kill, grace)
}

func killingProbe(c corev1.Container, reason string) *corev1.Probe {
	switch {
	case strings.HasPrefix(reason, probeKindLiveness):
		return c.LivenessProbe
	case strings.HasPrefix(reason, probeKindStartup):
		return c.StartupProbe
	}
	return nil
}

// podEnvSource reads Secrets and ConfigMaps for one Pod and remembers what it read for as long as the Pod exists,
// like kubelet reading them once when it creates the container: the environment of a running program does not
// change under it, and an unchanged Pod does not cost an API call every few seconds. A failed read is not cached,
// so a Secret that appears later is picked up on the next reconcile.
type podEnvSource struct {
	client kubernetes.Interface
	state  *probeState
}

func (s *podEnvSource) Secret(ctx context.Context, ns, name string) (*corev1.Secret, error) {
	if v, ok := s.state.secrets[name]; ok {
		return v, nil
	}
	v, err := s.client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if s.state.secrets == nil {
		s.state.secrets = map[string]*corev1.Secret{}
	}
	s.state.secrets[name] = v
	return v, nil
}

func (s *podEnvSource) ConfigMap(ctx context.Context, ns, name string) (*corev1.ConfigMap, error) {
	if v, ok := s.state.configMaps[name]; ok {
		return v, nil
	}
	v, err := s.client.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if s.state.configMaps == nil {
		s.state.configMaps = map[string]*corev1.ConfigMap{}
	}
	s.state.configMaps[name] = v
	return v, nil
}

// notify sends one event per (type, reason, message) for a Pod. Failing to send is not an error for the Pod.
func (s *probeState) notify(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, node, typ, reason, message string) {
	key := typ + "|" + reason + "|" + message
	if s.notified[key] {
		return
	}
	if s.notified == nil {
		s.notified = map[string]bool{}
	}
	s.notified[key] = true
	if len(message) > 1000 {
		message = message[:1000]
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x", pod.Name, time.Now().UnixNano()), Namespace: pod.Namespace},
		InvolvedObject:      corev1.ObjectReference{Kind: "Pod", APIVersion: "v1", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
		Reason:              reason,
		Message:             message,
		Type:                typ,
		Source:              corev1.EventSource{Component: "maccluster-agent", Host: node},
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
		ReportingController: "maccluster.example.com/agent",
		ReportingInstance:   node,
	}
	if _, err := client.CoreV1().Events(pod.Namespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		s.notified[key] = false // try again on the next reconcile
	}
}
