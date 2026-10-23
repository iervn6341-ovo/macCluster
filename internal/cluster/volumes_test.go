package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	runtime "maccluster/internal/runtime"
)

const podUID = "11111111-2222-3333-4444-555555555555"

func volPod(vols []corev1.Volume, mounts []corev1.VolumeMount) *corev1.Pod {
	p := testPod()
	p.UID = podUID
	p.Spec.Volumes = vols
	p.Spec.Containers[0].VolumeMounts = mounts
	return p
}

func emptyDirVol(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func volOpts(root string, allowed ...string) TranslateOptions {
	return TranslateOptions{Volumes: volumeSettings{root: root, allowedHostPaths: allowed}}
}

func plan(t *testing.T, p *corev1.Pod, o TranslateOptions) (volumeMountsResult, []string, error) {
	t.Helper()
	var problems []string
	res, err := planVolumes(p, p.Spec.Containers[0], o, func(f string, a ...any) { problems = append(problems, sprintf(f, a...)) })
	return res, problems, err
}

func sprintf(f string, a ...any) string {
	return strings.TrimSpace(strings.Join([]string{fmtSprintf(f, a...)}, ""))
}

func TestPlanEmptyDirMemoryAndSubPath(t *testing.T) {
	root := t.TempDir()
	limit := resource.MustParse("8Mi")
	p := volPod([]corev1.Volume{
		emptyDirVol("data"),
		{Name: "ram", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &limit}}},
	}, []corev1.VolumeMount{
		{Name: "data", MountPath: "/data"},
		{Name: "data", MountPath: "/sub", SubPath: "a/b"},
		{Name: "ram", MountPath: "/ram", ReadOnly: true},
	})
	res, problems, err := plan(t, p, volOpts(root))
	if err != nil || len(problems) != 0 {
		t.Fatalf("%v %v", err, problems)
	}
	want := map[string]runtime.Mount{
		"/data": {Name: "data", HostPath: filepath.Join(root, podUID, "data"), Container: "/data"},
		"/sub":  {Name: "data", HostPath: filepath.Join(root, podUID, "data", "a", "b"), Container: "/sub"},
		"/ram":  {Name: "ram", Container: "/ram", Tmpfs: true, SizeBytes: 8 << 20},
	}
	if len(res.mounts) != 3 {
		t.Fatalf("mounts: %+v", res.mounts)
	}
	for _, m := range res.mounts {
		if m != want[m.Container] {
			t.Errorf("mount at %s: got %+v want %+v", m.Container, m, want[m.Container])
		}
	}
	if len(res.plans) != 1 || res.plans[0].name != "data" {
		t.Fatalf("only the directory volume is written to disk: %+v", res.plans)
	}
}

