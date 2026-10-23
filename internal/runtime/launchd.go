package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The launchd backend runs a native program as a launchd job in the user's GUI domain instead of as a child of
// the agent. The program then belongs to launchd, not to the agent: it keeps the user's login session (so Metal
// works) and does not inherit the agent's identity or environment. The agent still decides what runs; it uses
// launchctl to start, signal and inspect the job, and keeps following the same log file and the same
// pid-based exit watching as for a process it adopted. The launchd job is never restarted by launchd
// (KeepAlive is off): restart policy and backoff stay in the agent.

// Options configures an engine.
type Options struct {
	// NativeBackend is "process" (default: the agent starts native programs itself) or "launchd".
	NativeBackend string
	// LaunchdUID is the user whose GUI domain runs the jobs; 0 means the agent's own user.
	LaunchdUID int
	// Launcher is the executable that starts jobs (it must run RunLauncher for "__launch"); default: this binary.
	Launcher string
}

const (
	backendProcess = "process"
	backendLaunchd = "launchd"
	launchdPrefix  = "com.maccluster.pod."
)

// ValidNativeBackend reports whether name selects a known native backend ("" is the default).
func ValidNativeBackend(name string) bool {
	return name == "" || name == backendProcess || name == backendLaunchd
}

// NewWithOptions is NewWithEnvironments with the backend choice.
func NewWithOptions(stateDir, envDir string, o Options) (Engine, error) {
	if !ValidNativeBackend(o.NativeBackend) {
		return nil, fmt.Errorf("unknown native backend %q", o.NativeBackend)
	}
	if o.NativeBackend == backendLaunchd {
		if err := checkLaunchdAvailable(o); err != nil {
			return nil, err
		}
	}
	eng, err := newEngine(stateDir, o)
	if err != nil {
		return nil, err
	}
	e := eng.(*engine)
	e.envDir = envDir
	if e.backend == backendLaunchd {
		e.sweepLaunchd()
	}
	return e, nil
}

func (e *engine) useLaunchd() bool { return e.backend == backendLaunchd }

func (e *engine) domain() string {
	uid := e.launchUID
	if uid == 0 {
		uid = os.Getuid()
	}
	return "gui/" + strconv.Itoa(uid)
}

// ownerTag separates the jobs of different state directories (a test worker and the real agent share one user).
func (e *engine) ownerTag() string { return key(e.dir)[:8] }

func (e *engine) launchLabel(uid string) string {
	return launchdPrefix + e.ownerTag() + "." + key(uid)[:16]
}

var launchctlBin = "launchctl"

func launchctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	c := exec.CommandContext(ctx, launchctlBin, args...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("launchctl %s: %w: %.512s", args[0], err, strings.TrimSpace(errb.String()+out.String()))
	}
	return out.String(), nil
}

func checkLaunchdAvailable(o Options) error {
	uid := o.LaunchdUID
	if uid == 0 {
		uid = os.Getuid()
	}
	if uid == 0 {
		return errors.New("launchd backend: set launchdUID to the login user when the agent runs as root")
	}
	if _, err := launchctl(context.Background(), "print", "gui/"+strconv.Itoa(uid)); err != nil {
		return fmt.Errorf("launchd backend: no GUI session for uid %d (the user must be logged in): %w", uid, err)
	}
	return nil
}

// launchdState is what `launchctl print <service>` says about a job.
type launchdState struct {
	Running  bool
	PID      int
	ExitCode *int   // set once the job has exited normally
	Signal   string // set when the last run was ended by a signal, e.g. "Killed: 9"
}

// parseLaunchdPrint reads the top-level fields of `launchctl print`. Only lines indented by exactly one tab are
// used: nested blocks repeat names such as "state".
func parseLaunchdPrint(out string) launchdState {
	var st launchdState
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "\t"), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			st.Running = v == "running"
		case "pid":
			st.PID, _ = strconv.Atoi(v)
		case "last exit code":
			if n, err := strconv.Atoi(v); err == nil {
				st.ExitCode = &n
			}
		case "last terminating signal":
			st.Signal = v
		}
	}
	return st
}

func (e *engine) launchdStatus(ctx context.Context, label string) (launchdState, error) {
	out, err := launchctl(ctx, "print", e.domain()+"/"+label)
	if err != nil {
		return launchdState{}, err
	}
	return parseLaunchdPrint(out), nil
}

