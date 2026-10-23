package servicenet

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// ManifestOptions describe one installation of the Service controller in a cluster.
type ManifestOptions struct {
	Namespace         string   // where the controller runs (for example maccluster-system)
	ManagedNamespaces []string // the namespaces whose Services it may manage: each gets its own RoleBinding
	Image             string   // pinned by digest
	AllowTag          bool     // accept an image without a digest
	Replicas          int32
	HealthPort        int32
}

const (
	controllerName    = "maccluster-service-controller"
	nodesRole         = controllerName + "-nodes"
	namespacedRole    = controllerName + "-namespaced"
	nodeLeaseRole     = controllerName + "-node-leases"
	electionRole      = controllerName + "-election"
	nodeLeaseNS       = "kube-node-lease"
	defaultHealthPort = 8081
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Namespaces the controller must never be given rights in: the cluster's own.
var protectedNamespaces = map[string]bool{"kube-system": true, "kube-public": true, "kube-node-lease": true, "default": false}

func (o *ManifestOptions) validate() error {
	if !dnsLabel.MatchString(o.Namespace) || len(o.Namespace) > 63 {
		return fmt.Errorf("namespace %q is not a valid namespace name", o.Namespace)
	}
	if protectedNamespaces[o.Namespace] {
		return fmt.Errorf("the controller must not run in %s", o.Namespace)
	}
	if len(o.ManagedNamespaces) == 0 {
		return fmt.Errorf("at least one managed namespace is required")
	}
	seen := map[string]bool{}
	for _, ns := range o.ManagedNamespaces {
		if !dnsLabel.MatchString(ns) || len(ns) > 63 {
			return fmt.Errorf("managed namespace %q is not a valid namespace name", ns)
		}
		if protectedNamespaces[ns] {
			return fmt.Errorf("the controller must not be given rights in %s", ns)
		}
		if seen[ns] {
			return fmt.Errorf("managed namespace %q is listed twice", ns)
		}
		seen[ns] = true
	}
	if o.Image == "" || strings.ContainsAny(o.Image, " \t\n") {
		return fmt.Errorf("an image is required")
	}
	if !strings.Contains(o.Image, "@sha256:") && !o.AllowTag {
		return fmt.Errorf("the image %q is not pinned by digest (name@sha256:...); a tag can be moved, pass the digest or allow a tag explicitly", o.Image)
	}
	if o.Replicas < 1 || o.Replicas > 5 {
		return fmt.Errorf("replicas must be between 1 and 5")
	}
	return nil
}

func objectLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": controllerName, "app.kubernetes.io/part-of": "maccluster"}
}

func subject(ns string) []rbacv1.Subject {
	return []rbacv1.Subject{{Kind: "ServiceAccount", Name: controllerName, Namespace: ns}}
}

func roleRef(kind, name string) rbacv1.RoleRef {
	return rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: kind, Name: name}
}

