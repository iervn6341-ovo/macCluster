package servicenet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"time"

	coordination "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Controller is designed to run in the Linux cluster, independently of Mac
// liveness. An explicit namespace allowlist and selectorless opt-in Services
// keep ordinary Services and EndpointSlices outside its ownership.
type Controller struct {
	Client     kubernetes.Interface
	Namespaces []string
	Now        func() time.Time
}

func (c *Controller) Run(ctx context.Context) error {
	if len(c.Namespaces) == 0 {
		return fmt.Errorf("at least one namespace is required")
	}
	for {
		if err := c.Reconcile(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
func (c *Controller) Reconcile(ctx context.Context) error {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	nodes, err := c.Client.CoreV1().Nodes().List(ctx, meta.ListOptions{})
	if err != nil {
		return err
	}
	leases, err := c.Client.CoordinationV1().Leases("kube-node-lease").List(ctx, meta.ListOptions{})
	if err != nil {
		return err
	}
	healthy := map[string]string{}
	leaseMap := map[string]*coordination.Lease{}
	for i := range leases.Items {
		l := &leases.Items[i]
		leaseMap[l.Name] = l
	}
	for _, n := range nodes.Items {
		if ip, ok := healthyNode(n, leaseMap[n.Name], now); ok {
			healthy[n.Name] = ip
		}
	}
	var failures []error
	for _, ns := range c.Namespaces {
		if ns == "" {
			return fmt.Errorf("empty namespace is not allowed")
		}
		services, err := c.Client.CoreV1().Services(ns).List(ctx, meta.ListOptions{LabelSelector: NetworkLabel + "=service"})
		if err != nil {
			return err
		}
		pods, err := c.Client.CoreV1().Pods(ns).List(ctx, meta.ListOptions{})
		if err != nil {
			return err
		}
		for i := range services.Items {
			s := &services.Items[i]
			want, err := desiredSlices(s, pods.Items, healthy)
			if err != nil {
				failures = append(failures, fmt.Errorf("service %s/%s: %w", ns, s.Name, err))
				want = map[string]*discovery.EndpointSlice{}
			}
			if err = c.sync(ctx, s, want); err != nil {
				failures = append(failures, err)
			}
		}
		// Also withdraw our slices when a Service loses its opt-in label.
		slices, err := c.Client.DiscoveryV1().EndpointSlices(ns).List(ctx, meta.ListOptions{LabelSelector: discovery.LabelManagedBy + "=" + ManagedBy})
		if err != nil {
			return err
		}
		active := map[types.UID]bool{}
		for _, s := range services.Items {
			active[s.UID] = true
		}
		for _, slice := range slices.Items {
			for _, o := range slice.OwnerReferences {
				if o.Kind == "Service" && !active[o.UID] {
					if err = c.remove(ctx, &slice); err != nil {
						return err
					}
				}
			}
		}
	}
	return errors.Join(failures...)
}
func healthyNode(n core.Node, l *coordination.Lease, now time.Time) (string, bool) {
	if n.Status.NodeInfo.OperatingSystem != "darwin" || n.Annotations["maccluster.example.com/installation-id"] == "" || n.DeletionTimestamp != nil {
		return "", false
	}
	ready := false
	for _, v := range n.Status.Conditions {
		if v.Type == core.NodeReady {
			ready = v.Status == core.ConditionTrue
		}
	}
	if !ready || l == nil || l.Spec.RenewTime == nil || l.Spec.LeaseDurationSeconds == nil {
		return "", false
	}
	age := now.Sub(l.Spec.RenewTime.Time)
	ttl := time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	if ttl <= 0 || ttl > 60*time.Second || age < -5*time.Second || age > ttl {
		return "", false
	}
	for _, a := range n.Status.Addresses {
		if a.Type == core.NodeInternalIP {
			ip := net.ParseIP(a.Address)
			if ip != nil && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLoopback() {
				return a.Address, true
			}
		}
	}
	return "", false
}
func desiredSlices(s *core.Service, pods []core.Pod, healthy map[string]string) (map[string]*discovery.EndpointSlice, error) {
	result := map[string]*discovery.EndpointSlice{}
	if len(s.Spec.Selector) > 0 || s.Spec.Type == core.ServiceTypeExternalName {
		return result, fmt.Errorf("requires a selectorless non-ExternalName Service")
	}
	selector := map[string]string{}
	if err := json.Unmarshal([]byte(s.Annotations[SelectorAnnotation]), &selector); err != nil || len(selector) == 0 {
		return result, fmt.Errorf("non-empty service-selector JSON annotation required")
	}
	for _, p := range pods {
		if p.Namespace != s.Namespace || p.DeletionTimestamp != nil || p.Labels[NetworkLabel] != "service" || p.Labels["maccluster.example.com/managed"] != "true" || !labels.SelectorFromSet(selector).Matches(labels.Set(p.Labels)) {
			continue
		}
		ip := healthy[p.Spec.NodeName]
		if ip == "" || p.Status.HostIP != ip || p.Status.Phase != core.PodRunning {
			continue
		}
		ready := false
		for _, cond := range p.Status.Conditions {
			if cond.Type == core.PodReady {
				ready = cond.Status == core.ConditionTrue
			}
		}
		if !ready {
			continue
		}
		ports, published := PublishedPorts(&p)
		if !published {
			continue
		}
		for _, sp := range s.Spec.Ports {
			protocol := sp.Protocol
			if protocol == "" {
				protocol = core.ProtocolTCP
			}
			if protocol != core.ProtocolTCP {
				return result, fmt.Errorf("only TCP Services are supported")
			}
			target := sp.TargetPort.String()
			if target == "0" || target == "" {
				target = strconv.Itoa(int(sp.Port))
			}
			port := ports[target]
			if port < 1 || port > 65535 {
				continue
			}
			sum := sha256.Sum256([]byte(string(s.UID) + "/" + string(p.UID) + "/" + sp.Name + "/" + strconv.Itoa(int(sp.Port))))
			name := "maccluster-" + hex.EncodeToString(sum[:12])
			portName := sp.Name
			isReady := true
			kind := discovery.AddressTypeIPv4
			if isIPv6(ip) {
				kind = discovery.AddressTypeIPv6
			}
			controller := true
			result[name] = &discovery.EndpointSlice{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: map[string]string{discovery.LabelServiceName: s.Name, discovery.LabelManagedBy: ManagedBy}, OwnerReferences: []meta.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: s.Name, UID: s.UID, Controller: &controller}}}, AddressType: kind, Ports: []discovery.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &port}}, Endpoints: []discovery.Endpoint{{Addresses: []string{ip}, Conditions: discovery.EndpointConditions{Ready: &isReady}, NodeName: &p.Spec.NodeName, TargetRef: &core.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID}}}}
		}
	}
	return result, nil
}
func (c *Controller) sync(ctx context.Context, s *core.Service, want map[string]*discovery.EndpointSlice) error {
	api := c.Client.DiscoveryV1().EndpointSlices(s.Namespace)
	existing, err := api.List(ctx, meta.ListOptions{LabelSelector: discovery.LabelServiceName + "=" + s.Name})
	if err != nil {
		return err
	}
	for _, slice := range existing.Items {
		if slice.Labels[discovery.LabelManagedBy] != ManagedBy || !ownedBy(slice, s) {
			return fmt.Errorf("refusing Service %s/%s with foreign EndpointSlices", s.Namespace, s.Name)
		}
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := want[k]
		old, err := api.Get(ctx, k, meta.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = api.Create(ctx, v, meta.CreateOptions{})
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if old.Labels[discovery.LabelManagedBy] != ManagedBy || !reflect.DeepEqual(old.OwnerReferences, v.OwnerReferences) {
			return fmt.Errorf("endpoint ownership mismatch: %s", k)
		}
		if reflect.DeepEqual(old.Ports, v.Ports) && reflect.DeepEqual(old.Endpoints, v.Endpoints) && old.AddressType == v.AddressType {
			continue
		}
		v.ResourceVersion = old.ResourceVersion
		if _, err = api.Update(ctx, v, meta.UpdateOptions{}); err != nil {
			return err
		}
	}
	for _, old := range existing.Items {
		if want[old.Name] == nil {
			if err = c.remove(ctx, &old); err != nil {
				return err
			}
		}
	}
	return nil
}
func (c *Controller) remove(ctx context.Context, s *discovery.EndpointSlice) error {
	err := c.Client.DiscoveryV1().EndpointSlices(s.Namespace).Delete(ctx, s.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &s.UID, ResourceVersion: &s.ResourceVersion}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func isIPv6(ip string) bool { parsed := net.ParseIP(ip); return parsed != nil && parsed.To4() == nil }

func ownedBy(slice discovery.EndpointSlice, s *core.Service) bool {
	for _, owner := range slice.OwnerReferences {
		if owner.Kind == "Service" && owner.APIVersion == "v1" && owner.UID == s.UID && owner.Name == s.Name {
			return true
		}
	}
	return false
}
