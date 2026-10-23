package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"maccluster/internal/envs"
)

const logLimit = 1024 * 1024

type engine struct {
	mu              sync.Mutex
	dir             string
	lock            *os.File
	entries         map[string]*entry
	closed          bool
	dockerRecovered bool
	discoverAfter   time.Time
	envDir          string
	backend         string // native backend: ""/"process" or "launchd"
	launchUID       int
	launcher        string
}

var errNoEnvironments = errors.New("environments are not enabled for this runtime")

type entry struct {
	spec        Spec
	specUnknown bool
	instance    Instance
	cmd         *exec.Cmd
	done        chan error
	log         *os.File
	logWriter   *boundedWriter
	exited      bool
	retry       time.Time
	marker      string
	adoptPID    int    // a process taken over from an earlier agent run; not our child
	specHash    string // hash of the spec an adopted process was started from
	blocked     string // why this UID must not be started (unverifiable launch record)
	stopWatch   func()
	gone        chan struct{} // closed when the process has exited; lets a stop wait without holding the engine lock
	goneOnce    *sync.Once
	stopping    bool   // a graceful stop is in progress: do not restart
	killReason  string // why the next exit was caused by the agent (a failed probe), used as its reason
	execEnv     []string
	execDir     string
	launchLabel string // the launchd job of a native program started through the launchd backend
}

// New exclusively owns a state directory. Native programs left running by an earlier agent are taken
// over when their marker still identifies the live process (PID, start time and command line), and a
// launch record that cannot be verified blocks only that UID.
func New(stateDir string) (Engine, error) { return newEngine(stateDir, Options{}) }

func newEngine(stateDir string, o Options) (Engine, error) {
	dir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "engine.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("state directory already owned: %w", err)
	}
	e := &engine{dir: dir, lock: lock, entries: map[string]*entry{}, backend: o.NativeBackend, launchUID: o.LaunchdUID, launcher: o.Launcher}
	e.adoptMarkers()
	return e, nil
}

var (
	userPattern = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)
	capPattern  = regexp.MustCompile(`^(ALL|[A-Z][A-Z0-9_]*)$`)
)

func validateSecurity(sec *SecuritySpec) error {
	if sec.User != "" && !userPattern.MatchString(sec.User) {
		return fmt.Errorf("invalid user %q: use a numeric uid or uid:gid", sec.User)
	}
	for _, g := range sec.GroupAdd {
		if !regexp.MustCompile(`^[0-9]+$`).MatchString(g) {
			return fmt.Errorf("invalid supplemental group %q", g)
		}
	}
	for _, c := range append(append([]string{}, sec.CapAdd...), sec.CapDrop...) {
		if !capPattern.MatchString(c) {
			return fmt.Errorf("invalid capability %q", c)
		}
	}
	return nil
}