func TestPlanRefusesWhatItCannotMount(t *testing.T) {
	pvc := corev1.Volume{Name: "claim", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}}
	p := volPod([]corev1.Volume{pvc, emptyDirVol("d")}, []corev1.VolumeMount{
		{Name: "claim", MountPath: "/c"}, {Name: "missing", MountPath: "/m"}, {Name: "d", MountPath: "/x", SubPathExpr: "$(POD)"},
	})
	_, problems, err := plan(t, p, volOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, " | ")
	for _, want := range []string{"persistentVolumeClaim", "hostPath below an allowed path", "refers to a volume the Pod does not define", "subPathExpr"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

func TestHostPathPolicy(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "models")
	evil := filepath.Join(base, "models-evil")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{allowed, evil, outside, filepath.Join(allowed, "sub")} {
		os.MkdirAll(d, 0o755)
	}
	os.Symlink(outside, filepath.Join(allowed, "escape")) // a link inside the allowed path that leads out of it
	dirType := corev1.HostPathDirectory
	mk := func(path string, typ *corev1.HostPathType) *corev1.HostPathVolumeSource {
		return &corev1.HostPathVolumeSource{Path: path, Type: typ}
	}
	if _, err := checkHostPath(mk(allowed, &dirType), nil); err == nil || !strings.Contains(err.Error(), "not allowed on this node") {
		t.Fatalf("with no allow list nothing may be mounted: %v", err)
	}
	if got, err := checkHostPath(mk(filepath.Join(allowed, "sub"), &dirType), []string{allowed}); err != nil || !strings.HasSuffix(got, "models/sub") {
		t.Fatalf("a path below the allowed one: %q %v", got, err)
	}
	for name, path := range map[string]string{"sibling with the same prefix": evil, "outside": outside, "symlink escape": filepath.Join(allowed, "escape"), "relative": "models"} {
		_, err := checkHostPath(mk(path, nil), []string{allowed})
		se, ok := err.(*SpecError)
		if !ok || se.Reason != ReasonHostPathNotAllowed || !strings.Contains(err.Error(), "allowedHostPaths") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// DirectoryOrCreate creates inside the allowed path and never outside it.
	create := corev1.HostPathDirectoryOrCreate
	made := filepath.Join(allowed, "new", "deep")
	if _, err := checkHostPath(mk(filepath.Join(allowed, "new"), &create), []string{allowed}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(allowed, "new")); err != nil || !info.IsDir() {
		t.Fatalf("not created: %v", err)
	}
	_ = made
	if _, err := checkHostPath(mk(filepath.Join(outside, "nope"), &create), []string{allowed}); err == nil {
		t.Fatal("DirectoryOrCreate created something outside the allowed path")
	}
	if _, err := os.Stat(filepath.Join(outside, "nope")); err == nil {
		t.Fatal("a directory was created outside the allowed path")
	}
	// A missing path of type Directory is a mount failure, not a policy denial.
	_, err := checkHostPath(mk(filepath.Join(allowed, "missing"), &dirType), []string{allowed})
	if se, ok := err.(*SpecError); !ok || se.Reason != ReasonFailedMount {
		t.Fatalf("missing directory: %v", err)
	}
	// Wrong type.
	file := filepath.Join(allowed, "f.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	if _, err := checkHostPath(mk(file, &dirType), []string{allowed}); err == nil {
		t.Fatal("a file accepted as a directory")
	}
}

func TestWriteVolumeIsAtomicKubeletLayoutAndIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vol")
	hash := filepath.Join(filepath.Dir(dir), ".vol.hash")
	files := map[string]volFile{"a.conf": {[]byte("one"), 0o644}, "sub/b": {[]byte("two"), 0o600}}
	changed, err := writeVolume(dir, hash, files)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }
	if read("a.conf") != "one" || read("sub/b") != "two" {
		t.Fatalf("content: %q %q", read("a.conf"), read("sub/b"))
	}
	if l, _ := os.Readlink(filepath.Join(dir, "a.conf")); l != filepath.Join("..data", "a.conf") {
		t.Fatalf("top-level entries are symlinks through ..data: %q", l)
	}
	if info, _ := os.Stat(filepath.Join(dir, "sub", "b")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v", info.Mode())
	}
	if again, err := writeVolume(dir, hash, files); err != nil || again {
		t.Fatalf("unchanged content must not be rewritten: %v %v", again, err)
	}
	// An update replaces ..data in one step, adds the new file and drops the one that is gone.
	before, _ := os.Readlink(filepath.Join(dir, "..data"))
	files2 := map[string]volFile{"a.conf": {[]byte("ONE"), 0o644}, "c": {[]byte("three"), 0o644}}
	if changed, err := writeVolume(dir, hash, files2); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	after, _ := os.Readlink(filepath.Join(dir, "..data"))
	if before == after || read("a.conf") != "ONE" || read("c") != "three" {
		t.Fatalf("update: %q -> %q, %q %q", before, after, read("a.conf"), read("c"))
	}
	if _, err := os.Lstat(filepath.Join(dir, "sub")); err == nil {
		t.Fatal("an entry that is no longer in the content was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, before)); err == nil {
		t.Fatal("the old timestamped directory was kept")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 4 { // ..data, ..<ts>, a.conf, c
		t.Fatalf("unexpected entries: %v", entries)
	}
}

func cmData(ns, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: data}
}

