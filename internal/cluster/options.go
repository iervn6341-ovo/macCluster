package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"maccluster/internal/hostinfo"
)

// NodeOptions is per-cluster, opt-in configuration in node-options.json. It
// never changes system routing or publishes all local containers automatically.
type NodeOptions struct {
	AdvertiseIP    string `json:"advertiseIP,omitempty"`
	ServiceNetwork bool   `json:"serviceNetwork,omitempty"`
	// ServiceNetworkAllowFrom limits who may use the Service entry ports to these sources (CIDRs or single addresses),
	// typically the cluster's nodes. Empty: every host that can reach this Mac may (not recommended).
	ServiceNetworkAllowFrom []string `json:"serviceNetworkAllowFrom,omitempty"`
	ReservedMemoryBytes     int64    `json:"reservedMemoryBytes,omitempty"`
	ReservedCPUMilli        int64    `json:"reservedCPUMilli,omitempty"`
	KubeletCert             string   `json:"kubeletCert,omitempty"`
	KubeletKey              string   `json:"kubeletKey,omitempty"`
	KubeletClientCA         string   `json:"kubeletClientCA,omitempty"`
	KubeletPort             int      `json:"kubeletPort,omitempty"`
	// KubeletCertMode: "self-signed" (the agent makes and renews its own serving certificate and takes the client CA
	// from its cluster credentials: nothing else to provide) or "csr" (the certificate files named above, for example a
	// kubelet-serving certificate the cluster signed). Unset: the endpoint is on only when the files are given.
	KubeletCertMode string `json:"kubeletCertMode,omitempty"`
	// NativeBackend selects how native programs are started: "process" (default) or "launchd".
	NativeBackend string `json:"nativeBackend,omitempty"`
	// KeepAwake (run-node only; `serve` uses the app setting): "when-workloads" (default), "always" or "never".
	KeepAwake string `json:"keepAwake,omitempty"`
	// EgressProxy is the in-cluster egress proxy (docs/egress-design.md), e.g. http://USER:PASSWORD@NODE-IP:PORT. Pods that
	// carry the annotation maccluster.example.com/cluster-egress: "true" get proxy environment variables for it.
	EgressProxy string `json:"egressProxy,omitempty"`
	// AllowedHostPaths: hostPath volumes of Docker Pods are only allowed below these absolute paths (none by default).
	AllowedHostPaths []string `json:"allowedHostPaths,omitempty"`
	// APIServerAddress ("host:port") is what Pods with a mounted ServiceAccount token get as KUBERNETES_SERVICE_HOST/PORT.
	// Default: the address of the cluster credentials, unless that is a loopback address.
	APIServerAddress string `json:"apiServerAddress,omitempty"`
	// LaunchdUID is the login user whose session runs the launchd jobs (0: the agent's own user).
	LaunchdUID int    `json:"launchdUID,omitempty"`
	StateDir   string `json:"-"`
}