func key(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:16]) }
func validate(s *Spec) error {
	if s.UID == "" {
		return errors.New("UID is required")
	}
	if s.Executor == "" {
		s.Executor = "docker"
	}
	if s.Executor != "docker" && s.Executor != "native" {
		return fmt.Errorf("unsupported executor %q", s.Executor)
	}
	if s.RestartPolicy == "" {
		s.RestartPolicy = "Always"
	}
	if s.RestartPolicy != "Always" && s.RestartPolicy != "Never" && s.RestartPolicy != "OnFailure" {
		return errors.New("unsupported restart policy")
	}
	if s.ContainerPort < 0 || s.ContainerPort > 65535 {
		return errors.New("invalid container port")
	}
	if s.Limits != nil {
		if s.Executor != "docker" {
			return errors.New("hard limits are only enforced by the Docker executor")
		}
		if s.Limits.CPUMilli < 0 || s.Limits.MemoryBytes < 0 {
			return errors.New("invalid resource limits")
		}
	}
	if len(s.Mounts) > 0 {
		if s.Executor != "docker" {
			return errors.New("volumes are only supported by the Docker executor")
		}
		if err := validateMounts(s.Mounts); err != nil {
			return err
		}
	}
	if s.Security != nil {
		if s.Executor != "docker" {
			return errors.New("security options are only supported by the Docker executor")
		}
		if err := validateSecurity(s.Security); err != nil {
			return err
		}
	}
	if len(s.FixedPorts) > 0 {
		if s.Executor != "native" {
			return errors.New("fixed ports are only supported by the native executor")
		}
		if len(s.FixedPorts) > maxFixedPorts {
			return fmt.Errorf("at most %d fixed ports are supported", maxFixedPorts)
		}
		seen := map[int]bool{}
		for _, p := range s.FixedPorts {
			if p < 1 || p > 65535 || seen[p] {
				return fmt.Errorf("invalid or duplicate fixed port %d", p)
			}
			seen[p] = true
		}
	}
	if s.Environment != "" {
		if s.Executor != "native" {
			return errors.New("environment is only supported by the native executor")
		}
		if !envs.ValidName(s.Environment) {
			return errors.New("invalid environment name")
		}
	}
	if s.Executor == "native" {
		// A bare command name is looked up in the environment's bin directory.
		bare := s.Environment != "" && len(s.Command) > 0 && !strings.ContainsRune(s.Command[0], '/')
		if len(s.Command) == 0 || (!filepath.IsAbs(s.Command[0]) && !bare) {
			return errors.New("native command must be an absolute path (or a bare command name when an environment is set)")
		}
	}
	if s.Executor == "docker" && s.Image == "" {
		return errors.New("docker image is required")
	}
	return nil
}
func (e *engine) Ensure(ctx context.Context, s Spec) (Instance, error) {
	if err := validate(&s); err != nil {
		return Instance{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Instance{}, errors.New("engine closed")
	}
	r := e.entries[s.UID]
	if r == nil {
		// Fail before creating state so a missing environment leaves no history.
		if s.Environment != "" {
			if _, err := e.resolve(s); err != nil {
				return Instance{}, err
			}
		}
		r = &entry{spec: s, instance: Instance{UID: s.UID, Name: s.Name, Namespace: s.Namespace, Executor: s.Executor, Environment: s.Environment, Phase: "Pending"}}
		e.restoreHistory(r)
		e.entries[s.UID] = r
	} else if r.specUnknown && r.blocked != "" {
		return r.instance, errors.New(r.blocked)
	} else if r.specUnknown && r.spec.Executor == "native" {
		if s.Executor != "native" {
			return r.instance, errors.New("recovered native UID cannot change executor")
		}
		if r.specHash != "" && !fingerprintMatches(r.specHash, s) {
			// A Pod's spec is immutable under its UID, so a difference means the fingerprint format or the agent
			// changed, not that someone swapped the program. Taking the process over is what keeps an agent upgrade
			// from orphaning every running program; the difference is reported, not enforced.
			r.instance.AdoptNote = "the running process was started from a spec that differs from the current one (the agent was probably upgraded); it was taken over as it is"
		}
		r.specHash = ""
		r.spec = s
		r.specUnknown = false
		r.instance.Environment = s.Environment
		r.instance.Name = s.Name
		r.instance.Namespace = s.Namespace
	} else if r.specUnknown {
		if s.Executor != "docker" {
			return r.instance, errors.New("recovered Docker UID cannot change executor")
		}
		r.spec = s
		r.specUnknown = false
		r.instance.Environment = s.Environment
		r.instance.Name = s.Name
		r.instance.Namespace = s.Namespace
	} else {
		a, _ := json.Marshal(r.spec)
		b, _ := json.Marshal(s)
		if string(a) != string(b) {
			return r.instance, errors.New("UID already exists with a different immutable spec")
		}
	}
	e.refresh(ctx, r)
	if !r.stopping && (r.instance.Phase == "Pending" || (r.instance.Phase != "Unknown" && r.exited && shouldRestart(r) && !time.Now().Before(r.retry))) {
		restarted := r.exited
		if restarted {
			r.instance.Restarts++
		}
		if err := e.start(ctx, r); err != nil {
			var busy *portBusyError
			if errors.As(err, &busy) {
				// Waiting for a port is neither a crash nor a restart: stay Pending
				// and retry on the next reconcile without inflating counters.
				if restarted {
					r.instance.Restarts--
				}
				r.instance.Phase = "Pending"
				r.instance.Message = err.Error()
				return r.instance, err
			}
			r.instance.Phase = "Failed"
			if r.spec.Executor == "docker" {
				if _, probeErr := docker(ctx, "ps", "-aq"); probeErr != nil {
					r.instance.Phase = "Unknown"
				}
			}
			r.instance.Message = err.Error()
			r.instance.ExitCode = 1
			r.exited = true
			r.retry = time.Now().Add(backoff(r.instance.Restarts))
			return r.instance, err
		}
	}
	return r.instance, nil
}
func shouldRestart(r *entry) bool {
	return r.spec.RestartPolicy == "Always" || (r.spec.RestartPolicy == "OnFailure" && r.instance.ExitCode != 0)
}
func backoff(n int) time.Duration {
	if n > 5 {
		n = 5
	}
	return time.Second * time.Duration(1<<n)
}
func allocatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	p := l.Addr().(*net.TCPAddr).Port
	return p, l.Close()
}
func (e *engine) start(ctx context.Context, r *entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := r.spec
	if s.Executor == "docker" {
		return e.startDocker(ctx, r)
	}
	launch, err := e.resolve(s)
	if err != nil {
		return err
	}
	ports := s.FixedPorts
	if len(ports) > 0 {
		if err := e.checkFixedPorts(r, ports); err != nil {
			return err
		}
		r.instance.Ports = append([]int(nil), ports...)
	} else {
		p, err := allocatePort()
		if err != nil {
			return err
		}
		ports = []int{p}
		r.instance.Ports = nil
	}
	p := ports[0]
	r.instance.Port = p
	args := append(append([]string{}, s.Command[1:]...), s.Args...)
	expand := portExpander(ports)
	for i := range args {
		args[i] = expand(args[i])
	}
	cmd := exec.Command(launch.command, args...)
	cmd.WaitDelay = time.Second
	cmd.Dir = launch.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = nativeEnviron(s, launch, ports)
	r.execEnv, r.execDir = cmd.Env, launch.dir
	startNewRun(e.dir, s.UID)
	w, err := openLog(logFilePath(e.dir, s.UID))
	if err != nil {
		return err
	}
	// The program writes to the file itself: output is neither lost nor able to raise SIGPIPE while the
	// agent is away. The agent only follows the file (see boundedWriter.tail).
	cmd.Stdout = w.file
	cmd.Stderr = w.file
	r.marker = filepath.Join(e.dir, key(s.UID)+".native")
	marker := nativeMarker{UID: s.UID, Name: s.Name, Namespace: s.Namespace, Spec: specFingerprint(s), Port: p, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if len(r.instance.Ports) > 0 {
		marker.Ports = append([]int(nil), r.instance.Ports...)
	}
	// Persist intent before Start, so a crash cannot silently launch duplicates.
	intent := marker
	intent.Intent = true
	if err = writeMarker(r.marker, intent); err != nil {
		w.Close()
		return err
	}
	if e.useLaunchd() {
		return e.startLaunchd(ctx, r, launch, args, cmd.Env, w, marker)
	}
	spawned := time.Now()
	if err = cmd.Start(); err != nil {
		w.Close()
		os.Remove(r.marker)
		return err
	}
	r.cmd = cmd
	r.log = w.file
	r.logWriter = w
	r.done = make(chan error, 1)
	r.gone, r.goneOnce = make(chan struct{}), &sync.Once{}
	gone, goneOnce := r.gone, r.goneOnce
	r.exited = false
	r.adoptPID = 0
	r.instance.AdoptNote = ""
	r.instance.Listening = nil
	r.instance.PID = cmd.Process.Pid
	r.instance.Phase = "Running"
	r.instance.Message = ""
	r.instance.StartedAt = marker.StartedAt
	r.instance.ExitCode = 0
	marker.Intent, marker.PID = false, cmd.Process.Pid
	if start, command, perr := processIdentity(cmd.Process.Pid); perr == nil {
		marker.Start, marker.Command = start, command
	}
	writeMarker(r.marker, marker)
	done := r.done
	go func() {
		err := cmd.Wait()
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		w.finish()
		done <- err
		goneOnce.Do(func() { close(gone) })
	}()
	go w.tail()
	startRedirectFollower(cmd.Process.Pid, w, gone, spawned)
	e.startListenerDiscovery(r, cmd.Process.Pid, ports, gone)
	return nil
}

type boundedWriter struct {
	mu          sync.Mutex
	file        *os.File
	offset      int64 // how much of the file has been published to followers
	idx         *os.File
	idxSize     int64
	lastNote    time.Time
	closed      bool
	subscribers map[*nativeLogReader]struct{}
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	n := len(p)
	original := p
	if len(p) > logLimit {
		p = p[len(p)-logLimit:]
	}
	st, err := w.file.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size()+int64(len(p)) > logLimit {
		if err = w.file.Truncate(0); err != nil {
			return 0, err
		}
		if _, err = w.file.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
	}
	_, err = w.file.Write(p)
	if err != nil {
		return 0, err
	}
	// Direct writes are published right here, so the tailer must not publish them again.
	if st, serr := w.file.Stat(); serr == nil {
		w.offset = st.Size()
	}
	w.publishLocked(original)
	return n, nil
}
func docker(ctx context.Context, args ...string) (string, error) { return dockerEnv(ctx, nil, args...) }

// dockerEnv runs the docker CLI with extra environment variables, for `-e NAME` values that must not appear
// on the command line.
func dockerEnv(ctx context.Context, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "docker", args...)
	if len(env) > 0 {
		c.Env = append(os.Environ(), env...)
	}
	buf, stderr := &boundedBuffer{}, &boundedBuffer{}
	c.Stdout = buf
	c.Stderr = stderr
	// Docker logs uses both streams for workload output. Other commands must
	// parse stdout alone: pull progress and warnings are written to stderr.
	if len(args) > 0 && args[0] == "logs" {
		c.Stderr = buf
	}
	err := c.Run()
	out := buf.data
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %.2048s %.2048s", args[0], err, stderr.data, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// Docker output is capped even if a single log line is arbitrarily long.
type boundedBuffer struct{ data []byte }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= logLimit {
		b.data = append(b.data[:0], p[len(p)-logLimit:]...)
		return n, nil
	}
	if len(b.data)+len(p) > logLimit {
		drop := len(b.data) + len(p) - logLimit
		copy(b.data, b.data[drop:])
		b.data = b.data[:len(b.data)-drop]
	}
	b.data = append(b.data, p...)
	return n, nil
}

