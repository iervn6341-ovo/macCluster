package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	authz "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"maccluster/internal/runtime"
	"os/exec"
)

// streamEngine runs commands on the host, like a native workload, and reports resource use.
type streamEngine struct {
	kubeletTestEngine
	attachLogs io.ReadCloser
	usage      []runtime.Usage
}

func (e *streamEngine) Command(ctx context.Context, uid string, argv []string, interactive, tty bool) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if tty {
		cmd.SysProcAttr = ttyAttr()
	}
	return cmd, nil
}
func (e *streamEngine) AttachCommand(context.Context, string, bool) (*exec.Cmd, error) {
	return nil, runtime.ErrAttachViaLogs
}
func (e *streamEngine) StreamLogs(context.Context, string, runtime.LogOptions) (io.ReadCloser, error) {
	return e.attachLogs, nil
}
func (e *streamEngine) Usage(context.Context) []runtime.Usage { return e.usage }

func streamFixture(t *testing.T) (*httptest.Server, *streamEngine) {
	t.Helper()
	h, _, engine, _ := kubeletFixture(t)
	se := &streamEngine{kubeletTestEngine: *engine}
	h.engine = se
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.TLS = kubeletRequest("/", http.MethodGet).TLS
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, se
}

func execURL(server *httptest.Server, argv []string, stdin, tty bool) *url.URL {
	q := url.Values{"command": argv, "output": {"1"}}
	if !tty {
		q.Set("error", "1")
	}
	if stdin {
		q.Set("input", "1")
	}
	if tty {
		q.Set("tty", "1")
	}
	u, _ := url.Parse(server.URL + "/exec/test/test/server?" + q.Encode())
	return u
}

type fixedSize struct{ sent bool }

