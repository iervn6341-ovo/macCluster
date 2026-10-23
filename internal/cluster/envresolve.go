package cluster

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ReasonConfigError is what kubelet reports when a container's configuration cannot be built, for example a
// Secret that does not exist yet. The Pod stays Pending and is retried on every reconcile.
const ReasonConfigError = "CreateContainerConfigError"

const NoteInvalidEnvNames = "InvalidEnvironmentVariableNames"

// EnvSource reads the objects an env/envFrom reference points at. The agent's implementation asks the API
// server; the Node authorizer only lets a node read what Pods bound to it reference.
type EnvSource interface {
	Secret(ctx context.Context, namespace, name string) (*corev1.Secret, error)
	ConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error)
}

// TranslateOptions are what a Pod needs from outside itself to be turned into a workload.
type TranslateOptions struct {
	Ctx      context.Context
	Source   EnvSource
	NodeName string // for fieldRef spec.nodeName (the Pod's own field is used when set)
	HostIP   string // for fieldRef status.hostIP
	// EgressProxy is the proxy URL (with credentials) that opted-in Pods use to reach the cluster network.
	EgressProxy string
	// Volumes configures Pod volumes (Docker). Plans, when set, receives what has to be written for them.
	Volumes volumeSettings
	Plans   *[]*volumePlan
	// APIServer is the API server's address (host:port) that a Pod with a mounted ServiceAccount token should use
	// (KUBERNETES_SERVICE_HOST and _PORT). Empty or a loopback address: not known.
	APIServer string
}

