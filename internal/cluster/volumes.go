package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	runtime "maccluster/internal/runtime"
)

// Volumes of Docker Pods (docs/archive/volumes-design.md). This layer writes the content, the engine only mounts the paths.
//
//	<state>/volumes/<Pod UID>/.maccluster-volume   ownership marker (the only thing cleanup trusts)
//	<state>/volumes/<Pod UID>/<volume>/            emptyDir, or the files of a configMap/secret/downwardAPI/projected volume
//	<state>/volumes/<Pod UID>/.<volume>.hash       what was last written (outside the volume, so the container never sees it)
//
// hostPath volumes are mounted straight from their allowed path; they are never written under the root and never
// removed by cleanup.

const (
	ReasonHostPathNotAllowed = "HostPathNotAllowed"
	ReasonFailedMount        = "FailedMount"

	volumeMarkerFile = ".maccluster-volume"
	volumeResync     = time.Minute // like kubelet: ConfigMap, Secret and downwardAPI content is re-read about once a minute

)

// orphanGrace is how long a Pod must have been missing before its volumes go (a variable so tests can shorten it).
var orphanGrace = 30 * time.Second

var podUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// volumeSettings is what the node options contribute to Pod volumes.
type volumeSettings struct {
	root             string   // <state>/volumes; empty disables Pod volumes
	allowedHostPaths []string // hostPath volumes are only allowed below these (resolved) paths
}

// nodeSettings is what the agent's options contribute to translating and running Pods.
type nodeSettings struct {
	observePods func([]PodObservation)
	egressProxy string
	apiServer   string // host:port of the API server for KUBERNETES_SERVICE_HOST/PORT
	volumes     volumeSettings
	cleaner     *volumeCleaner
}

type volFile struct {
	data []byte
	mode os.FileMode
}

// volumePlan is one volume the container mounts: where its content lives and how to produce it.
type volumePlan struct {
	name string
	dir  string // the directory on this Mac that holds the content ("" for tmpfs and hostPath)
	// build returns the files of a content volume; nil for emptyDir. It may keep what it needs between calls (an
	// issued token) in the volume state, and is told the current time.
	build func(ctx context.Context, c kubernetes.Interface, st *volumeState, now time.Time) (map[string]volFile, error)
}

// volumeState is what the agent remembers about a Pod's volumes between reconciles.
type volumeState struct {
	nextSync map[string]time.Time
	tokens   map[string]*issuedToken // ServiceAccount tokens issued for this Pod, by volume and file
}

// issuedToken is a ServiceAccount token this node asked the API server for on behalf of a Pod.
type issuedToken struct {
	token           string
	issued, expires time.Time
}

// refreshAt is when a token is replaced: after 80% of its lifetime, and in any case after 24 hours, like kubelet.
func (t *issuedToken) refreshAt() time.Time {
	at := t.issued.Add(time.Duration(float64(t.expires.Sub(t.issued)) * 0.8))
	if day := t.issued.Add(24 * time.Hour); day.Before(at) {
		return day
	}
	return at
}

// ---- planning

type volumeMountsResult struct {
	mounts []runtime.Mount
	plans  []*volumePlan
	notes  []Note
}

// tokenIsMounted says whether the ServiceAccount token is mounted into the Pod: only when it asks for it with an
// explicit automountServiceAccountToken: true (the API server's own injection when the field is unset does not count),
// and only for Docker Pods (a native program has no volumes). Otherwise the injected volume is ignored with a warning.
func tokenIsMounted(p *corev1.Pod, executor string) bool {
	return executor == "docker" && p.Spec.AutomountServiceAccountToken != nil && *p.Spec.AutomountServiceAccountToken
}

func volumeKindSupported(v corev1.Volume) bool {
	switch {
	case v.EmptyDir != nil, v.HostPath != nil, v.ConfigMap != nil, v.Secret != nil, v.DownwardAPI != nil, v.Projected != nil:
		return true
	}
	return false
}