type dockerInfo struct {
	ID    string `json:"Id"`
	Image string `json:"Image"`
	State struct {
		Running   bool
		ExitCode  int
		StartedAt string
		Error     string
		OOMKilled bool
	}
	Config          struct{ Labels map[string]string }
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string
			HostPort string
		}
	}
}

func inspect(ctx context.Context, id string) (dockerInfo, error) {
	out, err := docker(ctx, "inspect", id)
	if err != nil {
		return dockerInfo{}, err
	}
	var v []dockerInfo
	err = json.Unmarshal([]byte(out), &v)
	if err != nil || len(v) != 1 {
		return dockerInfo{}, errors.New("invalid Docker inspect response")
	}
	return v[0], nil
}
func (e *engine) startDocker(ctx context.Context, r *entry) error {
	s := r.spec
	name := "maccluster-" + key(e.dir+s.UID)
	if r.instance.ContainerID != "" {
		if _, err := docker(ctx, "start", r.instance.ContainerID); err != nil {
			return err
		}
		d, err := inspect(ctx, r.instance.ContainerID)
		if err != nil {
			return err
		}
		r.exited = false // A new exit needs a new backoff deadline.
		r.instance.PreviousStartedAt = r.instance.StartedAt
		e.applyDocker(r, d)
		return nil
	}
	// Recover only the deterministically named container with matching ownership.
	found, err := docker(ctx, "ps", "-aq", "--filter", "name=^/"+name+"$")
	if err != nil {
		return err
	}
	if found != "" {
		d, err := inspect(ctx, found)
		if err != nil {
			return err
		}
		if d.Config.Labels["maccluster.uid"] != s.UID || d.Config.Labels["maccluster.owner"] != key(e.dir) {
			return errors.New("container ownership mismatch")
		}
		r.instance.ContainerID = d.ID
		e.applyDocker(r, d)
		return nil
	}
	args := []string{"create", "--name", name, "--label", "maccluster.uid=" + s.UID, "--label", "maccluster.owner=" + key(e.dir), "--label", "maccluster.name=" + s.Name, "--label", "maccluster.namespace=" + s.Namespace, "--log-opt", "max-size=1m", "--log-opt", "max-file=2"}
	if s.ContainerPort > 0 {
		args = append(args, "-p", "127.0.0.1::"+strconv.Itoa(s.ContainerPort))
	}
	if s.WorkingDir != "" {
		args = append(args, "-w", s.WorkingDir)
	}
	for k, v := range s.Env {
		args = append(args, "-e", k+"="+v)
	}
	// Values that came from the cluster (Secrets, ConfigMaps) are not put on the command line: `-e NAME` makes the
	// docker CLI take the value from its own environment. Nothing is written to disk, and values with newlines
	// (certificates, keys) work, which an --env-file cannot carry.
	var inherited []string
	names := make([]string, 0, len(s.EnvResolved))
	for k := range s.EnvResolved {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		args = append(args, "-e", k)
		inherited = append(inherited, k+"="+s.EnvResolved[k])
	}
	args = append(args, dockerMountArgs(s.Mounts)...)
	args = append(args, dockerLimitArgs(s.Limits)...)
	args = append(args, dockerSecurityArgs(s.Security)...)
	if len(s.Command) > 0 {
		args = append(args, "--entrypoint", s.Command[0])
	}
	args = append(args, s.Image)
	if len(s.Command) > 1 {
		args = append(args, s.Command[1:]...)
	}
	args = append(args, s.Args...)
	id, err := dockerEnv(ctx, inherited, args...)
	if err != nil {
		return err
	}
	r.instance.ContainerID = id
	if _, err = docker(ctx, "start", id); err != nil {
		return err
	}
	d, err := inspect(ctx, id)
	if err != nil {
		return err
	}
	r.exited = false
	e.applyDocker(r, d)
	return nil
}