var supportedFieldPaths = []string{"metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "metadata.labels['<key>']", "metadata.annotations['<key>']"}

func fieldPathSupported(path string) bool {
	switch path {
	case "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP":
		return true
	}
	_, _, ok := mapFieldKey(path)
	return ok
}

// mapFieldKey parses metadata.labels['key'] and metadata.annotations['key'].
func mapFieldKey(path string) (kind, key string, ok bool) {
	for _, k := range []string{"labels", "annotations"} {
		prefix := "metadata." + k + "['"
		if strings.HasPrefix(path, prefix) && strings.HasSuffix(path, "']") && len(path) > len(prefix)+2 {
			return k, path[len(prefix) : len(path)-2], true
		}
	}
	return "", "", false
}

func resourceSupported(r *corev1.ResourceFieldSelector, container string) (string, bool) {
	if r.ContainerName != "" && r.ContainerName != container {
		return fmt.Sprintf("resourceFieldRef for container %q (only this Pod's one container)", r.ContainerName), false
	}
	switch r.Resource {
	case "limits.cpu", "limits.memory", "requests.cpu", "requests.memory":
		return "", true
	}
	return fmt.Sprintf("resourceFieldRef %s (supported: limits.cpu, limits.memory, requests.cpu, requests.memory)", r.Resource), false
}

// checkEnvSupport names the env sources this node cannot provide, so they are reported with everything else.
func checkEnvSupport(c corev1.Container, add func(string, ...any)) {
	for _, env := range c.Env {
		v := env.ValueFrom
		if v == nil {
			continue
		}
		switch {
		case v.FieldRef != nil:
			path := v.FieldRef.FieldPath
			switch {
			case path == "status.podIP" || path == "status.podIPs":
				add("env %s: fieldRef %s is not available: Mac Pods have no Pod IP", env.Name, path)
			case !fieldPathSupported(path):
				add("env %s: fieldRef %s is not supported (supported: %s)", env.Name, path, strings.Join(supportedFieldPaths, ", "))
			}
		case v.ResourceFieldRef != nil:
			if why, ok := resourceSupported(v.ResourceFieldRef, c.Name); !ok {
				add("env %s: %s", env.Name, why)
			}
		case v.SecretKeyRef != nil || v.ConfigMapKeyRef != nil:
		default:
			add("env %s: valueFrom has no source", env.Name)
		}
	}
	for i, from := range c.EnvFrom {
		if from.ConfigMapRef == nil && from.SecretRef == nil {
			add("envFrom[%d]: no configMapRef or secretRef", i)
		}
	}
}

// expandRefs replaces $(NAME) with a defined variable's value, turns $$ into $ and leaves anything it does not
// know (such as $(MACCLUSTER_PORT), which the runtime fills in at start) as it was, as kubelet does.
func expandRefs(in string, lookup func(name string) (string, bool)) string {
	if !strings.Contains(in, "$") {
		return in
	}
	var out strings.Builder
	for i := 0; i < len(in); i++ {
		if in[i] != '$' || i+1 >= len(in) {
			out.WriteByte(in[i])
			continue
		}
		switch in[i+1] {
		case '$':
			out.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(in[i+2:], ')')
			if end < 0 {
				out.WriteByte('$')
				continue
			}
			name := in[i+2 : i+2+end]
			if v, ok := lookup(name); ok {
				out.WriteString(v)
			} else {
				out.WriteString("$(" + name + ")")
			}
			i += 2 + end
		default:
			out.WriteByte('$')
		}
	}
	return out.String()
}

type envResult struct {
	literal  map[string]string // plain values from the Pod spec
	resolved map[string]string // values read from the cluster, or built from them
	command  []string
	args     []string
	notes    []Note
}

func configError(format string, args ...any) error {
	return &SpecError{Reason: ReasonConfigError, Fields: []string{fmt.Sprintf(format, args...)}}
}

// buildEnv assembles a container's environment the way kubelet does: envFrom first, then env in order (each value
// may use $(NAME) of what is defined before it), later entries winning; command and args expand against the result.
func buildEnv(o TranslateOptions, p *corev1.Pod, c corev1.Container) (envResult, error) {
	ctx := o.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res := envResult{literal: map[string]string{}, resolved: map[string]string{}}
	values := map[string]string{}
	derived := map[string]bool{}
	set := func(name, value string, fromCluster bool) {
		values[name] = value
		derived[name] = fromCluster
	}
	var skipped []string
	for _, from := range c.EnvFrom {
		var data map[string]string
		switch {
		case from.ConfigMapRef != nil:
			ref := from.ConfigMapRef
			cm, err := o.readConfigMap(ctx, p.Namespace, ref.Name)
			if err != nil {
				if apierrors.IsNotFound(err) && ref.Optional != nil && *ref.Optional {
					continue
				}
				return res, configError("configmap %q: %v", ref.Name, cleanErr(err))
			}
			data = cm.Data
		case from.SecretRef != nil:
			ref := from.SecretRef
			sec, err := o.readSecret(ctx, p.Namespace, ref.Name)
			if err != nil {
				if apierrors.IsNotFound(err) && ref.Optional != nil && *ref.Optional {
					continue
				}
				return res, configError("secret %q: %v", ref.Name, cleanErr(err))
			}
			data = map[string]string{}
			for k, v := range sec.Data {
				data[k] = string(v)
			}
		}
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			name := from.Prefix + k
			if len(validation.IsEnvVarName(name)) > 0 {
				skipped = append(skipped, name)
				continue
			}
			set(name, data[k], true)
		}
	}
	if len(skipped) > 0 {
		res.notes = append(res.notes, Note{corev1.EventTypeWarning, NoteInvalidEnvNames, "keys that are not valid environment variable names were skipped: " + strings.Join(skipped, ", ")})
	}
	lookup := func(usedDerived *bool) func(string) (string, bool) {
		return func(name string) (string, bool) {
			v, ok := values[name]
			if ok && derived[name] {
				*usedDerived = true
			}
			return v, ok
		}
	}
	for _, env := range c.Env {
		if env.ValueFrom == nil {
			used := false
			set(env.Name, expandRefs(env.Value, lookup(&used)), used)
			continue
		}
		v, err := valueFrom(ctx, o, p, c, env)
		if err != nil {
			return res, err
		}
		if v == nil {
			continue // optional and absent
		}
		set(env.Name, *v, true)
	}
	for name, v := range values {
		if derived[name] {
			res.resolved[name] = v
		} else {
			res.literal[name] = v
		}
	}
	unused := false
	mapping := lookup(&unused)
	for _, s := range c.Command {
		res.command = append(res.command, expandRefs(s, mapping))
	}
	for _, s := range c.Args {
		res.args = append(res.args, expandRefs(s, mapping))
	}
	return res, nil
}

func cleanErr(err error) string {
	if apierrors.IsNotFound(err) {
		return "not found"
	}
	return err.Error()
}

func (o TranslateOptions) readSecret(ctx context.Context, ns, name string) (*corev1.Secret, error) {
	if o.Source == nil {
		return nil, fmt.Errorf("this node cannot read Secrets right now (no cluster connection)")
	}
	return o.Source.Secret(ctx, ns, name)
}

func (o TranslateOptions) readConfigMap(ctx context.Context, ns, name string) (*corev1.ConfigMap, error) {
	if o.Source == nil {
		return nil, fmt.Errorf("this node cannot read ConfigMaps right now (no cluster connection)")
	}
	return o.Source.ConfigMap(ctx, ns, name)
}