// planVolumes resolves the container's volume mounts for a Docker Pod. Problems that are the Pod's own (an
// unsupported kind, a path that is not allowed) are returned as unsupported entries; the caller decides the reason.
func planVolumes(p *corev1.Pod, c corev1.Container, o TranslateOptions, add func(string, ...any)) (volumeMountsResult, error) {
	var res volumeMountsResult
	byName := map[string]corev1.Volume{}
	for _, v := range p.Spec.Volumes {
		byName[v.Name] = v
	}
	podDir := ""
	if o.Volumes.root != "" {
		podDir = filepath.Join(o.Volumes.root, string(p.UID))
	}
	planned := map[string]*volumePlan{}
	for _, m := range c.VolumeMounts {
		if strings.HasPrefix(m.Name, "kube-api-access-") && isServiceAccountVolume(byName[m.Name]) && !tokenIsMounted(p, "docker") {
			continue // the ServiceAccount token volume: not mounted unless the Pod asks for it; reported by the caller
		}
		v, ok := byName[m.Name]
		if !ok {
			add("volumeMounts: %q at %s refers to a volume the Pod does not define", m.Name, m.MountPath)
			continue
		}
		if !volumeKindSupported(v) {
			add("volumes: %q (%s) is not supported%s", v.Name, volumeKind(v), unsupportedVolumeHint(v))
			continue
		}
		if podDir == "" && v.HostPath == nil && !(v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory) {
			add("volumes: %q (%s) needs a node with a state directory for volumes", v.Name, volumeKind(v))
			continue
		}
		if m.MountPropagation != nil || m.SubPathExpr != "" {
			add("volumeMounts: %q uses subPathExpr or mountPropagation, which are not supported", m.Name)
			continue
		}
		mount := runtime.Mount{Name: m.Name, Container: filepath.Clean(m.MountPath), ReadOnly: m.ReadOnly}
		switch {
		case v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory:
			mount.Tmpfs, mount.ReadOnly = true, false
			if v.EmptyDir.SizeLimit != nil {
				mount.SizeBytes = v.EmptyDir.SizeLimit.Value()
			}
			if m.SubPath != "" {
				add("volumeMounts: %q: subPath of a memory volume is not supported", m.Name)
				continue
			}
		case v.HostPath != nil:
			host, err := checkHostPath(v.HostPath, o.Volumes.allowedHostPaths)
			if err != nil {
				return res, err
			}
			mount.HostPath = host
			if m.SubPath != "" {
				mount.HostPath = filepath.Join(host, filepath.Clean("/"+m.SubPath))
			}
		default:
			plan := planned[v.Name]
			if plan == nil {
				plan = &volumePlan{name: v.Name, dir: filepath.Join(podDir, v.Name)}
				if v.EmptyDir == nil {
					build, err := contentBuilder(p, c, v, o)
					if err != nil {
						add("volumes: %q: %v", v.Name, err)
						continue
					}
					plan.build = build
				} else if v.EmptyDir.SizeLimit != nil {
					res.notes = append(res.notes, Note{corev1.EventTypeNormal, NoteSizeLimitNotEnforced, fmt.Sprintf("emptyDir %q: sizeLimit is only enforced for medium Memory (a tmpfs); this one is a directory on the Mac", v.Name)})
				}
				planned[v.Name] = plan
				res.plans = append(res.plans, plan)
			}
			mount.HostPath = plan.dir
			if m.SubPath != "" {
				rel := filepath.Clean("/" + m.SubPath)
				mount.HostPath = filepath.Join(plan.dir, rel)
			}
		}
		res.mounts = append(res.mounts, mount)
	}
	sort.Slice(res.mounts, func(i, j int) bool { return res.mounts[i].Container < res.mounts[j].Container })
	return res, nil
}

func unsupportedVolumeHint(v corev1.Volume) string {
	switch {
	case v.PersistentVolumeClaim != nil:
		return " (a PersistentVolumeClaim needs a storage provisioner this node does not have; mount the storage on the Mac and use a hostPath below an allowed path instead)"
	case v.NFS != nil:
		return " (mount the NFS share on the Mac and use a hostPath below an allowed path instead)"
	}
	return ""
}

const NoteSizeLimitNotEnforced = "VolumeSizeLimitNotEnforced"