// dockerLimitArgs maps hard limits to cgroup limits. Swap is capped at the memory limit, as for a Kubernetes container.
func dockerLimitArgs(l *ResourceSpec) []string {
	if l == nil {
		return nil
	}
	var args []string
	if l.CPUMilli > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(l.CPUMilli)/1000, 'f', 3, 64))
	}
	if l.MemoryBytes > 0 {
		args = append(args, "--memory", strconv.FormatInt(l.MemoryBytes, 10), "--memory-swap", strconv.FormatInt(l.MemoryBytes, 10))
	}
	return args
}

func dockerSecurityArgs(sec *SecuritySpec) []string {
	if sec == nil {
		return nil
	}
	var args []string
	if sec.User != "" {
		args = append(args, "--user", sec.User)
	}
	for _, g := range sec.GroupAdd {
		args = append(args, "--group-add", g)
	}
	if sec.ReadOnlyRootFS {
		args = append(args, "--read-only")
	}
	if sec.NoNewPrivileges {
		args = append(args, "--security-opt", "no-new-privileges")
	}
	for _, c := range sec.CapDrop {
		args = append(args, "--cap-drop", c)
	}
	for _, c := range sec.CapAdd {
		args = append(args, "--cap-add", c)
	}
	return args
}

func (e *engine) applyDocker(r *entry, d dockerInfo) {
	r.instance.ContainerID = d.ID
	r.instance.StartedAt = d.State.StartedAt
	r.instance.ExitCode = d.State.ExitCode
	if d.State.Error != "" {
		r.instance.Message = d.State.Error
	}
	if d.State.Running {
		r.instance.Message = ""
		r.instance.Phase = "Running"
		r.exited = false
	} else {
		reason := exitReason(d.State.ExitCode)
		if d.State.OOMKilled {
			reason = "OOMKilled"
		}
		e.markExit(r, d.State.ExitCode, reason)
	}
	r.instance.ImageID = d.Image
	var mappings []string
	for key, bindings := range d.NetworkSettings.Ports {
		container, proto := splitPort(key)
		for _, b := range bindings {
			if b.HostIP == "127.0.0.1" {
				r.instance.Port, _ = strconv.Atoi(b.HostPort)
			}
			if b.HostPort != "" {
				mappings = append(mappings, fmt.Sprintf("%s:%s->%d/%s", b.HostIP, b.HostPort, container, proto))
			}
		}
	}
	sort.Strings(mappings)
	if len(mappings) > 0 || d.State.Running {
		r.instance.PortMappings = strings.Join(mappings, ", ")
	}
}
func exitReason(code int) string {
	if code == 0 {
		return "Completed"
	}
	return "NonZeroExit"
}
func (e *engine) markExit(r *entry, code int, reason string) {
	if !r.exited && r.killReason != "" {
		reason, r.killReason = r.killReason, ""
	}
	if !r.exited {
		r.retry = time.Now().Add(backoff(r.instance.Restarts))
		r.instance.LastExitAt = time.Now().UTC().Format(time.RFC3339Nano)
		r.instance.LastExitCode = code
		r.instance.LastExitReason = reason
		if code != 0 {
			r.instance.FailureCount++
		}
		e.saveHistory(r)
	}
	r.exited = true
	r.instance.ExitCode = code
	r.instance.Phase = "Succeeded"
	if code != 0 {
		r.instance.Phase = "Failed"
	}
}
func (e *engine) refresh(ctx context.Context, r *entry) {
	if r.spec.Executor == "docker" {
		if r.instance.ContainerID == "" && r.instance.Phase == "Unknown" {
			if _, err := docker(ctx, "ps", "-aq"); err == nil {
				r.instance.Phase = "Failed"
			}
		}
		if r.instance.ContainerID != "" {
			d, err := inspect(ctx, r.instance.ContainerID)
			if err != nil {
				r.instance.Message = err.Error()
				if dockerMissing(ctx, r.instance.ContainerID) {
					e.markExit(r, -1, "ContainerRemoved")
					r.instance.Message = "Container removed outside macCluster"
					r.instance.ContainerID = ""
					r.instance.Port = 0
				} else {
					// Keep the ID so a subsequent inspect can recover.
					r.instance.Phase = "Unknown"
				}
				return
			}
			e.applyDocker(r, d)
		}
		return
	}
	if r.done != nil && !r.exited {
		select {
		case err := <-r.done:
			code := 0
			reason := "Completed"
			var lx *launchdExit
			if errors.As(err, &lx) {
				code, reason = lx.Code, "Completed"
				switch {
				case lx.Unknown:
					reason = "ExitedWithUnknownStatus"
				case lx.Signal != "":
					reason = "Signal: " + lx.Signal
				case lx.Code != 0:
					reason = "NonZeroExit"
				}
			} else if errors.Is(err, errDetachedExit) {
				code, reason = 1, "ExitedWithUnknownStatus"
			} else if err != nil {
				code = 1
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
					if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
						reason = "Signal: " + status.Signal().String()
					}
				}
				if reason == "Completed" {
					reason = "NonZeroExit"
				}
			}
			r.logWriter.finish()
			e.markExit(r, code, reason)
			r.instance.PID = 0
			r.adoptPID = 0
			os.Remove(r.marker) // Wait has also cleaned up the owned process group.
		default:
		}
	}
}
func (e *engine) Stop(ctx context.Context, uid string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.stop(ctx, uid); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(e.dir, key(uid)+".history.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (e *engine) stop(ctx context.Context, uid string) error {
	r := e.entries[uid]
	if r == nil {
		return nil
	}
	if r.spec.Executor == "docker" {
		if r.instance.ContainerID != "" {
			if _, err := docker(ctx, "rm", "-f", r.instance.ContainerID); err != nil && !dockerMissing(ctx, r.instance.ContainerID) {
				return err
			}
		}
	} else if r.blocked != "" {
		os.Remove(r.marker) // The operator is deleting this workload: the unverifiable record goes with it.
	} else if r.cmd != nil || r.adoptPID > 0 {
		if !r.exited {
			pgid := r.adoptPID
			if r.cmd != nil {
				pgid = r.cmd.Process.Pid
			}
			syscall.Kill(-pgid, syscall.SIGTERM)
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-r.done:
			case <-timer.C:
				syscall.Kill(-pgid, syscall.SIGKILL)
				<-r.done
			case <-ctx.Done():
				syscall.Kill(-pgid, syscall.SIGKILL)
				<-r.done
			}
			timer.Stop()
			r.logWriter.finish()
		}
		if r.stopWatch != nil {
			r.stopWatch()
		}
		os.Remove(r.marker)
	}
	e.bootoutLaunchd(r)
	delete(e.entries, uid)
	return nil
}