func LoadNodeOptions(dir string) (NodeOptions, error) {
	o := NodeOptions{StateDir: dir}
	b, err := os.ReadFile(filepath.Join(dir, "node-options.json"))
	if os.IsNotExist(err) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	if err = json.Unmarshal(b, &o); err != nil {
		return o, err
	}
	return o, nil
}
func advertisedIP(ctx context.Context, endpoint, configured string) (string, error) {
	if configured != "" {
		ip := net.ParseIP(configured)
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
			return "", fmt.Errorf("advertiseIP must be a non-loopback local IP")
		}
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return "", err
		}
		for _, a := range addrs {
			v, _, e := net.ParseCIDR(a.String())
			if e == nil && v.Equal(ip) {
				return ip.String(), nil
			}
		}
		return "", fmt.Errorf("advertiseIP is not assigned to this host")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "443")
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP
	if ip.IsLoopback() || ip.IsUnspecified() {
		return "", fmt.Errorf("automatic address unavailable through loopback proxy; configure advertiseIP")
	}
	return ip.String(), nil
}
func updateHostStatus(ctx context.Context, n *core.Node, o NodeOptions, shares int) error {
	path := o.StateDir
	if path == "" {
		path = os.TempDir()
	}
	facts, err := hostinfo.Collect(ctx, path)
	if err != nil {
		return err
	}
	cpuReserve := o.ReservedCPUMilli
	if cpuReserve == 0 {
		cpuReserve = 1000
	}
	memReserve := o.ReservedMemoryBytes
	if memReserve == 0 {
		memReserve = facts.MemoryBytes / 4
		if memReserve < 4<<30 {
			memReserve = 4 << 30
		}
		if memReserve >= facts.MemoryBytes {
			memReserve = facts.MemoryBytes / 2
		}
	}
	if cpuReserve < 0 || cpuReserve >= facts.CPUCount*1000 || memReserve < 0 || memReserve >= facts.MemoryBytes {
		return fmt.Errorf("invalid node resource reservation")
	}
	if shares < 1 {
		shares = 1
	}
	n.Status.Capacity = core.ResourceList{core.ResourceCPU: *resource.NewQuantity(facts.CPUCount, resource.DecimalSI), core.ResourceMemory: *resource.NewQuantity(facts.MemoryBytes, resource.BinarySI), core.ResourcePods: resource.MustParse("20")}
	if facts.DiskError == "" && facts.DiskTotalBytes > 0 {
		// Scheduling by ephemeral-storage requests: the volume that holds the agent's state and workload files.
		n.Status.Capacity[core.ResourceEphemeralStorage] = *resource.NewQuantity(int64(facts.DiskTotalBytes), resource.BinarySI)
	}
	n.Status.Allocatable = core.ResourceList{core.ResourceCPU: *resource.NewMilliQuantity((facts.CPUCount*1000-cpuReserve)/int64(shares), resource.DecimalSI), core.ResourceMemory: *resource.NewQuantity((facts.MemoryBytes-memReserve)/int64(shares), resource.BinarySI), core.ResourcePods: resource.MustParse("20")}
	if facts.DiskError == "" && facts.DiskTotalBytes > 0 {
		n.Status.Allocatable[core.ResourceEphemeralStorage] = *resource.NewQuantity(int64(facts.DiskAvailableBytes), resource.BinarySI)
	}
	n.Status.NodeInfo.MachineID, n.Status.NodeInfo.SystemUUID, n.Status.NodeInfo.BootID = facts.MachineID, facts.SystemUUID, facts.BootID
	n.Status.NodeInfo.OperatingSystem = goruntime.GOOS
	n.Status.NodeInfo.Architecture = facts.Architecture
	n.Status.NodeInfo.KernelVersion = facts.KernelVersion
	n.Status.NodeInfo.OSImage = "macOS " + facts.OSVersion
	pressure := core.ConditionUnknown
	if facts.MemoryPressure == hostinfo.PressureNormal {
		pressure = core.ConditionFalse
	} else if facts.MemoryPressure == hostinfo.PressureWarning || facts.MemoryPressure == hostinfo.PressureCritical {
		pressure = core.ConditionTrue
	}
	setNodeCondition(n, core.NodeMemoryPressure, pressure, "HostMemoryPressure", facts.MemoryPressure)
	disk := core.ConditionUnknown
	if facts.DiskError == "" && facts.DiskTotalBytes > 0 {
		disk = core.ConditionFalse
		if facts.DiskAvailableBytes < 5<<30 || float64(facts.DiskAvailableBytes)/float64(facts.DiskTotalBytes) < 0.05 {
			disk = core.ConditionTrue
		}
	}
	setNodeCondition(n, core.NodeDiskPressure, disk, "HostDiskSpace", "Monitors agent state volume; Docker VM disk has a separate budget")
	pids := core.ConditionUnknown
	pidReason, pidMessage := "NotMonitored", "Process table use could not be read: "+facts.ProcessError
	if facts.MaxProcesses > 0 && facts.ProcessError == "" {
		pids, pidReason = core.ConditionFalse, "HostProcessTable"
		pidMessage = fmt.Sprintf("%d of %d host processes (Per-Pod PID limits are not implemented)", facts.ProcessCount, facts.MaxProcesses)
		if facts.ProcessCount*10 >= facts.MaxProcesses*9 {
			pids = core.ConditionTrue
		}
	}
	setNodeCondition(n, core.NodePIDPressure, pids, pidReason, pidMessage)
	// Service forwarding does not establish a CNI Pod network.
	setNodeCondition(n, core.NodeNetworkUnavailable, core.ConditionTrue, "NoPodOverlay", "Opt-in Service forwarding only; no Pod IP network")
	return nil
}
func setNodeCondition(n *core.Node, t core.NodeConditionType, s core.ConditionStatus, reason, message string) {
	now := meta.NewTime(time.Now())
	v := core.NodeCondition{Type: t, Status: s, Reason: reason, Message: message, LastHeartbeatTime: now, LastTransitionTime: now}
	for i, old := range n.Status.Conditions {
		if old.Type == t {
			if old.Status == s {
				v.LastTransitionTime = old.LastTransitionTime
			}
			n.Status.Conditions[i] = v
			return
		}
	}
	n.Status.Conditions = append(n.Status.Conditions, v)
}