// Manifests returns the objects of an installation, in the order they should be applied: the namespace and the
// ServiceAccount, the rights (every one as narrow as the controller's work allows), then the Deployment and its budget.
func Manifests(o ManifestOptions) ([]any, error) {
	if o.HealthPort == 0 {
		o.HealthPort = defaultHealthPort
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	managed := append([]string(nil), o.ManagedNamespaces...)
	sort.Strings(managed)
	typ := func(kind, api string) meta.TypeMeta { return meta.TypeMeta{Kind: kind, APIVersion: api} }
	var out []any
	out = append(out,
		&core.Namespace{TypeMeta: typ("Namespace", "v1"), ObjectMeta: meta.ObjectMeta{Name: o.Namespace, Labels: objectLabels()}},
		&core.ServiceAccount{TypeMeta: typ("ServiceAccount", "v1"), ObjectMeta: meta.ObjectMeta{Name: controllerName, Namespace: o.Namespace, Labels: objectLabels()}},
		// Read the Mac nodes (address, readiness).
		&rbacv1.ClusterRole{TypeMeta: typ("ClusterRole", "rbac.authorization.k8s.io/v1"), ObjectMeta: meta.ObjectMeta{Name: nodesRole, Labels: objectLabels()},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}}}},
		&rbacv1.ClusterRoleBinding{TypeMeta: typ("ClusterRoleBinding", "rbac.authorization.k8s.io/v1"), ObjectMeta: meta.ObjectMeta{Name: nodesRole, Labels: objectLabels()},
			Subjects: subject(o.Namespace), RoleRef: roleRef("ClusterRole", nodesRole)},
		// Read the nodes' liveness leases.
		&rbacv1.Role{TypeMeta: typ("Role", "rbac.authorization.k8s.io/v1"), ObjectMeta: meta.ObjectMeta{Name: nodeLeaseRole, Namespace: nodeLeaseNS, Labels: objectLabels()},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch"}}}},
		&rbacv1.RoleBinding{TypeMeta: typ("RoleBinding", "rbac.authorization.k8s.io/v1"), ObjectMeta: meta.ObjectMeta{Name: nodeLeaseRole, Namespace: nodeLeaseNS, Labels: objectLabels()},
			Subjects: subject(o.Namespace), RoleRef: roleRef("Role", nodeLeaseRole)},
		// Its own leader-election Lease.
		&rbacv1.Role{TypeMeta: typ("Role", "rbac.authorization.k8s.io/v1"), ObjectMeta: meta.ObjectMeta{Name: electionRole, Namespace: o.Namespace, Labels: objectLabels()},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update"}}}},
		&rbacv1.RoleBinding{TypeMeta: typ("RoleBinding", "rbac.authorization.k8s.io/v1"), ObjectMeta: meta.ObjectMeta{Name: electionRole, Namespace: o.Namespace, Labels: objectLabels()},
			Subjects: subject(o.Namespace), RoleRef: roleRef("Role", electionRole)},
		// What it manages: one ClusterRole, bound with a RoleBinding in each managed namespace and nowhere else.
		&rbacv1.ClusterRole{TypeMeta: typ("ClusterRole", "rbac.authorization.k8s.io/v1"), ObjectMeta: meta.ObjectMeta{Name: namespacedRole, Labels: objectLabels()},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"services", "pods"}, Verbs: []string{"get", "list", "watch"}},
				{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			}},
	)
	for _, ns := range managed {
		out = append(out, &rbacv1.RoleBinding{TypeMeta: typ("RoleBinding", "rbac.authorization.k8s.io/v1"), ObjectMeta: meta.ObjectMeta{Name: namespacedRole, Namespace: ns, Labels: objectLabels()},
			Subjects: subject(o.Namespace), RoleRef: roleRef("ClusterRole", namespacedRole)})
	}
	one, zero := intstr.FromInt32(1), intstr.FromInt32(0)
	yes, no := true, false
	minAvailable := intstr.FromInt32(1)
	healthPort := intstr.FromInt32(o.HealthPort)
	probe := func(initial int32) *core.Probe {
		return &core.Probe{ProbeHandler: core.ProbeHandler{HTTPGet: &core.HTTPGetAction{Path: "/healthz", Port: healthPort}}, InitialDelaySeconds: initial, PeriodSeconds: 10, FailureThreshold: 3, TimeoutSeconds: 2}
	}
	replicas := o.Replicas
	out = append(out, &appsv1.Deployment{TypeMeta: typ("Deployment", "apps/v1"), ObjectMeta: meta.ObjectMeta{Name: controllerName, Namespace: o.Namespace, Labels: objectLabels()},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &meta.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": controllerName}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &zero, MaxSurge: &one}},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: objectLabels()},
				Spec: core.PodSpec{
					ServiceAccountName:           controllerName,
					AutomountServiceAccountToken: &yes, // the controller talks to the API
					NodeSelector:                 map[string]string{"kubernetes.io/os": "linux"},
					// Spread over nodes so one node's failure does not take every replica.
					TopologySpreadConstraints: []core.TopologySpreadConstraint{{MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: core.ScheduleAnyway,
						LabelSelector: &meta.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": controllerName}}}},
					Containers: []core.Container{{
						Name:  "controller",
						Image: o.Image,
						Args:  []string{"--namespaces", strings.Join(managed, ","), "--health-addr", fmt.Sprintf(":%d", o.HealthPort)},
						Env: []core.EnvVar{
							{Name: "POD_NAME", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.name"}}},
							{Name: "POD_NAMESPACE", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
						},
						Ports:          []core.ContainerPort{{Name: "health", ContainerPort: o.HealthPort}},
						LivenessProbe:  probe(10),
						ReadinessProbe: probe(2),
						Resources: core.ResourceRequirements{
							Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("10m"), core.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   core.ResourceList{core.ResourceMemory: resource.MustParse("256Mi")},
						},
						SecurityContext: &core.SecurityContext{RunAsNonRoot: &yes, ReadOnlyRootFilesystem: &yes, AllowPrivilegeEscalation: &no,
							Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}, SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}},
					}},
				},
			},
		}})
	if o.Replicas > 1 {
		out = append(out, &policyv1.PodDisruptionBudget{TypeMeta: typ("PodDisruptionBudget", "policy/v1"), ObjectMeta: meta.ObjectMeta{Name: controllerName, Namespace: o.Namespace, Labels: objectLabels()},
			Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvailable, Selector: &meta.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": controllerName}}}})
	}
	return out, nil
}

// ManifestsYAML renders Manifests as one multi-document YAML stream.
func ManifestsYAML(o ManifestOptions) (string, error) {
	objs, err := Manifests(o)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Generated by `maccluster-agent network-controller-manifests`. Apply with kubectl apply -f.\n")
	for _, obj := range objs {
		doc, err := yaml.Marshal(obj)
		if err != nil {
			return "", err
		}
		b.WriteString("---\n")
		// The generated objects carry an empty status and creationTimestamp: drop them.
		var out []string
		for _, line := range strings.Split(strings.TrimRight(string(doc), "\n"), "\n") {
			if line == "status: {}" || line == "status:" || strings.TrimSpace(line) == "creationTimestamp: null" {
				continue
			}
			out = append(out, line)
		}
		b.WriteString(strings.Join(out, "\n") + "\n")
	}
	return b.String(), nil
}