// recoverDocker permits reconciliation to delete workloads removed while the
// agent was down. A failed optional Docker probe never prevents native operation.
func (e *engine) recoverDocker(ctx context.Context) {
	if e.closed || e.dockerRecovered || time.Now().Before(e.discoverAfter) {
		return
	}
	e.discoverAfter = time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := docker(ctx, "ps", "-aq", "--filter", "label=maccluster.owner="+key(e.dir))
	if err != nil {
		return
	}
	complete := true
	for _, id := range strings.Fields(out) {
		d, err := inspect(ctx, id)
		if err != nil {
			complete = false
			continue
		}
		labels := d.Config.Labels
		uid := labels["maccluster.uid"]
		if uid == "" || d.ID == "" || labels["maccluster.owner"] != key(e.dir) {
			continue
		}
		if _, exists := e.entries[uid]; exists {
			continue
		}
		r := &entry{spec: Spec{UID: uid, Executor: "docker"}, specUnknown: true, instance: Instance{UID: uid, Executor: "docker", Name: labels["maccluster.name"], Namespace: labels["maccluster.namespace"]}}
		e.restoreHistory(r)
		e.applyDocker(r, d)
		e.entries[uid] = r
	}
	e.dockerRecovered = complete
}

func (e *engine) List(ctx context.Context) []Instance {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recoverDocker(ctx)
	out := make([]Instance, 0, len(e.entries))
	for _, r := range e.entries {
		e.refresh(ctx, r)
		out = append(out, r.instance)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}
func (e *engine) Logs(ctx context.Context, uid string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.entries[uid]
	if r != nil && r.spec.Executor == "docker" {
		out, err := docker(ctx, "logs", "--tail", "500", r.instance.ContainerID)
		if len(out) > logLimit {
			out = out[len(out)-logLimit:]
		}
		return out, err
	}
	// A native log is a file: it stays readable while the service is stopped, replaced or not yet
	// re-created after an agent restart, so only a UID with no file at all is unknown.
	data, err := readLogHistory(logFilePath(e.dir, uid))
	if err != nil {
		if r == nil && errors.Is(err, os.ErrNotExist) {
			return "", errors.New("unknown UID")
		}
		return "", err
	}
	// The app shows everything the program has said; only kubectl logs is limited to one run.
	if len(data) < logLimit {
		if prev, perr := readLogHistory(prevLogPath(e.dir, uid)); perr == nil && len(prev) > 0 {
			data = append(prev, data...)
			if len(data) > logLimit {
				data = data[len(data)-logLimit:]
			}
		}
	}
	return string(data), nil
}
func (e *engine) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	var errs []error
	for uid := range e.entries {
		if err := e.stop(ctx, uid); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 && e.lock != nil {
		e.lock.Close()
		e.lock = nil
	}
	return errors.Join(errs...)
}

// Release implements Releaser: the engine stops following its workloads and gives up the state directory, and
// every program and container keeps running. Programs keep writing to their log files, Docker keeps its containers,
// launchd keeps its jobs; the next engine over this state directory takes them over from the markers and labels.
func (e *engine) Release(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	for _, r := range e.entries {
		if r.stopWatch != nil {
			r.stopWatch()
		}
		if r.logWriter != nil {
			r.logWriter.finish()
		}
		if r.goneOnce != nil && r.gone != nil {
			r.goneOnce.Do(func() { close(r.gone) }) // ends the followers of the program's redirected output
		}
	}
	if e.lock != nil {
		e.lock.Close()
		e.lock = nil
	}
	return nil
}

func portExpander(ports []int) func(string) string {
	portList := joinPorts(ports)
	return func(v string) string {
		v = strings.ReplaceAll(v, "$(MACCLUSTER_PORTS)", portList)
		return strings.ReplaceAll(v, "$(MACCLUSTER_PORT)", strconv.Itoa(ports[0]))
	}
}

// nativeEnviron is the environment of a native program: the agent's own, the registered environment's, the
// Pod's, and the ports the program was given. Exec runs commands with the same one.
func nativeEnviron(s Spec, launch resolved, ports []int) []string {
	expand := portExpander(ports)
	env := os.Environ()
	for _, kv := range launch.env {
		env = append(env, expand(kv))
	}
	for _, m := range []map[string]string{s.Env, s.EnvResolved} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+expand(m[k]))
		}
	}
	return append(env, "MACCLUSTER_PORT="+strconv.Itoa(ports[0]), "MACCLUSTER_PORTS="+joinPorts(ports))
}