// checkHostPath applies the allow list: resolved symlinks, compared by whole path components, so /Volumes/models-evil
// is not below /Volumes/models. An empty list allows nothing.
func checkHostPath(h *corev1.HostPathVolumeSource, allowed []string) (string, error) {
	denied := func(why string) error {
		return &SpecError{Reason: ReasonHostPathNotAllowed, Fields: []string{fmt.Sprintf("hostPath %q: %s; allowed paths are set with allowedHostPaths in the node options (now: %s)", h.Path, why, listOrNone(allowed))}}
	}
	if !filepath.IsAbs(h.Path) {
		return "", denied("the path must be absolute")
	}
	path := filepath.Clean(h.Path)
	typ := corev1.HostPathUnset
	if h.Type != nil {
		typ = *h.Type
	}
	if len(allowed) == 0 {
		return "", denied("hostPath volumes are not allowed on this node")
	}
	// Resolve what exists; for a path that does not exist yet (DirectoryOrCreate) resolve its parent.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", denied(err.Error())
		}
		if typ != corev1.HostPathDirectoryOrCreate && typ != corev1.HostPathFileOrCreate {
			return "", &SpecError{Reason: ReasonFailedMount, Fields: []string{fmt.Sprintf("hostPath %q does not exist", h.Path)}}
		}
		parent, perr := filepath.EvalSymlinks(filepath.Dir(path))
		if perr != nil {
			return "", &SpecError{Reason: ReasonFailedMount, Fields: []string{fmt.Sprintf("hostPath %q: its parent directory does not exist", h.Path)}}
		}
		resolved = filepath.Join(parent, filepath.Base(path))
	}
	ok := false
	for _, a := range allowed {
		ra, aerr := filepath.EvalSymlinks(a)
		if aerr != nil {
			ra = filepath.Clean(a)
		}
		if resolved == ra || strings.HasPrefix(resolved, ra+string(filepath.Separator)) {
			ok = true
			break
		}
	}
	if !ok {
		return "", denied("it is not below an allowed path")
	}
	// The types kubelet checks.
	switch typ {
	case corev1.HostPathDirectoryOrCreate:
		if err := os.MkdirAll(resolved, 0o755); err != nil {
			return "", &SpecError{Reason: ReasonFailedMount, Fields: []string{fmt.Sprintf("hostPath %q: %v", h.Path, err)}}
		}
	case corev1.HostPathFileOrCreate:
		if _, serr := os.Stat(resolved); errors.Is(serr, os.ErrNotExist) {
			f, cerr := os.OpenFile(resolved, os.O_CREATE|os.O_WRONLY, 0o644)
			if cerr != nil {
				return "", &SpecError{Reason: ReasonFailedMount, Fields: []string{fmt.Sprintf("hostPath %q: %v", h.Path, cerr)}}
			}
			f.Close()
		}
	}
	info, serr := os.Stat(resolved)
	if serr != nil {
		return "", &SpecError{Reason: ReasonFailedMount, Fields: []string{fmt.Sprintf("hostPath %q: %v", h.Path, serr)}}
	}
	switch typ {
	case corev1.HostPathDirectory, corev1.HostPathDirectoryOrCreate:
		if !info.IsDir() {
			return "", &SpecError{Reason: ReasonFailedMount, Fields: []string{fmt.Sprintf("hostPath %q is not a directory", h.Path)}}
		}
	case corev1.HostPathFile, corev1.HostPathFileOrCreate:
		if info.IsDir() {
			return "", &SpecError{Reason: ReasonFailedMount, Fields: []string{fmt.Sprintf("hostPath %q is not a file", h.Path)}}
		}
	case corev1.HostPathUnset:
	default:
		return "", &SpecError{Reason: ReasonUnsupportedPodSpec, Fields: []string{fmt.Sprintf("hostPath %q: type %s is not supported", h.Path, typ)}}
	}
	return resolved, nil
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// ---- content volumes

func fileMode(m *int32, def int32) os.FileMode {
	if m != nil {
		return os.FileMode(*m) & 0o777
	}
	return os.FileMode(def) & 0o777
}

func validRelPath(p string) error {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "..") || strings.Contains(p, "/..") || strings.Contains(p, "../") || filepath.Clean(p) != p {
		return fmt.Errorf("invalid file path %q", p)
	}
	return nil
}

// contentBuilder returns what produces the files of a configMap, secret, downwardAPI or projected volume.
type volumeBuilder = func(context.Context, kubernetes.Interface, *volumeState, time.Time) (map[string]volFile, error)