// launchdExit is the exit of a job, delivered on an entry's done channel.
type launchdExit struct {
	Code    int
	Signal  string
	Unknown bool
}

func (x *launchdExit) Error() string {
	if x.Unknown {
		return "launchd job ended with unknown status"
	}
	if x.Signal != "" {
		return "launchd job terminated by signal: " + x.Signal
	}
	return "launchd job exited with code " + strconv.Itoa(x.Code)
}

// waitLaunchdExit reads how a job ended (the record appears a moment after the process is gone) and then
// removes the job, so the label can be used again.
func (e *engine) waitLaunchdExit(label string) *launchdExit {
	defer launchctl(context.Background(), "bootout", e.domain()+"/"+label)
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := e.launchdStatus(context.Background(), label)
		if err != nil {
			return &launchdExit{Unknown: true, Code: 1}
		}
		if !st.Running {
			switch {
			case st.Signal != "":
				return &launchdExit{Signal: st.Signal, Code: 1}
			case st.ExitCode != nil:
				return &launchdExit{Code: *st.ExitCode}
			}
		}
		if time.Now().After(deadline) {
			return &launchdExit{Unknown: true, Code: 1}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func xmlText(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// launchdPlist is the job definition. It carries no environment: launchd cuts values at the first newline (a
// PEM certificate or a multi-line secret would be truncated) and `launchctl print` shows them. The environment
// travels in a private file that the launcher reads, deletes and execs with (see RunLauncher).
func launchdPlist(label, program string, args []string, dir, logPath string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n<plist version=\"1.0\"><dict>\n")
	str := func(k, v string) { fmt.Fprintf(&b, "<key>%s</key><string>%s</string>\n", k, xmlText(v)) }
	str("Label", label)
	b.WriteString("<key>ProgramArguments</key><array>")
	for _, a := range append([]string{program}, args...) {
		fmt.Fprintf(&b, "<string>%s</string>", xmlText(a))
	}
	b.WriteString("</array>\n")
	if dir != "" {
		str("WorkingDirectory", dir)
	}
	str("StandardOutPath", logPath)
	str("StandardErrorPath", logPath)
	b.WriteString("<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><false/>\n<key>ExitTimeOut</key><integer>5</integer>\n</dict></plist>\n")
	return []byte(b.String())
}

// startLaunchd is the launchd counterpart of starting and watching a child. The marker has been written as
// intent by the caller.
func (e *engine) startLaunchd(ctx context.Context, r *entry, launch resolved, args, env []string, w *boundedWriter, marker nativeMarker) error {
	s := r.spec
	label := e.launchLabel(s.UID)
	domain := e.domain()
	fail := func(err error) error {
		w.Close()
		os.Remove(r.marker)
		return err
	}
	spawned := time.Now()
	launchctl(ctx, "bootout", domain+"/"+label) // a leftover of an earlier run of this UID
	jobs := filepath.Join(e.dir, "jobs")
	if err := os.MkdirAll(jobs, 0700); err != nil {
		return fail(err)
	}
	envData, err := json.Marshal(env)
	if err != nil {
		return fail(err)
	}
	envFile := filepath.Join(jobs, key(s.UID)+".env")
	if err = os.WriteFile(envFile, envData, 0600); err != nil {
		return fail(err)
	}
	plist := filepath.Join(jobs, key(s.UID)+".plist")
	launcher := e.launcherPath()
	argv := append([]string{"__launch", envFile, launch.command}, args...)
	if err = os.WriteFile(plist, launchdPlist(label, launcher, argv, launch.dir, w.file.Name()), 0600); err != nil {
		os.Remove(envFile)
		return fail(err)
	}
	_, err = launchctl(ctx, "bootstrap", domain, plist)
	os.Remove(plist)
	if err != nil {
		os.Remove(envFile)
		return fail(err)
	}
	// Wait for the job to have a process (or to have already ended: a very short program).
	var st launchdState
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, err = e.launchdStatus(ctx, label); err != nil {
			launchctl(context.Background(), "bootout", domain+"/"+label)
			os.Remove(envFile)
			return fail(err)
		}
		if st.PID > 0 || st.ExitCode != nil || st.Signal != "" {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			launchctl(context.Background(), "bootout", domain+"/"+label)
			os.Remove(envFile)
			return fail(errors.New("launchd job did not start"))
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.launchLabel = label
	r.log, r.logWriter = w.file, w
	r.done = make(chan error, 1)
	r.gone, r.goneOnce = make(chan struct{}), &sync.Once{}
	done, gone, goneOnce := r.done, r.gone, r.goneOnce
	r.exited, r.cmd = false, nil
	r.adoptPID = st.PID
	r.instance.PID = st.PID
	r.instance.Listening = nil
	r.instance.Phase, r.instance.Message, r.instance.ExitCode = "Running", "", 0
	r.instance.StartedAt = marker.StartedAt
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			x := e.waitLaunchdExit(label)
			w.finish()
			done <- x
			goneOnce.Do(func() { close(gone) })
		})
	}
	marker.Intent, marker.PID, marker.Label = false, st.PID, label
	if st.PID > 0 {
		if start, command, perr := processIdentity(st.PID); perr == nil {
			marker.Start, marker.Command = start, command
		}
		writeMarker(r.marker, marker)
		stop, werr := watchExit(st.PID, func() { finish() })
		if werr != nil {
			launchctl(context.Background(), "bootout", domain+"/"+label)
			w.Close()
			os.Remove(r.marker)
			r.logWriter, r.log, r.adoptPID, r.launchLabel = nil, nil, 0, ""
			return werr
		}
		r.stopWatch = stop
		// A program that died between the first look and the watch (an exit kqueue cannot report for a process
		// that is already gone or not yet reaped) is found by asking launchd once more, now that the watch is set.
		if again, serr := e.launchdStatus(ctx, label); serr == nil && !again.Running {
			go finish()
		}
	} else {
		// Already gone: nothing to watch, only to report.
		os.Remove(r.marker)
		r.adoptPID = 0
		go finish()
	}
	go w.tail()
	if st.PID > 0 {
		startRedirectFollower(st.PID, w, gone, spawned)
		declared := r.instance.Ports
		if len(declared) == 0 && r.instance.Port > 0 {
			declared = []int{r.instance.Port}
		}
		e.startListenerDiscovery(r, st.PID, declared, gone)
	}
	return nil
}