const execOutputLimit = 4096

type limitedOutput struct{ data []byte }

func (l *limitedOutput) Write(p []byte) (int, error) {
	if room := execOutputLimit - len(l.data); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.data = append(l.data, p[:room]...)
	}
	return len(p), nil
}

// Command implements Commander.
func (e *engine) Command(ctx context.Context, uid string, argv []string, interactive, tty bool) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.entries[uid]
	if r == nil || r.exited || r.instance.Phase != "Running" || e.closed {
		return nil, errors.New("the container is not running")
	}
	if r.spec.Executor == "docker" {
		args := []string{"exec"}
		if interactive {
			args = append(args, "-i")
		}
		if tty {
			args = append(args, "-t")
		}
		args = append(args, r.instance.ContainerID)
		return exec.CommandContext(ctx, "docker", append(args, argv...)...), nil
	}
	env, dir := r.execEnv, r.execDir
	if env == nil {
		// A program taken over from an earlier agent run: rebuild what it was started with.
		launch, err := e.resolve(r.spec)
		if err != nil {
			return nil, err
		}
		ports := r.instance.Ports
		if len(ports) == 0 {
			ports = []int{r.instance.Port}
		}
		env, dir = nativeEnviron(r.spec, launch, ports), launch.dir
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = env, dir
	if tty {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	}
	return cmd, nil
}