func TestPrepareConfigMapSecretAndFailedMount(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset(cmData("default", "cfg", map[string]string{"app.yaml": "v1", "other": "x"}),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sec"}, Data: map[string][]byte{"token": []byte("s3cret")}})
	mode := int32(0o400)
	p := volPod([]corev1.Volume{
		{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}, Items: []corev1.KeyToPath{{Key: "app.yaml", Path: "conf/app.yaml"}}}}},
		{Name: "sec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "sec", DefaultMode: &mode}}},
	}, []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/cfg", ReadOnly: true}, {Name: "sec", MountPath: "/etc/sec"}})
	p.Namespace = "default"
	res, problems, err := plan(t, p, volOpts(root))
	if err != nil || len(problems) != 0 {
		t.Fatalf("%v %v", err, problems)
	}
	st := &volumeState{}
	now := time.Now()
	if err := prepareVolumes(context.Background(), client, podUID, st, root, res.plans, now); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, podUID, "cfg", "conf", "app.yaml")); string(b) != "v1" {
		t.Fatalf("configMap item: %q", b)
	}
	if _, err := os.Stat(filepath.Join(root, podUID, "cfg", "other")); err == nil {
		t.Fatal("a key that is not in items was written")
	}
	if info, _ := os.Stat(filepath.Join(root, podUID, "sec", "token")); info.Mode().Perm() != 0o400 {
		t.Fatalf("defaultMode: %v", info.Mode())
	}
	if b, err := os.ReadFile(filepath.Join(root, podUID, volumeMarkerFile)); err != nil || !strings.Contains(string(b), podUID) {
		t.Fatalf("ownership marker: %q %v", b, err)
	}
	// The ConfigMap changes: nothing happens before the resync time, then the new content appears.
	client.CoreV1().ConfigMaps("default").Update(context.Background(), cmData("default", "cfg", map[string]string{"app.yaml": "v2"}), metav1.UpdateOptions{})
	prepareVolumes(context.Background(), client, podUID, st, root, res.plans, now.Add(10*time.Second))
	if b, _ := os.ReadFile(filepath.Join(root, podUID, "cfg", "conf", "app.yaml")); string(b) != "v1" {
		t.Fatalf("content changed before the resync: %q", b)
	}
	prepareVolumes(context.Background(), client, podUID, st, root, res.plans, now.Add(2*time.Minute))
	if b, _ := os.ReadFile(filepath.Join(root, podUID, "cfg", "conf", "app.yaml")); string(b) != "v2" {
		t.Fatalf("update not applied: %q", b)
	}
	// The source disappears: the container keeps what it has.
	client.CoreV1().ConfigMaps("default").Delete(context.Background(), "cfg", metav1.DeleteOptions{})
	if err := prepareVolumes(context.Background(), client, podUID, st, root, res.plans, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("a refresh failure after start must not fail the Pod: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, podUID, "cfg", "conf", "app.yaml")); string(b) != "v2" {
		t.Fatalf("content was lost: %q", b)
	}
}

func TestMissingSourceIsAFailedMountUntilItAppears(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset()
	p := volPod([]corev1.Volume{{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "later"}}}}},
		[]corev1.VolumeMount{{Name: "cfg", MountPath: "/c"}})
	p.Namespace = "default"
	res, _, _ := plan(t, p, volOpts(root))
	st := &volumeState{}
	err := prepareVolumes(context.Background(), client, podUID, st, root, res.plans, time.Now())
	if err == nil || !strings.Contains(err.Error(), `MountVolume.SetUp failed for volume "cfg"`) || !strings.Contains(err.Error(), "later") {
		t.Fatalf("got %v", err)
	}
	client.CoreV1().ConfigMaps("default").Create(context.Background(), cmData("default", "later", map[string]string{"k": "v"}), metav1.CreateOptions{})
	if err := prepareVolumes(context.Background(), client, podUID, st, root, res.plans, time.Now()); err != nil {
		t.Fatalf("it must start once the ConfigMap exists: %v", err)
	}
	// optional: true is fine without the source.
	opt := true
	p.Spec.Volumes[0].ConfigMap.Name, p.Spec.Volumes[0].ConfigMap.Optional = "never-there", &opt
	res, _, _ = plan(t, p, volOpts(root))
	if err := prepareVolumes(context.Background(), client, podUID, &volumeState{}, root, res.plans, time.Now()); err != nil {
		t.Fatalf("optional source: %v", err)
	}
}

