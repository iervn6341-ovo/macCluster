package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	runtime "maccluster/internal/runtime"
	"maccluster/internal/servicenet"
)

type serviceNetwork struct {
	ip         string
	enabled    bool
	forwarding *servicenet.Forwarder
}

func (n *serviceNetwork) publish(ctx context.Context, c kubernetes.Interface, p *core.Pod, i runtime.Instance, s *core.PodStatus, wanted map[string]bool) error {
	s.HostIP = n.ip
	ports := map[string]int{}
	if p.Labels[servicenet.NetworkLabel] == "service" {
		if !n.enabled || n.ip == "" {
			return fmt.Errorf("Service networking is not enabled for this node")
		}
		if len(p.Spec.Containers) != 1 {
			return fmt.Errorf("Service networking requires one container")
		}
		ready := false
		for _, v := range s.Conditions {
			if v.Type == core.PodReady {
				ready = v.Status == core.ConditionTrue
			}
		}
		if ready {
			for _, cp := range p.Spec.Containers[0].Ports {
				if cp.Protocol != "" && cp.Protocol != core.ProtocolTCP {
					return fmt.Errorf("only TCP forwarding is supported")
				}
				target := i.Port
				if len(i.Ports) > 0 {
					target = int(cp.ContainerPort)
					if !containsPort(i.Ports, target) {
						return fmt.Errorf("undeclared native fixed port")
					}
				}
				key := string(p.UID) + "/" + strconv.Itoa(int(cp.ContainerPort))
				exposed, err := n.forwarding.Ensure(key, n.ip, target)
				if err != nil {
					return err
				}
				wanted[key] = true
				ports[strconv.Itoa(int(cp.ContainerPort))] = exposed
				if cp.Name != "" {
					ports[cp.Name] = exposed
				}
			}
		}
	}
	// Publish the mapping in the Pod's status, which is what a node may write. Pods that are not Service-networked
	// carry nothing.
	if p.Labels[servicenet.NetworkLabel] == "service" {
		status := core.ConditionFalse
		msg := ""
		reason := "NotReady"
		if len(ports) > 0 {
			b, _ := json.Marshal(ports)
			status, msg, reason = core.ConditionTrue, string(b), "Published"
		}
		cond := core.PodCondition{Type: core.PodConditionType(servicenet.PortsCondition), Status: status, Reason: reason, Message: msg, LastTransitionTime: meta.Now()}
		for _, old := range p.Status.Conditions {
			if old.Type == cond.Type && old.Status == cond.Status {
				cond.LastTransitionTime = old.LastTransitionTime
			}
		}
		s.Conditions = append(s.Conditions, cond)
	}
	return nil
}