func contentBuilder(p *corev1.Pod, c corev1.Container, v corev1.Volume, o TranslateOptions) (volumeBuilder, error) {
	switch {
	case v.ConfigMap != nil:
		src := v.ConfigMap
		return func(ctx context.Context, cl kubernetes.Interface, _ *volumeState, _ time.Time) (map[string]volFile, error) {
			return configMapFiles(ctx, cl, p.Namespace, src.Name, src.Items, src.DefaultMode, src.Optional)
		}, nil
	case v.Secret != nil:
		src := v.Secret
		return func(ctx context.Context, cl kubernetes.Interface, _ *volumeState, _ time.Time) (map[string]volFile, error) {
			return secretFiles(ctx, cl, p.Namespace, src.SecretName, src.Items, src.DefaultMode, src.Optional)
		}, nil
	case v.DownwardAPI != nil:
		items, mode := v.DownwardAPI.Items, v.DownwardAPI.DefaultMode
		return func(context.Context, kubernetes.Interface, *volumeState, time.Time) (map[string]volFile, error) {
			return downwardFiles(p, c, o, items, mode)
		}, nil
	case v.Projected != nil:
		src := v.Projected
		for _, s := range src.Sources {
			if s.ClusterTrustBundle != nil {
				return nil, fmt.Errorf("projected clusterTrustBundle sources are not supported")
			}
		}
		return func(ctx context.Context, cl kubernetes.Interface, st *volumeState, now time.Time) (map[string]volFile, error) {
			out := map[string]volFile{}
			for _, s := range src.Sources {
				var files map[string]volFile
				var err error
				switch {
				case s.ServiceAccountToken != nil:
					var f volFile
					f, err = serviceAccountTokenFile(ctx, cl, st, now, p, v.Name, s.ServiceAccountToken, src.DefaultMode)
					files = map[string]volFile{s.ServiceAccountToken.Path: f}
				case s.ConfigMap != nil:
					files, err = configMapFiles(ctx, cl, p.Namespace, s.ConfigMap.Name, s.ConfigMap.Items, src.DefaultMode, s.ConfigMap.Optional)
				case s.Secret != nil:
					files, err = secretFiles(ctx, cl, p.Namespace, s.Secret.Name, s.Secret.Items, src.DefaultMode, s.Secret.Optional)
				case s.DownwardAPI != nil:
					files, err = downwardFiles(p, c, o, s.DownwardAPI.Items, src.DefaultMode)
				}
				if err != nil {
					return nil, err
				}
				for k, f := range files {
					if _, dup := out[k]; dup {
						return nil, fmt.Errorf("conflicting path %q in a projected volume", k)
					}
					out[k] = f
				}
			}
			return out, nil
		}, nil
	}
	return nil, fmt.Errorf("unsupported volume")
}

func selectKeys(kind, name string, keys map[string][]byte, items []corev1.KeyToPath, defMode *int32, optional *bool) (map[string]volFile, error) {
	out := map[string]volFile{}
	if len(items) == 0 {
		for k, v := range keys {
			if err := validRelPath(k); err != nil || strings.Contains(k, "/") {
				continue // a key that is not a usable file name is skipped, as kubelet does
			}
			out[k] = volFile{v, fileMode(nil, defaultVolumeMode(defMode))}
		}
		return out, nil
	}
	for _, it := range items {
		if err := validRelPath(it.Path); err != nil {
			return nil, err
		}
		v, ok := keys[it.Key]
		if !ok {
			if optional != nil && *optional {
				continue
			}
			return nil, fmt.Errorf("the key %q is not in %s %q", it.Key, kind, name)
		}
		def := defaultVolumeMode(defMode)
		out[it.Path] = volFile{v, fileMode(it.Mode, def)}
	}
	return out, nil
}

func defaultVolumeMode(m *int32) int32 {
	if m != nil {
		return *m
	}
	return 0o644
}

func configMapFiles(ctx context.Context, cl kubernetes.Interface, ns, name string, items []corev1.KeyToPath, defMode *int32, optional *bool) (map[string]volFile, error) {
	cm, err := cl.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && optional != nil && *optional {
			return map[string]volFile{}, nil
		}
		return nil, fmt.Errorf("configmap %q: %s", name, cleanErr(err))
	}
	keys := map[string][]byte{}
	for k, v := range cm.Data {
		keys[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		keys[k] = v
	}
	return selectKeys("ConfigMap", name, keys, items, defMode, optional)
}

