package cluster

import (
	"context"
	"sort"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/node/api/statsv1alpha1"
	"google.golang.org/protobuf/proto"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"maccluster/internal/hostinfo"
	"maccluster/internal/runtime"
)

// usageTracker turns cumulative CPU counters into the "nanocores right now" that the summary API also carries, by
// remembering the previous sample of each workload and of the node.
type usageTracker struct {
	mu        sync.Mutex
	started   time.Time
	last      map[string]cpuSample
	nodeCPU   float64 // cumulative core-seconds: the host load integrated over time, since macOS has no such counter
	nodeAt    time.Time
	nodeSteps cpuSample
}

type cpuSample struct {
	nanos uint64
	at    time.Time
}

func newUsageTracker() *usageTracker {
	return &usageTracker{started: time.Now(), last: map[string]cpuSample{}}
}

// rate returns the average CPU use since the previous sample of the same key, in nanocores.
func (t *usageTracker) rate(key string, nanos uint64, at time.Time) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.last[key]
	t.last[key] = cpuSample{nanos, at}
	if !ok || !at.After(prev.at) || nanos < prev.nanos {
		return 0
	}
	return uint64(float64(nanos-prev.nanos) / at.Sub(prev.at).Seconds())
}

// sampleNode integrates the host's CPU load into a counter and reads its memory use.
func (t *usageTracker) sampleNode(ctx context.Context) (cpuNanos uint64, memory uint64, at time.Time, err error) {
	load, err := hostinfo.CurrentLoad(ctx)
	if err != nil {
		return 0, 0, time.Now(), err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !t.nodeAt.IsZero() {
		t.nodeCPU += load.CPUCores * now.Sub(t.nodeAt).Seconds()
	}
	t.nodeAt = now
	return uint64(t.nodeCPU * 1e9), load.MemoryWorkingSetBytes, now, nil
}

// podUsage is one Pod of this node with what it is using.
type podUsage struct {
	pod   *core.Pod
	usage runtime.Usage
}

// track returns the usage tracker, creating it for a kubeletAPI that was built without one.
func (k *kubeletAPI) track() *usageTracker {
	k.trackerOnce.Do(func() {
		if k.tracker == nil {
			k.tracker = newUsageTracker()
		}
	})
	return k.tracker
}

func (k *kubeletAPI) nodePods(ctx context.Context) ([]*core.Pod, error) {
	list, err := k.client.CoreV1().Pods("").List(ctx, meta.ListOptions{FieldSelector: "spec.nodeName=" + k.nodeName})
	if err != nil {
		return nil, err
	}
	pods := make([]*core.Pod, 0, len(list.Items))
	for i := range list.Items {
		pods = append(pods, &list.Items[i])
	}
	return pods, nil
}

func (k *kubeletAPI) podUsages(ctx context.Context) []podUsage {
	reader, ok := k.engine.(runtime.UsageReader)
	if !ok {
		return nil
	}
	pods, err := k.nodePods(ctx)
	if err != nil {
		return nil
	}
	byUID := map[string]*core.Pod{}
	for _, p := range pods {
		byUID[string(p.UID)] = p
	}
	var out []podUsage
	for _, u := range reader.Usage(ctx) {
		if p := byUID[u.UID]; p != nil && len(p.Spec.Containers) > 0 {
			out = append(out, podUsage{p, u})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].pod.Namespace+"/"+out[i].pod.Name < out[j].pod.Namespace+"/"+out[j].pod.Name
	})
	return out
}

func u64(v uint64) *uint64 { return &v }

func (k *kubeletAPI) statsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	cpuNanos, mem, at, err := k.track().sampleNode(ctx)
	if err != nil {
		return nil, err
	}
	now := meta.NewTime(at)
	node := statsv1alpha1.NodeStats{
		NodeName:  k.nodeName,
		StartTime: meta.NewTime(k.track().started),
		CPU:       &statsv1alpha1.CPUStats{Time: now, UsageCoreNanoSeconds: u64(cpuNanos), UsageNanoCores: u64(k.track().rate("node", cpuNanos, at))},
		Memory:    &statsv1alpha1.MemoryStats{Time: now, UsageBytes: u64(mem), WorkingSetBytes: u64(mem)},
	}
	if facts, e := hostinfo.Collect(ctx, k.stateDir()); e == nil && facts.DiskError == "" && facts.DiskTotalBytes > 0 {
		node.Fs = &statsv1alpha1.FsStats{Time: now, AvailableBytes: u64(facts.DiskAvailableBytes), CapacityBytes: u64(facts.DiskTotalBytes), UsedBytes: u64(facts.DiskTotalBytes - facts.DiskAvailableBytes)}
	}
	summary := &statsv1alpha1.Summary{Node: node, Pods: []statsv1alpha1.PodStats{}}
	for _, pu := range k.podUsages(ctx) {
		t := meta.NewTime(pu.usage.At)
		cpu := &statsv1alpha1.CPUStats{Time: t, UsageCoreNanoSeconds: u64(pu.usage.CPUUsageNanoSeconds), UsageNanoCores: u64(k.track().rate(string(pu.pod.UID), pu.usage.CPUUsageNanoSeconds, pu.usage.At))}
		memory := &statsv1alpha1.MemoryStats{Time: t, UsageBytes: u64(pu.usage.MemoryWorkingSetBytes), WorkingSetBytes: u64(pu.usage.MemoryWorkingSetBytes)}
		start := meta.NewTime(pu.usage.StartedAt)
		summary.Pods = append(summary.Pods, statsv1alpha1.PodStats{
			PodRef:     statsv1alpha1.PodReference{Name: pu.pod.Name, Namespace: pu.pod.Namespace, UID: string(pu.pod.UID)},
			StartTime:  start,
			Containers: []statsv1alpha1.ContainerStats{{Name: pu.pod.Spec.Containers[0].Name, StartTime: start, CPU: cpu, Memory: memory}},
			CPU:        cpu,
			Memory:     memory,
		})
	}
	return summary, nil
}