func (f *fixedSize) Next() *remotecommand.TerminalSize {
	if f.sent {
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	f.sent = true
	return &remotecommand.TerminalSize{Width: 100, Height: 30}
}

func TestKubeletExecRunsACommandAndReportsItsExitCode(t *testing.T) {
	server, _ := streamFixture(t)
	exe, err := remotecommand.NewSPDYExecutor(&rest.Config{Host: server.URL}, http.MethodPost, execURL(server, []string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"}, false, false))
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err = exe.StreamWithContext(context.Background(), remotecommand.StreamOptions{Stdout: &out, Stderr: &errOut})
	var exit utilexec.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 {
		t.Fatalf("kubectl must learn the exit code: %v", err)
	}
	if out.String() != "out\n" || errOut.String() != "err\n" {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
}

func TestKubeletExecCarriesStdinAndSupportsCp(t *testing.T) {
	server, _ := streamFixture(t)
	exe, err := remotecommand.NewSPDYExecutor(&rest.Config{Host: server.URL}, http.MethodPost, execURL(server, []string{"/bin/sh", "-c", "cat; echo done"}, true, false))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := exe.StreamWithContext(context.Background(), remotecommand.StreamOptions{Stdin: strings.NewReader("hello\n"), Stdout: &out, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\ndone\n" {
		t.Fatalf("got %q", out.String())
	}
	// `kubectl cp` is a tar stream through exec: send a file in, read it back out.
	dir := t.TempDir()
	var tarIn bytes.Buffer
	if err := runTar(&tarIn, dir, "payload.txt", "tar-through-exec"); err != nil {
		t.Skip("tar is not available: " + err.Error())
	}
	extract := []string{"/usr/bin/tar", "xmf", "-", "-C", dir}
	exe, _ = remotecommand.NewSPDYExecutor(&rest.Config{Host: server.URL}, http.MethodPost, execURL(server, extract, true, false))
	if err := exe.StreamWithContext(context.Background(), remotecommand.StreamOptions{Stdin: &tarIn, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, dir+"/payload.txt"); got != "tar-through-exec" {
		t.Fatalf("copied %q", got)
	}
}

func TestKubeletExecWithATerminalIsATerminalOfTheRequestedSize(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("pseudo-terminals are implemented for macOS")
	}
	server, _ := streamFixture(t)
	exe, err := remotecommand.NewSPDYExecutor(&rest.Config{Host: server.URL}, http.MethodPost, execURL(server, []string{"/bin/sh", "-c", "[ -t 0 ] && echo is-a-tty; sleep 0.5; stty size"}, true, true))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = exe.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: strings.NewReader(""), Stdout: &out, Tty: true, TerminalSizeQueue: &fixedSize{}})
	if err != nil {
		t.Fatalf("%v (output %q)", err, out.String())
	}
	got := strings.ReplaceAll(out.String(), "\r", "")
	if !strings.Contains(got, "is-a-tty") || !strings.Contains(got, "30 100") {
		t.Fatalf("a TTY session must see a terminal and its size: %q", got)
	}
}

func TestKubeletAttachToNativeFollowsOutputAndEndsWithIt(t *testing.T) {
	server, engine := streamFixture(t)
	pr, pw := io.Pipe()
	engine.attachLogs = pr
	u, _ := url.Parse(server.URL + "/attach/test/test/server?output=1&error=1")
	exe, err := remotecommand.NewSPDYExecutor(&rest.Config{Host: server.URL}, http.MethodPost, u)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		pw.Write([]byte("line one\n"))
		time.Sleep(100 * time.Millisecond)
		pw.Write([]byte("line two\n"))
		pw.Close()
	}()
	var out bytes.Buffer
	if err := exe.StreamWithContext(context.Background(), remotecommand.StreamOptions{Stdout: &out, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "line one\nline two\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestKubeletNodeEndpointsAreAuthorizedAsTheRightSubresource(t *testing.T) {
	for _, tc := range []struct{ path, sub string }{{"/healthz", "proxy"}, {"/pods", "proxy"}, {"/stats/summary", "stats"}, {"/stats/summary/", "stats"}, {"/metrics/resource", "metrics"}} {
		t.Run(tc.path, func(t *testing.T) {
			h, client, _, _ := kubeletFixture(t)
			var spec authz.SubjectAccessReviewSpec
			client.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				spec = a.(k8stesting.CreateAction).GetObject().(*authz.SubjectAccessReview).Spec
				return true, &authz.SubjectAccessReview{Status: authz.SubjectAccessReviewStatus{Allowed: true}}, nil
			})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, kubeletRequest(tc.path, http.MethodGet))
			if spec.ResourceAttributes == nil || spec.ResourceAttributes.Subresource != tc.sub || spec.ResourceAttributes.Verb != "get" || spec.ResourceAttributes.Name != "node-test" {
				t.Fatalf("SAR: %+v", spec.ResourceAttributes)
			}
			if w.Code != http.StatusOK && tc.sub == "proxy" {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
		})
	}
	h, _, _, _ := kubeletFixture(t)
	for _, path := range []string{"/healthz", "/pods", "/stats/summary", "/metrics/resource"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, kubeletRequest(path, http.MethodPost))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: HTTP %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, kubeletRequest("/configz", http.MethodGet))
	if w.Code != http.StatusNotFound {
		t.Fatalf("what is not implemented stays a 404: %d", w.Code)
	}
}

func TestKubeletNodeEndpointsReportPodsAndResourceUse(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("host load is sampled on macOS")
	}
	h, _, engine, p := kubeletFixture(t)
	started := time.Now().Add(-time.Minute)
	se := &streamEngine{kubeletTestEngine: *engine, usage: []runtime.Usage{{UID: string(p.UID), At: time.Now(), CPUUsageNanoSeconds: 2_500_000_000, MemoryWorkingSetBytes: 64 << 20, StartedAt: started}}}
	h.engine = se

	w := httptest.NewRecorder()
	h.ServeHTTP(w, kubeletRequest("/stats/summary", http.MethodGet))
	if w.Code != http.StatusOK {
		t.Fatalf("summary: HTTP %d %s", w.Code, w.Body.String())
	}
	var summary struct {
		Node struct {
			NodeName    string
			CPU, Memory map[string]any
		}
		Pods []struct {
			PodRef     struct{ Name, Namespace, UID string }
			Containers []struct {
				Name   string
				CPU    struct{ UsageCoreNanoSeconds uint64 }
				Memory struct{ WorkingSetBytes uint64 }
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Node.NodeName != "node-test" || summary.Node.CPU == nil || summary.Node.Memory == nil {
		t.Fatalf("node stats: %+v", summary.Node)
	}
	if len(summary.Pods) != 1 || summary.Pods[0].PodRef.Name != "test" || summary.Pods[0].Containers[0].Name != "server" ||
		summary.Pods[0].Containers[0].CPU.UsageCoreNanoSeconds != 2_500_000_000 || summary.Pods[0].Containers[0].Memory.WorkingSetBytes != 64<<20 {
		t.Fatalf("pod stats: %+v", summary.Pods)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, kubeletRequest("/metrics/resource", http.MethodGet))
	text := w.Body.String()
	for _, want := range []string{
		"node_cpu_usage_seconds_total", "node_memory_working_set_bytes",
		`container_cpu_usage_seconds_total{container="server",namespace="test",pod="test"} 2.5`,
		`container_memory_working_set_bytes{container="server",namespace="test",pod="test"} 6.7108864e+07`,
		`pod_memory_working_set_bytes{namespace="test",pod="test"}`, "container_start_time_seconds", "resource_scrape_error 0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics/resource lacks %q:\n%s", want, text)
		}
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, kubeletRequest("/pods", http.MethodGet))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"test"`) {
		t.Fatalf("pods: HTTP %d %s", w.Code, w.Body.String())
	}
	_ = core.Pod{}
}