// AttachCommand implements Attacher.
func (e *engine) AttachCommand(ctx context.Context, uid string, stdin bool) (*exec.Cmd, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.entries[uid]
	if r == nil || r.exited || r.instance.Phase != "Running" || e.closed {
		return nil, errors.New("the container is not running")
	}
	if r.spec.Executor != "docker" {
		return nil, ErrAttachViaLogs
	}
	args := []string{"attach", "--sig-proxy=false"}
	if !stdin {
		args = append(args, "--no-stdin")
	}
	return exec.CommandContext(ctx, "docker", append(args, r.instance.ContainerID)...), nil
}

// Exec implements Execer.
func (e *engine) Exec(ctx context.Context, uid string, argv []string, timeout time.Duration) (ExecResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := e.Command(cctx, uid, argv, false, false)
	if err != nil {
		return ExecResult{ExitCode: -1}, err
	}
	out := &limitedOutput{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	res := ExecResult{Output: strings.TrimSpace(string(out.data))}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case cctx.Err() != nil:
		res.ExitCode = -1
		return res, fmt.Errorf("timed out after %s", timeout)
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

// signalGroup sends TERM to the process group, waits up to grace for the process to go away, then sends KILL.
func signalGroup(ctx context.Context, pgid int, gone <-chan struct{}, grace time.Duration) {
	syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-gone:
		return
	case <-timer.C:
	case <-ctx.Done():
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-gone:
	case <-time.After(3 * time.Second):
	}
}

func graceSeconds(grace time.Duration) string {
	n := int(grace.Round(time.Second) / time.Second)
	if n < 0 {
		n = 0
	}
	return strconv.Itoa(n)
}

// dockerLong runs docker with a timeout that fits a stop with a long grace period.
func dockerLong(ctx context.Context, timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).Run()
}