func valueFrom(ctx context.Context, o TranslateOptions, p *corev1.Pod, c corev1.Container, env corev1.EnvVar) (*string, error) {
	v := env.ValueFrom
	switch {
	case v.FieldRef != nil:
		s, err := fieldValue(p, o, v.FieldRef.FieldPath)
		if err != nil {
			return nil, configError("env %s: %v", env.Name, err)
		}
		return &s, nil
	case v.ResourceFieldRef != nil:
		s, err := resourceValue(c, v.ResourceFieldRef)
		if err != nil {
			return nil, &SpecError{Reason: ReasonUnsupportedPodSpec, Fields: []string{fmt.Sprintf("env %s: %v", env.Name, err)}}
		}
		return &s, nil
	case v.SecretKeyRef != nil:
		ref := v.SecretKeyRef
		sec, err := o.readSecret(ctx, p.Namespace, ref.Name)
		if err != nil {
			if apierrors.IsNotFound(err) && ref.Optional != nil && *ref.Optional {
				return nil, nil
			}
			return nil, configError("secret %q: %v", ref.Name, cleanErr(err))
		}
		data, ok := sec.Data[ref.Key]
		if !ok {
			if ref.Optional != nil && *ref.Optional {
				return nil, nil
			}
			return nil, configError("couldn't find key %s in Secret %s/%s", ref.Key, p.Namespace, ref.Name)
		}
		s := string(data)
		return &s, nil
	case v.ConfigMapKeyRef != nil:
		ref := v.ConfigMapKeyRef
		cm, err := o.readConfigMap(ctx, p.Namespace, ref.Name)
		if err != nil {
			if apierrors.IsNotFound(err) && ref.Optional != nil && *ref.Optional {
				return nil, nil
			}
			return nil, configError("configmap %q: %v", ref.Name, cleanErr(err))
		}
		s, ok := cm.Data[ref.Key]
		if !ok {
			if ref.Optional != nil && *ref.Optional {
				return nil, nil
			}
			return nil, configError("couldn't find key %s in ConfigMap %s/%s", ref.Key, p.Namespace, ref.Name)
		}
		return &s, nil
	}
	return nil, configError("env %s: valueFrom has no source", env.Name)
}

func fieldValue(p *corev1.Pod, o TranslateOptions, path string) (string, error) {
	switch path {
	case "metadata.name":
		return p.Name, nil
	case "metadata.namespace":
		return p.Namespace, nil
	case "metadata.uid":
		return string(p.UID), nil
	case "spec.nodeName":
		if p.Spec.NodeName != "" {
			return p.Spec.NodeName, nil
		}
		return o.NodeName, nil
	case "spec.serviceAccountName":
		return p.Spec.ServiceAccountName, nil
	case "status.hostIP":
		if o.HostIP == "" {
			return "", fmt.Errorf("the host IP of this node is not known yet")
		}
		return o.HostIP, nil
	}
	if kind, key, ok := mapFieldKey(path); ok {
		m := p.Labels
		if kind == "annotations" {
			m = p.Annotations
		}
		return m[key], nil
	}
	return "", fmt.Errorf("fieldRef %s is not supported", path)
}

// resourceValue evaluates requests/limits for the container, rounded up to the divisor (cores for CPU, bytes for memory).
func resourceValue(c corev1.Container, r *corev1.ResourceFieldSelector) (string, error) {
	list := c.Resources.Requests
	name := corev1.ResourceCPU
	if strings.HasSuffix(r.Resource, "memory") {
		name = corev1.ResourceMemory
	}
	if strings.HasPrefix(r.Resource, "limits.") {
		list = c.Resources.Limits
	}
	q, ok := list[name]
	if !ok {
		if strings.HasPrefix(r.Resource, "limits.") {
			return "", fmt.Errorf("%s: no limit is set; this node does not default it to the node's allocatable resources", r.Resource)
		}
		return "0", nil
	}
	divisor := r.Divisor
	if divisor.IsZero() {
		divisor.Set(1)
	}
	if name == corev1.ResourceCPU {
		d := divisor.MilliValue()
		if d <= 0 {
			return "", fmt.Errorf("invalid divisor")
		}
		return fmt.Sprint(int64(math.Ceil(float64(q.MilliValue()) / float64(d)))), nil
	}
	d := divisor.Value()
	if d <= 0 {
		return "", fmt.Errorf("invalid divisor")
	}
	return fmt.Sprint(int64(math.Ceil(float64(q.Value()) / float64(d)))), nil
}