// bootoutLaunchd removes the job of an entry, if it has one.
func (e *engine) bootoutLaunchd(r *entry) {
	if r.launchLabel != "" {
		launchctl(context.Background(), "bootout", e.domain()+"/"+r.launchLabel)
	}
}

// sweepLaunchd removes jobs of this state directory that no marker accounts for: a crash between loading a job
// and writing its marker would otherwise leave a program nobody tracks. Jobs of other state directories are
// not touched. Adopted markers are processed first (New), so their labels are known here.
func (e *engine) sweepLaunchd() {
	// Environment files are deleted by the launcher; an old one left behind belongs to a job that never started
	// (a young one may be a job the previous agent started a moment ago).
	stale, _ := filepath.Glob(filepath.Join(e.dir, "jobs", "*"))
	for _, f := range stale {
		if st, err := os.Stat(f); err == nil && time.Since(st.ModTime()) > time.Minute {
			os.Remove(f)
		}
	}
	out, err := launchctl(context.Background(), "list")
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, r := range e.entries {
		if r.launchLabel != "" {
			known[r.launchLabel] = true
		}
	}
	prefix := launchdPrefix + e.ownerTag() + "."
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && strings.HasPrefix(f[2], prefix) && !known[f[2]] {
			launchctl(context.Background(), "bootout", e.domain()+"/"+f[2])
		}
	}
}

func (e *engine) launcherPath() string {
	if e.launcher != "" {
		return e.launcher
	}
	if p, err := os.Executable(); err == nil {
		return p
	}
	return os.Args[0]
}

// RunLauncher is the body of the "__launch" command that a launchd job runs: it reads the environment from the
// private file (a JSON array of NAME=value), deletes the file, and replaces itself with the program. args are
// the file, the program and its arguments. It returns only on failure.
func RunLauncher(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "launcher: usage: __launch ENVFILE PROGRAM [ARG...]")
		return 2
	}
	data, err := os.ReadFile(args[0])
	os.Remove(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "launcher:", err)
		return 126
	}
	var env []string
	if err = json.Unmarshal(data, &env); err != nil {
		fmt.Fprintln(os.Stderr, "launcher: bad environment file:", err)
		return 126
	}
	err = syscall.Exec(args[1], append([]string{args[1]}, args[2:]...), env)
	fmt.Fprintln(os.Stderr, "launcher:", err)
	return 127
}