func secretFiles(ctx context.Context, cl kubernetes.Interface, ns, name string, items []corev1.KeyToPath, defMode *int32, optional *bool) (map[string]volFile, error) {
	sec, err := cl.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && optional != nil && *optional {
			return map[string]volFile{}, nil
		}
		return nil, fmt.Errorf("secret %q: %s", name, cleanErr(err))
	}
	return selectKeys("Secret", name, sec.Data, items, defMode, optional)
}

func downwardFiles(p *corev1.Pod, c corev1.Container, o TranslateOptions, items []corev1.DownwardAPIVolumeFile, defMode *int32) (map[string]volFile, error) {
	out := map[string]volFile{}
	for _, it := range items {
		if err := validRelPath(it.Path); err != nil {
			return nil, err
		}
		var val string
		var err error
		switch {
		case it.FieldRef != nil && (it.FieldRef.FieldPath == "metadata.labels" || it.FieldRef.FieldPath == "metadata.annotations"):
			// Only a downwardAPI volume can show the whole map: one `key="value"` line each, sorted, like kubelet.
			m := p.Labels
			if it.FieldRef.FieldPath == "metadata.annotations" {
				m = p.Annotations
			}
			val = formatMap(m)
		case it.FieldRef != nil:
			val, err = fieldValue(p, o, it.FieldRef.FieldPath)
		case it.ResourceFieldRef != nil:
			cc := c
			val, err = resourceValue(cc, it.ResourceFieldRef)
		default:
			err = fmt.Errorf("an item has no source")
		}
		if err != nil {
			return nil, fmt.Errorf("downwardAPI %s: %v", it.Path, err)
		}
		out[it.Path] = volFile{[]byte(val), fileMode(it.Mode, defaultVolumeMode(defMode))}
	}
	return out, nil
}

func formatMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%q\n", k, m[k])
	}
	return b.String()
}

// serviceAccountTokenFile returns the token file of a projected volume, asking the API server for a new token only
// when there is none or the old one is due for replacement. The token is bound to this Pod, so it stops working
// when the Pod is deleted, and to its audience and lifetime as the Pod's spec asks. A failed renewal keeps the old
// token until it really expires.
func serviceAccountTokenFile(ctx context.Context, cl kubernetes.Interface, st *volumeState, now time.Time, p *corev1.Pod, volume string, proj *corev1.ServiceAccountTokenProjection, defMode *int32) (volFile, error) {
	if err := validRelPath(proj.Path); err != nil {
		return volFile{}, err
	}
	key := volume + "/" + proj.Path
	if st.tokens == nil {
		st.tokens = map[string]*issuedToken{}
	}
	cur := st.tokens[key]
	if cur == nil || !now.Before(cur.refreshAt()) {
		sa := p.Spec.ServiceAccountName
		if sa == "" {
			sa = "default"
		}
		lifetime := int64(3600)
		if proj.ExpirationSeconds != nil {
			lifetime = *proj.ExpirationSeconds
		}
		req := &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{
			ExpirationSeconds: &lifetime,
			BoundObjectRef:    &authv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: p.Name, UID: p.UID},
		}}
		if proj.Audience != "" {
			req.Spec.Audiences = []string{proj.Audience}
		}
		resp, err := cl.CoreV1().ServiceAccounts(p.Namespace).CreateToken(ctx, sa, req, metav1.CreateOptions{})
		switch {
		case err == nil:
			cur = &issuedToken{token: resp.Status.Token, issued: now, expires: resp.Status.ExpirationTimestamp.Time}
			st.tokens[key] = cur
		case cur != nil && now.Before(cur.expires):
			// keep the token we have; the next resync tries again
		default:
			return volFile{}, fmt.Errorf("serviceaccounts/%s token: %s", sa, cleanErr(err))
		}
	}
	return volFile{[]byte(cur.token), fileMode(nil, defaultVolumeMode(defMode))}, nil
}

// ---- writing