func counter(name, help string, metrics ...*dto.Metric) *dto.MetricFamily {
	return &dto.MetricFamily{Name: proto.String(name), Help: proto.String(help), Type: dto.MetricType_COUNTER.Enum(), Metric: metrics}
}

func gauge(name, help string, metrics ...*dto.Metric) *dto.MetricFamily {
	return &dto.MetricFamily{Name: proto.String(name), Help: proto.String(help), Type: dto.MetricType_GAUGE.Enum(), Metric: metrics}
}

func sample(value float64, at time.Time, isCounter bool, labels ...string) *dto.Metric {
	m := &dto.Metric{TimestampMs: proto.Int64(at.UnixMilli())}
	if isCounter {
		m.Counter = &dto.Counter{Value: proto.Float64(value)}
	} else {
		m.Gauge = &dto.Gauge{Value: proto.Float64(value)}
	}
	for i := 0; i+1 < len(labels); i += 2 {
		m.Label = append(m.Label, &dto.LabelPair{Name: proto.String(labels[i]), Value: proto.String(labels[i+1])})
	}
	return m
}

// metricsResource is the endpoint metrics-server reads (`kubectl top` and the HPA): the kubelet "resource metrics" v1 families.
func (k *kubeletAPI) metricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	cpuNanos, mem, at, err := k.track().sampleNode(ctx)
	if err != nil {
		return []*dto.MetricFamily{gauge("resource_scrape_error", "1 if there was an error while getting container metrics, 0 otherwise", sample(1, time.Now(), false))}, nil
	}
	var containerCPU, containerMem, containerStart, podCPU, podMem []*dto.Metric
	for _, pu := range k.podUsages(ctx) {
		labels := []string{"container", pu.pod.Spec.Containers[0].Name, "namespace", pu.pod.Namespace, "pod", pu.pod.Name}
		podLabels := []string{"namespace", pu.pod.Namespace, "pod", pu.pod.Name}
		cpu, ws := float64(pu.usage.CPUUsageNanoSeconds)/1e9, float64(pu.usage.MemoryWorkingSetBytes)
		containerCPU = append(containerCPU, sample(cpu, pu.usage.At, true, labels...))
		containerMem = append(containerMem, sample(ws, pu.usage.At, false, labels...))
		containerStart = append(containerStart, sample(float64(pu.usage.StartedAt.UnixNano())/1e9, pu.usage.At, false, labels...))
		podCPU = append(podCPU, sample(cpu, pu.usage.At, true, podLabels...))
		podMem = append(podMem, sample(ws, pu.usage.At, false, podLabels...))
	}
	return []*dto.MetricFamily{
		counter("node_cpu_usage_seconds_total", "Cumulative cpu time consumed by the node in core-seconds", sample(float64(cpuNanos)/1e9, at, true)),
		gauge("node_memory_working_set_bytes", "Current working set of the node in bytes", sample(float64(mem), at, false)),
		counter("container_cpu_usage_seconds_total", "Cumulative cpu time consumed by the container in core-seconds", containerCPU...),
		gauge("container_memory_working_set_bytes", "Current working set of the container in bytes", containerMem...),
		gauge("container_start_time_seconds", "Start time of the container since unix epoch in seconds", containerStart...),
		counter("pod_cpu_usage_seconds_total", "Cumulative cpu time consumed by the pod in core-seconds", podCPU...),
		gauge("pod_memory_working_set_bytes", "Current working set of the pod in bytes", podMem...),
		gauge("resource_scrape_error", "1 if there was an error while getting container metrics, 0 otherwise", sample(0, at, false)),
	}, nil
}

func (k *kubeletAPI) stateDir() string {
	if k.stateDirPath != "" {
		return k.stateDirPath
	}
	return "/"
}
