package cluster

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ManagementConfig binds privileged credentials to the already joined trust
// domain before making any authenticated request.
func ManagementConfig(ctx context.Context, nodeCfg *rest.Config, path string) (*rest.Config, error) {
	if nodeCfg == nil || !filepath.IsAbs(path) {
		return nil, errors.New("management kubeconfig must be an absolute path for a joined cluster")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, errors.New("cannot load management kubeconfig")
	}
	if cfg.Insecure || cfg.ExecProvider != nil || cfg.AuthProvider != nil {
		return nil, errors.New("management kubeconfig cannot use insecure TLS or executable authentication")
	}
	endpoint := func(s string) string {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		return strings.TrimRight(u.String(), "/")
	}
	if endpoint(cfg.Host) == "" || endpoint(cfg.Host) != endpoint(nodeCfg.Host) || cfg.ServerName != nodeCfg.ServerName {
		return nil, errors.New("management kubeconfig API endpoint does not match joined cluster")
	}
	pool := func(c *rest.Config) (*x509.CertPool, error) {
		data := c.CAData
		if len(data) == 0 && c.CAFile != "" {
			var err error
			data, err = os.ReadFile(c.CAFile)
			if err != nil {
				return nil, err
			}
		}
		p := x509.NewCertPool()
		if len(data) == 0 || !p.AppendCertsFromPEM(data) {
			return nil, errors.New("missing or invalid CA")
		}
		return p, nil
	}
	a, err := pool(cfg)
	if err != nil {
		return nil, errors.New("management kubeconfig CA is invalid")
	}
	b, err := pool(nodeCfg)
	if err != nil || !a.Equal(b) {
		return nil, errors.New("management kubeconfig CA trust does not match joined cluster")
	}
	if cfg.Proxy == nil {
		cfg.Proxy = nodeCfg.Proxy
	}
	cfg.Timeout = 15 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.New("cannot initialize management connection")
	}
	if err = client.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Error(); err != nil {
		return nil, errors.New("management API connection verification failed")
	}
	return cfg, nil
}

func controllerOwner(refs []metav1.OwnerReference) (*metav1.OwnerReference, error) {
	var found *metav1.OwnerReference
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			if found != nil {
				return nil, errors.New("ambiguous controller ownership")
			}
			r := refs[i]
			found = &r
		}
	}
	return found, nil
}
func deletionOptions(uid types.UID) metav1.DeleteOptions {
	foreground := metav1.DeletePropagationForeground
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &foreground}
}

// DeleteAssigned verifies assignment independently of the caller's status cache.
// Every destructive request carries the UID of the object actually inspected.
func DeleteAssigned(ctx context.Context, cfg *rest.Config, nodeName, uid, mode string) error {
	if nodeName == "" || uid == "" || (mode != "pod" && mode != "workload") {
		return errors.New("node, UID and valid deletion mode are required")
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return fmt.Errorf("list assigned Pods: %w", err)
	}
	for _, pod := range pods.Items {
		if string(pod.UID) != uid || pod.Spec.NodeName != nodeName {
			continue
		}
		owner, err := controllerOwner(pod.OwnerReferences)
		if err != nil {
			return err
		}
		if mode == "pod" || owner == nil {
			return client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, deletionOptions(pod.UID))
		}
		dc, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return err
		}
		for {
			var resource schema.GroupVersionResource
			switch owner.APIVersion + "/" + owner.Kind {
			case "apps/v1/ReplicaSet":
				resource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
			case "apps/v1/Deployment":
				resource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
			case "apps/v1/StatefulSet":
				resource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
			case "apps/v1/DaemonSet":
				resource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
			case "batch/v1/Job":
				resource = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
			default:
				return fmt.Errorf("unsupported workload controller %s", owner.Kind)
			}
			target := dc.Resource(resource).Namespace(pod.Namespace)
			obj, err := target.Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("verify workload owner: %w", err)
			}
			if owner.UID == "" || obj.GetUID() != owner.UID {
				return errors.New("workload controller UID no longer matches Pod ownership")
			}
			if owner.Kind == "ReplicaSet" {
				parent, err := controllerOwner(obj.GetOwnerReferences())
				if err != nil {
					return err
				}
				if parent != nil {
					if parent.Kind != "Deployment" || parent.APIVersion != "apps/v1" {
						return errors.New("unsupported ReplicaSet controller")
					}
					owner = parent
					continue
				}
			}
			return target.Delete(ctx, obj.GetName(), deletionOptions(obj.GetUID()))
		}
	}
	return errors.New("Pod UID is not assigned to this Mac")
}

// LeaveNode removes only the node registered by this installation.
func LeaveNode(ctx context.Context, cfg *rest.Config, nodeName, installationID string) error {
	if nodeName == "" || installationID == "" {
		return errors.New("node name and installation ID are required")
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if node.UID == "" || node.Annotations["maccluster.example.com/installation-id"] != installationID {
		return errors.New("refusing to remove Node owned by another installation")
	}
	if !node.Spec.Unschedulable {
		node.Spec.Unschedulable = true
		if _, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("cordon owned Node: %w", err)
		}
	}
	err = client.CoreV1().Nodes().Delete(ctx, nodeName, deletionOptions(node.UID))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