func TestDownwardAPIAndProjectedVolumes(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset(cmData("default", "cfg", map[string]string{"k": "v"}))
	p := volPod([]corev1.Volume{
		{Name: "me", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: []corev1.DownwardAPIVolumeFile{{Path: "name", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}}},
		{Name: "all", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "ns", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}}},
		}}}},
	}, []corev1.VolumeMount{{Name: "me", MountPath: "/me"}, {Name: "all", MountPath: "/all"}})
	p.Namespace, p.Name = "default", "mypod"
	res, problems, err := plan(t, p, volOpts(root))
	if err != nil || len(problems) != 0 {
		t.Fatalf("%v %v", err, problems)
	}
	if err := prepareVolumes(context.Background(), client, podUID, &volumeState{}, root, res.plans, time.Now()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"me/name": "mypod", "all/k": "v", "all/ns": "default"} {
		if b, _ := os.ReadFile(filepath.Join(root, podUID, path)); string(b) != want {
			t.Errorf("%s = %q, want %q", path, b, want)
		}
	}
	// The whole label and annotation maps are files of `key="value"` lines, sorted.
	p.Labels = map[string]string{"b": "2", "a": "one \"x\""}
	p.Annotations = map[string]string{"n": "v"}
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "meta", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: []corev1.DownwardAPIVolumeFile{
		{Path: "labels", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"}},
		{Path: "annotations", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations"}}}}}})
	p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "meta", MountPath: "/meta"})
	res2, problems2, err2 := plan(t, p, volOpts(root))
	if err2 != nil || len(problems2) != 0 {
		t.Fatalf("%v %v", err2, problems2)
	}
	if err := prepareVolumes(context.Background(), client, podUID, &volumeState{}, root, res2.plans, time.Now()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, podUID, "meta", "labels")); string(b) != "a=\"one \\\"x\\\"\"\nb=\"2\"\n" {
		t.Fatalf("labels file: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, podUID, "meta", "annotations")); string(b) != "n=\"v\"\n" {
		t.Fatalf("annotations file: %q", b)
	}
}

func TestCleanupRemovesOnlyWhatIsOursAndOrphaned(t *testing.T) {
	orphanGrace = 0
	defer func() { orphanGrace = 30 * time.Second }()
	root := t.TempDir()
	mk := func(uid string, marker bool) string {
		d := filepath.Join(root, uid)
		os.MkdirAll(filepath.Join(d, "data"), 0o700)
		os.WriteFile(filepath.Join(d, "data", "f"), []byte("x"), 0o600)
		if marker {
			os.WriteFile(filepath.Join(d, volumeMarkerFile), []byte(`{"uid":"`+uid+`"}`), 0o600)
		}
		return d
	}
	gone := "aaaaaaaa-0000-0000-0000-000000000001"
	kept := "aaaaaaaa-0000-0000-0000-000000000002"
	busy := "aaaaaaaa-0000-0000-0000-000000000003"
	noMarker := "aaaaaaaa-0000-0000-0000-000000000004"
	wrongMarker := "aaaaaaaa-0000-0000-0000-000000000005"
	dGone, dKept, dBusy, dNoMarker, dWrong := mk(gone, true), mk(kept, true), mk(busy, true), mk(noMarker, false), mk(wrongMarker, true)
	os.WriteFile(filepath.Join(dWrong, volumeMarkerFile), []byte(`{"uid":"someone-else"}`), 0o600)
	// Things that are not Pod directories at all, and a symlink that looks like one.
	notUID := filepath.Join(root, "precious")
	os.MkdirAll(notUID, 0o700)
	os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600)
	target := t.TempDir()
	os.WriteFile(filepath.Join(target, "keep.txt"), []byte("x"), 0o600)
	link := filepath.Join(root, "aaaaaaaa-0000-0000-0000-000000000006")
	os.Symlink(target, link)

	vc := &volumeCleaner{}
	keep := func(uid string) bool { return uid == kept }
	running := func(uid string) bool { return uid == busy }
	now := time.Now()
	// The first sweep only notes what is missing; nothing is removed yet.
	if removed := vc.sweep(root, keep, running, now); len(removed) != 0 {
		t.Fatalf("removed on first sight: %v", removed)
	}
	removed := vc.sweep(root, keep, running, now.Add(time.Second))
	if len(removed) != 1 || removed[0] != gone {
		t.Fatalf("removed %v, want only %s", removed, gone)
	}
	if _, err := os.Stat(dGone); err == nil {
		t.Fatal("the orphan was kept")
	}
	for name, d := range map[string]string{"kept Pod": dKept, "running container": dBusy, "no marker": dNoMarker, "someone else's marker": dWrong, "not a UID": notUID, "plain file": filepath.Join(root, "file.txt")} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s was removed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Fatal("cleanup followed a symlink out of the root")
	}
	// A Pod that comes back before the grace period ends is not removed.
	revived := "aaaaaaaa-0000-0000-0000-000000000007"
	dRevived := mk(revived, true)
	orphanGrace = time.Minute
	vc.sweep(root, func(string) bool { return false }, running, now)
	vc.sweep(root, func(uid string) bool { return uid == revived }, running, now.Add(2*time.Minute))
	if _, err := os.Stat(dRevived); err != nil {
		t.Fatal("a Pod that was back by the time of the check lost its volumes")
	}
}

