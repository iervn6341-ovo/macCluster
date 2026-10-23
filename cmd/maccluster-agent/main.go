package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"maccluster/internal/envs"
	runtime "maccluster/internal/runtime"
)

// Set only for the bundled local-command tool; it cannot run a daemon.
var commandRole = "agent"

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if commandRole == "tools" {
		if len(os.Args) < 2 {
			return errors.New("local command required")
		}
		if err := validateToolCommand(os.Args[1:]); err != nil {
			return err
		}
	}
	if len(os.Args) < 2 || os.Args[1] == "--help" || os.Args[1] == "-h" || (os.Args[1] == "help" && len(os.Args) == 2) {
		fmt.Fprint(os.Stdout, cliHelp)
		return nil
	}
	if os.Args[1] == "help" {
		os.Args = append([]string{os.Args[0], os.Args[2]}, append(os.Args[3:], "--help")...)
	}
	for _, arg := range os.Args[2:] {
		if arg == "--help" || arg == "-h" {
			if writeCommandHelp(os.Stdout, os.Args[1]) {
				return nil
			}
			break
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "__launch" {
		os.Exit(runtime.RunLauncher(os.Args[2:]))
	}
	if os.Args[1] == "service" {
		return serviceCommand(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "run-node" {
		return runNode(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "egress-proxy" {
		return egressProxy(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "network-controller-manifests" {
		return networkControllerManifests(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "network-controller" {
		return networkController(os.Args[2:])
	}
	if len(os.Args) < 2 {
		return errors.New("usage: maccluster-agent serve|status|join|logs|create|delete|leave|configure|env|smoke [--state-dir PATH]")
	}
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	dir := flags.String("state-dir", filepath.Join(home, "Library", "Application Support", "MacCluster"), "private state directory")
	uid := flags.String("uid", "", "workload UID")
	clusterID := flags.String("cluster-id", "", "cluster profile ID; empty means local")
	envName := flags.String("name", "", "environment name (env subcommand)")
	envRoot := flags.String("root", "", "environment root directory, e.g. an existing virtualenv")
	envBin := flags.String("bin-dir", "", "environment bin directory (default ROOT/bin)")
	envWork := flags.String("workdir", "", "default working directory for the environment")
	envType := flags.String("type", envs.TypeLink, "environment type: link or dir")
	envVars := map[string]string{}
	flags.Func("set-env", "KEY=VALUE applied to workloads using the environment (repeatable)", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return errors.New("expected KEY=VALUE")
		}
		envVars[k] = val
		return nil
	})
	var extraPath []string
	flags.Func("extra-path", "additional PATH directory for the environment (repeatable)", func(v string) error {
		extraPath = append(extraPath, v)
		return nil
	})
	asJSON := flags.Bool("json", false, "machine-readable output (env list)")
	args := os.Args[2:]
	if os.Args[1] == "env" && len(args) > 0 {
		args = args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	var err error
	*dir, err = filepath.Abs(*dir)
	if err != nil {
		return err
	}
	// launchd does not inherit interactive shell PATH.
	os.Setenv("PATH", os.Getenv("PATH")+":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin")
	switch os.Args[1] {
	case "docker-inspect", "docker-stats", "docker-logs":
		var req struct {
			ContainerID string `json:"containerID"`
		}
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			return err
		}
		var out string
		var err error
		if os.Args[1] == "docker-logs" {
			out, err = runtime.LogsDocker(context.Background(), req.ContainerID)
		} else if os.Args[1] == "docker-stats" {
			out, err = runtime.StatsDocker(context.Background(), req.ContainerID)
		} else {
			out, err = runtime.InspectDocker(context.Background(), req.ContainerID)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(os.Stdout, out)
		return err
	case "docker-login", "docker-logout", "docker-logins":
		switch os.Args[1] {
		case "docker-logins":
			list, err := dockerLogins(dockerConfigPath())
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(list)
		case "docker-logout":
			var req struct {
				Registry string `json:"registry"`
			}
			if err := json.NewDecoder(io.LimitReader(os.Stdin, 4096)).Decode(&req); err != nil {
				return err
			}
			if err := dockerLogoutRun(context.Background(), req.Registry); err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(map[string]bool{"loggedOut": true})
		default:
			var req dockerLoginRequest
			decoder := json.NewDecoder(io.LimitReader(os.Stdin, 16384))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil {
				return fmt.Errorf("invalid request")
			}
			if err := dockerLoginRun(context.Background(), req); err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(map[string]bool{"loggedIn": true})
		}
	case "docker-control":
		var req struct {
			ContainerID string `json:"containerID"`
			Action      string `json:"action"`
		}
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			return err
		}
		if err := runtime.ControlExternalDocker(context.Background(), req.ContainerID, req.Action); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]bool{"completed": true})
	case "inventory":
		items, err := runtime.DockerInventory(context.Background())
		if err != nil {
			return err
		}
		if items == nil {
			items = []runtime.ExternalContainer{}
		}
		return json.NewEncoder(os.Stdout).Encode(items)
	case "env":
		if len(os.Args) < 3 {
			return errors.New("usage: maccluster-agent env add|list|shell|remove [--name N --root PATH ...]")
		}
		return envCommand(*dir, os.Args[2], *asJSON, envs.Env{Name: *envName, Type: *envType, Root: *envRoot, BinDir: *envBin, WorkDir: *envWork, ExtraPath: extraPath, Env: envVars})
	case "serve":
		return serve(*dir)
	case "smoke":
		return smoke(*dir)
	case "status", "join", "logs", "create", "delete", "leave", "configure", "drain", "settings", "netcheck":
		return client(*dir, os.Args[1], *uid, *clusterID)
	default:
		return fmt.Errorf("unknown subcommand")
	}
}
func client(dir, command, uid, clusterID string) error {
	socket := filepath.Join(dir, "agent.sock")
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 90 * time.Second}
	method := "GET"
	path := "/v1/" + command
	var body io.Reader
	if command == "join" || command == "create" || command == "delete" || command == "leave" || command == "configure" || command == "settings" {
		method = "POST"
		body = io.LimitReader(os.Stdin, 65537)
	}
	if command == "drain" {
		method = "POST" // no request body: it must not wait for standard input
	}
	if command == "logs" {
		path += "?uid=" + url.QueryEscape(uid) + "&clusterID=" + url.QueryEscape(clusterID)
	}
	req, _ := http.NewRequest(method, "http://localhost"+path, body)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("agent unavailable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(os.Stdout, io.LimitReader(res.Body, 2<<20))
	return err
}
func serve(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("agent is already running")
	}
	socket := filepath.Join(dir, "agent.sock")
	if len(socket) > 103 {
		return fmt.Errorf("state directory path too long for macOS Unix socket")
	}
	os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	installationID, err := loadInstallationID(dir)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	s, err := newServer(ctx, dir, installationID)
	if err != nil {
		return err
	}
	s.shutdown = cancel
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.getStatus)
	mux.HandleFunc("GET /v1/netcheck", s.netcheck)
	mux.HandleFunc("GET /v1/logs", s.logs)
	mux.HandleFunc("POST /v1/join", s.join)
	mux.HandleFunc("POST /v1/create", s.create)
	mux.HandleFunc("POST /v1/delete", s.delete)
	mux.HandleFunc("POST /v1/leave", s.leave)
	mux.HandleFunc("POST /v1/configure", s.configure)
	mux.HandleFunc("POST /v1/drain", s.drain)
	mux.HandleFunc("POST /v1/settings", s.settings)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 100 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err = <-done:
		cancel()
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownErr := srv.Shutdown(shutdown)
	stop()
	cleanup, cleanupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	cleanupErr := s.close(cleanup)
	cleanupCancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, shutdownErr, cleanupErr)
}

// smoke tests the exact runtime interface used by Kubernetes, without requiring
// admin privileges on a cluster. Its state is separate from the daemon.
func smoke(dir string) error {
	engine, err := runtime.New(filepath.Join(dir, "smoke-runtime"))
	if err != nil {
		return err
	}
	defer engine.Close(context.Background())
	var specs []runtime.Spec
	if err = json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&specs); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, spec := range specs {
		inst, e := engine.Ensure(ctx, spec)
		if e != nil {
			return e
		}
		json.NewEncoder(os.Stdout).Encode(inst)
	}
	for _, inst := range engine.List(ctx) {
		if inst.Port > 0 {
			var good bool
			for n := 0; n < 30; n++ {
				c := http.Client{Timeout: time.Second}
				res, e := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", inst.Port))
				if e == nil {
					res.Body.Close()
					good = res.StatusCode < 500
					if good {
						break
					}
				}
				time.Sleep(200 * time.Millisecond)
			}
			if !good {
				return fmt.Errorf("smoke HTTP failed for %s", inst.Name)
			}
		}
	}
	for _, inst := range engine.List(ctx) {
		if err = engine.Stop(ctx, inst.UID); err != nil {
			return err
		}
	}
	fmt.Println("smoke: start, HTTP and cleanup passed")
	return nil
}

func loadInstallationID(dir string) (string, error) {
	path := filepath.Join(dir, "installation-id")
	if b, err := os.ReadFile(path); err == nil {
		v := string(b)
		decoded, e := hex.DecodeString(v)
		if e != nil || len(decoded) != 16 {
			return "", fmt.Errorf("invalid installation identity")
		}
		return v, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	v := hex.EncodeToString(b)
	return v, os.WriteFile(path, []byte(v), 0600)
}

// envCommand edits the host-local environment registry directly. The agent
// re-reads it on each launch and Node label sync, so no API call is needed.
func envCommand(stateDir, action string, asJSON bool, e envs.Env) error {
	dir := filepath.Join(stateDir, "environments")
	switch action {
	case "add":
		if err := envs.Add(dir, e); err != nil {
			return err
		}
		fmt.Printf("environment %q registered\n", e.Name)
		return nil
	case "list":
		list, err := envs.ListAll(dir)
		if err != nil {
			return err
		}
		if asJSON {
			if list == nil {
				list = []envs.Status{} // "[]", not "null": a Mac with no environments yet must still decode as a list
			}
			return json.NewEncoder(os.Stdout).Encode(list)
		}
		for _, v := range list {
			state := "available"
			if !v.Available {
				state = "unavailable: " + v.Problem
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", v.Name, v.Type, v.Root, state)
		}
		return nil
	case "shell":
		// Prints how to start an interactive shell inside the environment; the
		// caller (the app's embedded terminal) owns the PTY.
		env, err := envs.Get(dir, e.Name)
		if err != nil {
			return err
		}
		spec, err := env.Shell(filepath.Join(stateDir, "shell"), os.Getenv("SHELL"), os.Environ())
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(spec)
	case "remove":
		// Running workloads keep their process; new launches of a removed
		// environment fail and the Pod stays Pending with a reason.
		return envs.Remove(dir, e.Name)
	}
	return fmt.Errorf("unknown env action %q", action)
}

// GUI local operations are separate from the long-running agent. API polling
// goes directly over the Unix socket and must never fall back to this tool.
func validateToolCommand(args []string) error {
	switch args[0] {
	case "env", "inventory", "docker-inspect", "docker-stats", "docker-logs", "docker-login", "docker-logout", "docker-logins", "docker-control", "service":
		return nil
	default:
		return fmt.Errorf("%s is not a local tool command", args[0])
	}
}