// writeVolume replaces the content of dir with files the way kubelet does: the files go into a new timestamped
// directory, `..data` is switched to it atomically, and each top-level entry is a symlink through `..data`. A
// container that has the directory bind-mounted sees the new content at once, without being restarted.
// It returns false when nothing changed.
func writeVolume(dir, hashFile string, files map[string]volFile) (bool, error) {
	h := sha256.New()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00", n, files[n].mode, len(files[n].data))
		h.Write(files[n].data)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if old, err := os.ReadFile(hashFile); err == nil && string(old) == sum {
		if _, err := os.Lstat(filepath.Join(dir, "..data")); err == nil || len(files) == 0 {
			return false, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	ts := "..2" + strconv.FormatInt(time.Now().UnixNano(), 36)
	tsDir := filepath.Join(dir, ts)
	if err := os.Mkdir(tsDir, 0o700); err != nil {
		return false, err
	}
	for _, n := range names {
		full := filepath.Join(tsDir, n)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return false, err
		}
		if err := os.WriteFile(full, files[n].data, 0o600); err != nil {
			return false, err
		}
		if err := os.Chmod(full, files[n].mode); err != nil {
			return false, err
		}
	}
	link := filepath.Join(dir, "..data_tmp")
	os.Remove(link)
	if err := os.Symlink(ts, link); err != nil {
		return false, err
	}
	if err := os.Rename(link, filepath.Join(dir, "..data")); err != nil {
		return false, err
	}
	tops := map[string]bool{}
	for _, n := range names {
		tops[strings.SplitN(n, "/", 2)[0]] = true
	}
	for top := range tops {
		l := filepath.Join(dir, top)
		want := filepath.Join("..data", top)
		if cur, err := os.Readlink(l); err == nil && cur == want {
			continue
		}
		os.Remove(l)
		if err := os.Symlink(want, l); err != nil {
			return false, err
		}
	}
	// Remove what the new content no longer has: our own earlier symlinks and old timestamped directories.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "..data" || name == ts || tops[name]:
		case strings.HasPrefix(name, ".."):
			os.RemoveAll(filepath.Join(dir, name))
		case e.Type()&os.ModeSymlink != 0:
			os.Remove(filepath.Join(dir, name))
		}
	}
	return true, os.WriteFile(hashFile, []byte(sum), 0o600)
}

// prepareVolumes makes the volumes of a Pod exist and keeps their content current. The first preparation must
// succeed before the container starts (an error is returned, and the Pod waits); later refresh errors keep the
// old content.
func prepareVolumes(ctx context.Context, c kubernetes.Interface, uid string, st *volumeState, root string, plans []*volumePlan, now time.Time) error {
	if len(plans) == 0 {
		return nil
	}
	if root == "" {
		return fmt.Errorf("this node has no directory for Pod volumes")
	}
	podDir := filepath.Join(root, uid)
	if err := ensureOwnedDir(root, podDir, uid); err != nil {
		return err
	}
	if st.nextSync == nil {
		st.nextSync = map[string]time.Time{}
	}
	for _, pl := range plans {
		_, started := st.nextSync[pl.name]
		if started && now.Before(st.nextSync[pl.name]) {
			continue
		}
		if err := os.MkdirAll(pl.dir, 0o700); err != nil {
			return fmt.Errorf("MountVolume.SetUp failed for volume %q: %v", pl.name, err)
		}
		if pl.build != nil {
			files, err := pl.build(ctx, c, st, now)
			if err != nil {
				if !started {
					return fmt.Errorf("MountVolume.SetUp failed for volume %q: %v", pl.name, err)
				}
				// A refresh that fails keeps the content the container has; try again at the next resync.
				st.nextSync[pl.name] = now.Add(volumeResync)
				continue
			}
			if _, err := writeVolume(pl.dir, filepath.Join(podDir, "."+pl.name+".hash"), files); err != nil {
				return fmt.Errorf("MountVolume.SetUp failed for volume %q: %v", pl.name, err)
			}
		}
		// A little spread, so many Pods do not all read the API at the same instant.
		st.nextSync[pl.name] = now.Add(volumeResync + time.Duration(len(pl.name)%7)*time.Second)
	}
	return nil
}

// ---- ownership and cleanup

type volumeMarker struct {
	UID string `json:"uid"`
}