func TestEnsureOwnedDirRefusesForeignDirectories(t *testing.T) {
	root := t.TempDir()
	if err := ensureOwnedDir(root, filepath.Join(root, podUID), podUID); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnedDir(root, filepath.Join(root, podUID), podUID); err != nil {
		t.Fatalf("an owned directory is accepted again: %v", err)
	}
	other := "99999999-2222-3333-4444-555555555555"
	os.MkdirAll(filepath.Join(root, other), 0o700) // exists, no marker
	if err := ensureOwnedDir(root, filepath.Join(root, other), other); err == nil {
		t.Fatal("a directory without our marker was taken over")
	}
	if err := ensureOwnedDir(root, filepath.Join(root, "../escape"), podUID); err == nil {
		t.Fatal("a path outside the root was accepted")
	}
	if err := ensureOwnedDir(root, filepath.Join(root, "not-a-uid"), "not-a-uid"); err == nil {
		t.Fatal("an unusual UID was accepted")
	}
}

func TestTranslateGivesADockerPodItsMounts(t *testing.T) {
	root := t.TempDir()
	p := volPod([]corev1.Volume{emptyDirVol("data")}, []corev1.VolumeMount{{Name: "data", MountPath: "/data"}})
	var plans []*volumePlan
	spec, _, err := TranslateWithOptions(p, TranslateOptions{Volumes: volumeSettings{root: root}, Plans: &plans})
	if err != nil || len(spec.Mounts) != 1 || spec.Mounts[0].Container != "/data" || len(plans) != 1 {
		t.Fatalf("%+v %v %v", spec.Mounts, plans, err)
	}
	// A native Pod still cannot have volumes.
	p.Labels[ExecutorLabel] = "native"
	if _, _, err := TranslateWithOptions(p, TranslateOptions{Volumes: volumeSettings{root: root}}); err == nil {
		t.Fatal("a native Pod with a volume must be refused for now")
	}
	// A hostPath that is not allowed keeps the Pod Pending with its own reason.
	hp := corev1.Volume{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc"}}}
	p2 := volPod([]corev1.Volume{hp}, []corev1.VolumeMount{{Name: "h", MountPath: "/h"}})
	_, _, err = TranslateWithOptions(p2, TranslateOptions{Volumes: volumeSettings{root: root}})
	if se, ok := err.(*SpecError); !ok || se.Reason != ReasonHostPathNotAllowed {
		t.Fatalf("got %v", err)
	}
}

func fmtSprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

// tokenClient returns a fake client whose TokenRequest endpoint issues numbered tokens and records the requests.
func tokenClient(t *testing.T, fail *bool) (*fake.Clientset, *[]*authv1.TokenRequest) {
	t.Helper()
	client := fake.NewSimpleClientset()
	var requests []*authv1.TokenRequest
	client.PrependReactor("create", "serviceaccounts", func(a clienttesting.Action) (bool, kruntime.Object, error) {
		ca, ok := a.(clienttesting.CreateAction)
		if !ok || a.GetSubresource() != "token" {
			return false, nil, nil
		}
		if fail != nil && *fail {
			return true, nil, fmt.Errorf("the API server is not reachable")
		}
		req := ca.GetObject().(*authv1.TokenRequest)
		requests = append(requests, req.DeepCopy())
		out := req.DeepCopy()
		out.Status = authv1.TokenRequestStatus{Token: fmt.Sprintf("token-%d", len(requests)), ExpirationTimestamp: metav1.NewTime(tokenNow.Add(time.Duration(*req.Spec.ExpirationSeconds) * time.Second))}
		return true, out, nil
	})
	return client, &requests
}

var tokenNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func tokenPod() *corev1.Pod {
	exp := int64(3607)
	p := volPod([]corev1.Volume{{Name: "api", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: &exp, Audience: "https://api.example"}},
		{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}}},
	}}}}}, []corev1.VolumeMount{{Name: "api", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}})
	p.Namespace, p.Name, p.Spec.ServiceAccountName = "default", "mypod", "reader"
	return p
}