// Kill implements Killer.
func (e *engine) Kill(ctx context.Context, uid, reason string, grace time.Duration) error {
	e.mu.Lock()
	r := e.entries[uid]
	if r == nil || r.exited || r.instance.Phase != "Running" {
		e.mu.Unlock()
		return nil
	}
	r.killReason = reason
	executor, cid, gone := r.spec.Executor, r.instance.ContainerID, r.gone
	pgid := r.adoptPID
	if r.cmd != nil {
		pgid = r.cmd.Process.Pid
	}
	e.mu.Unlock()
	if executor == "docker" {
		if cid != "" {
			dockerLong(ctx, grace+20*time.Second, "stop", "-t", graceSeconds(grace), cid)
		}
		return nil
	}
	if pgid > 0 && gone != nil {
		signalGroup(ctx, pgid, gone, grace)
	}
	return nil
}

// StopGrace implements GraceStopper.
func (e *engine) StopGrace(ctx context.Context, uid string, grace time.Duration) error {
	e.mu.Lock()
	r := e.entries[uid]
	if r == nil {
		e.mu.Unlock()
		return nil
	}
	r.stopping = true
	executor, cid, gone, exited := r.spec.Executor, r.instance.ContainerID, r.gone, r.exited
	pgid := r.adoptPID
	if r.cmd != nil {
		pgid = r.cmd.Process.Pid
	}
	e.mu.Unlock()
	switch {
	case executor == "docker" && cid != "":
		dockerLong(ctx, grace+20*time.Second, "stop", "-t", graceSeconds(grace), cid)
	case executor == "native" && !exited && pgid > 0 && gone != nil:
		signalGroup(ctx, pgid, gone, grace)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stop(ctx, uid)
}