// ensureOwnedDir creates root/<uid> with its marker. An existing directory is only accepted if its marker names this
// Pod, so nothing that merely sits in the volumes directory is ever treated as ours.
func ensureOwnedDir(root, podDir, uid string) error {
	if !podUIDPattern.MatchString(uid) {
		return fmt.Errorf("refusing to create volumes for the unusual Pod UID %q", uid)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if filepath.Dir(podDir) != filepath.Clean(root) {
		return fmt.Errorf("volume directory outside its root")
	}
	if info, err := os.Lstat(podDir); err == nil {
		if !info.IsDir() || !markerMatches(podDir, uid) {
			return fmt.Errorf("%s exists but is not a volume directory of this Pod", podDir)
		}
		return nil
	}
	if err := os.Mkdir(podDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(podDir, volumeMarkerFile), []byte(`{"uid":"`+uid+`"}`), 0o600)
}

func markerMatches(podDir, uid string) bool {
	b, err := os.ReadFile(filepath.Join(podDir, volumeMarkerFile))
	return err == nil && strings.TrimSpace(string(b)) == `{"uid":"`+uid+`"}`
}

// volumeCleaner removes the volume directories of Pods that no longer need them, and only those. It is conservative
// on purpose: a directory goes only if its name is a Pod UID, its marker names that same UID, nothing runs for that
// UID, the Pod has been missing for a while (so one odd API answer cannot trigger it), and it is a real directory
// below the root (never a symlink, never outside).
type volumeCleaner struct {
	missingSince map[string]time.Time
}

func (vc *volumeCleaner) sweep(root string, keep func(uid string) bool, running func(uid string) bool, now time.Time) (removed []string) {
	if root == "" {
		return nil
	}
	if vc.missingSince == nil {
		vc.missingSince = map[string]time.Time{}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		uid := e.Name()
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || !podUIDPattern.MatchString(uid) {
			continue
		}
		seen[uid] = true
		podDir := filepath.Join(root, uid)
		if !markerMatches(podDir, uid) || keep(uid) || running(uid) {
			delete(vc.missingSince, uid)
			continue
		}
		since, ok := vc.missingSince[uid]
		if !ok {
			vc.missingSince[uid] = now
			continue
		}
		if now.Sub(since) < orphanGrace {
			continue
		}
		// Re-check right before removing: a real directory directly below the root.
		if info, err := os.Lstat(podDir); err != nil || !info.IsDir() || filepath.Dir(podDir) != filepath.Clean(root) {
			continue
		}
		if os.RemoveAll(podDir) == nil {
			removed = append(removed, uid)
			delete(vc.missingSince, uid)
		}
	}
	for uid := range vc.missingSince {
		if !seen[uid] {
			delete(vc.missingSince, uid)
		}
	}
	return removed
}

// podMountsAToken says whether one of the container's mounts is a projected volume with a ServiceAccount token.
func podMountsAToken(p *corev1.Pod, c corev1.Container) bool {
	byName := map[string]corev1.Volume{}
	for _, v := range p.Spec.Volumes {
		byName[v.Name] = v
	}
	for _, m := range c.VolumeMounts {
		if v, ok := byName[m.Name]; ok && v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.ServiceAccountToken != nil {
					return true
				}
			}
		}
	}
	return false
}

const NoteKubernetesAddressUnknown = "KubernetesServiceAddressUnknown"

// kubernetesServiceEnv gives an in-cluster client library the API server's real address: a Mac has no ClusterIP, but
// it can reach the control plane itself. It never overrides what the Pod sets.
func kubernetesServiceEnv(apiServer string) (map[string]string, *Note) {
	host, port, err := net.SplitHostPort(apiServer)
	if err != nil || host == "" || port == "" {
		return nil, &Note{corev1.EventTypeWarning, NoteKubernetesAddressUnknown, "the ServiceAccount token is mounted but this node does not know an address of the API server that the Pod can reach (set apiServerAddress in the node options), so KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set"}
	}
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
		return nil, &Note{corev1.EventTypeWarning, NoteKubernetesAddressUnknown, "this node reaches the API server through a loopback address that a Pod cannot use; set apiServerAddress in the node options so KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT can be set"}
	}
	return map[string]string{"KUBERNETES_SERVICE_HOST": host, "KUBERNETES_SERVICE_PORT": port}, nil
}