func TestServiceAccountTokenIsBoundToThePodAndRotated(t *testing.T) {
	root := t.TempDir()
	client, requests := tokenClient(t, nil)
	p := tokenPod()
	res, problems, err := plan(t, p, volOpts(root))
	if err != nil || len(problems) != 0 {
		t.Fatalf("%v %v", err, problems)
	}
	st := &volumeState{}
	read := func() string { b, _ := os.ReadFile(filepath.Join(root, podUID, "api", "token")); return string(b) }
	if err := prepareVolumes(context.Background(), client, podUID, st, root, res.plans, tokenNow); err != nil {
		t.Fatal(err)
	}
	if read() != "token-1" || len(*requests) != 1 {
		t.Fatalf("first token: %q after %d requests", read(), len(*requests))
	}
	req := (*requests)[0]
	ref := req.Spec.BoundObjectRef
	if ref == nil || ref.Kind != "Pod" || ref.Name != "mypod" || ref.UID != podUID {
		t.Fatalf("the token must be bound to this Pod: %+v", ref)
	}
	if len(req.Spec.Audiences) != 1 || req.Spec.Audiences[0] != "https://api.example" || *req.Spec.ExpirationSeconds != 3607 {
		t.Fatalf("audience/lifetime: %+v", req.Spec)
	}
	if b, _ := os.ReadFile(filepath.Join(root, podUID, "api", "namespace")); string(b) != "default" {
		t.Fatalf("namespace file: %q", b)
	}
	// Resyncs during the first 80% of the lifetime keep the same token and ask for nothing.
	for _, at := range []time.Duration{time.Minute, 20 * time.Minute, 47 * time.Minute} {
		prepareVolumes(context.Background(), client, podUID, st, root, res.plans, tokenNow.Add(at))
	}
	if read() != "token-1" || len(*requests) != 1 {
		t.Fatalf("a token that is not due was replaced: %q, %d requests", read(), len(*requests))
	}
	// After 80% of 3607 s (about 48 minutes) a new one is issued and the file switches to it.
	prepareVolumes(context.Background(), client, podUID, st, root, res.plans, tokenNow.Add(49*time.Minute))
	if read() != "token-2" || len(*requests) != 2 {
		t.Fatalf("not rotated: %q, %d requests", read(), len(*requests))
	}
	// If the API server cannot be reached the old token stays until it really expires...
	fail := true
	failing, _ := tokenClient(t, &fail)
	st.tokens["api/token"].issued = tokenNow
	st.tokens["api/token"].expires = tokenNow.Add(time.Hour)
	if err := prepareVolumes(context.Background(), failing, podUID, st, root, res.plans, tokenNow.Add(55*time.Minute)); err != nil {
		t.Fatalf("a failed renewal of a token that is still valid must not fail the Pod: %v", err)
	}
	if read() != "token-2" {
		t.Fatalf("the valid token was lost: %q", read())
	}
	// ...and a token that has expired with no replacement is an error the Pod reports.
	st.nextSync = nil
	if err := prepareVolumes(context.Background(), failing, podUID, st, root, res.plans, tokenNow.Add(2*time.Hour)); err == nil || !strings.Contains(err.Error(), "serviceaccounts/reader token") {
		t.Fatalf("got %v", err)
	}
}

func TestTheFirstTokenMustSucceedBeforeTheContainerStarts(t *testing.T) {
	fail := true
	client, _ := tokenClient(t, &fail)
	root := t.TempDir()
	res, _, _ := plan(t, tokenPod(), volOpts(root))
	err := prepareVolumes(context.Background(), client, podUID, &volumeState{}, root, res.plans, tokenNow)
	if err == nil || !strings.Contains(err.Error(), `MountVolume.SetUp failed for volume "api"`) {
		t.Fatalf("got %v", err)
	}
}

func TestTokenIsMountedOnlyWhenAskedForAndOnlyForDocker(t *testing.T) {
	yes, no := true, false
	p := tokenPod()
	if tokenIsMounted(p, "docker") {
		t.Fatal("unset must not mount: the API server injects the volume either way")
	}
	p.Spec.AutomountServiceAccountToken = &no
	if tokenIsMounted(p, "docker") {
		t.Fatal("false must not mount")
	}
	p.Spec.AutomountServiceAccountToken = &yes
	if !tokenIsMounted(p, "docker") || tokenIsMounted(p, "native") {
		t.Fatal("explicit true mounts for Docker only")
	}
}

func apiInjectedPod(automount *bool) *corev1.Pod {
	exp := int64(3607)
	p := testPod()
	p.UID = podUID
	p.Spec.AutomountServiceAccountToken = automount
	p.Spec.Volumes = []corev1.Volume{{Name: "kube-api-access-abcde", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: &exp}},
		{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
		{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}}},
	}}}}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "kube-api-access-abcde", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}}
	return p
}

func TestExplicitTrueMountsTheInjectedTokenVolumeAndSetsTheAPIServerAddress(t *testing.T) {
	yes := true
	root := t.TempDir()
	o := TranslateOptions{Volumes: volumeSettings{root: root}, APIServer: "192.0.2.80:6443"}
	var plans []*volumePlan
	o.Plans = &plans
	p := apiInjectedPod(&yes)
	p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "KUBERNETES_SERVICE_PORT", Value: "9999"}}
	spec, notes, err := TranslateWithOptions(p, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Container != "/var/run/secrets/kubernetes.io/serviceaccount" || !spec.Mounts[0].ReadOnly || len(plans) != 1 {
		t.Fatalf("mounts %+v plans %d", spec.Mounts, len(plans))
	}
	for _, n := range notes {
		if n.Reason == NoteServiceAccountTokenNotMounted {
			t.Fatal("the not-mounted warning must not appear when the token is mounted")
		}
	}
	if spec.EnvResolved["KUBERNETES_SERVICE_HOST"] != "192.0.2.80" {
		t.Fatalf("host: %v", spec.EnvResolved)
	}
	if _, set := spec.EnvResolved["KUBERNETES_SERVICE_PORT"]; set || spec.Env["KUBERNETES_SERVICE_PORT"] != "9999" {
		t.Fatalf("the Pod's own value must win: %v / %v", spec.EnvResolved, spec.Env)
	}
	// A loopback address cannot be used from a container: warn, set nothing.
	o.APIServer = "127.0.0.1:16443"
	spec, notes, err = TranslateWithOptions(apiInjectedPod(&yes), o)
	if err != nil || spec.EnvResolved["KUBERNETES_SERVICE_HOST"] != "" {
		t.Fatalf("%v %v", spec.EnvResolved, err)
	}
	found := false
	for _, n := range notes {
		found = found || n.Reason == NoteKubernetesAddressUnknown
	}
	if !found {
		t.Fatalf("no warning about the address: %+v", notes)
	}
	// Unset: the injected volume is ignored with the warning, as before.
	spec, notes, err = TranslateWithOptions(apiInjectedPod(nil), TranslateOptions{Volumes: volumeSettings{root: root}, APIServer: "192.0.2.80:6443"})
	if err != nil || len(spec.Mounts) != 0 || len(notes) != 1 || notes[0].Reason != NoteServiceAccountTokenNotMounted {
		t.Fatalf("unset: %+v %+v %v", spec.Mounts, notes, err)
	}
	// Native: still ignored with the warning (a native program has no volumes).
	nat := apiInjectedPod(&yes)
	nat.Labels[ExecutorLabel] = "native"
	if spec, notes, err := TranslateWithOptions(nat, o); err != nil || len(spec.Mounts) != 0 || len(notes) == 0 {
		t.Fatalf("native: %+v %+v %v", spec.Mounts, notes, err)
	}
}
