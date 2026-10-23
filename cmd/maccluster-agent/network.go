package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"maccluster/internal/cluster"
	"maccluster/internal/power"
	runtime "maccluster/internal/runtime"
	"maccluster/internal/servicenet"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// controllerConfig is how the cluster-side Service controller was asked to run.
type controllerConfig struct {
	kubeconfig string
	namespaces []string
	elect      bool
	election   servicenet.ElectionConfig
	healthAddr string
	interval   time.Duration
}

// parseControllerArgs reads the flags of `network-controller`. env is how a Pod tells the process who it is
// (POD_NAME, POD_NAMESPACE from the downward API); readFile reads the in-cluster namespace file.
func parseControllerArgs(args []string, env func(string) string, readFile func(string) ([]byte, error), hostname func() (string, error)) (controllerConfig, error) {
	f := flag.NewFlagSet("network-controller", flag.ContinueOnError)
	cfg := controllerConfig{election: servicenet.DefaultElection(), interval: 3 * time.Second}
	namespaces := f.String("namespaces", "", "explicit comma-separated namespace allowlist (required)")
	f.StringVar(&cfg.kubeconfig, "kubeconfig", "", "kubeconfig, or in-cluster credentials when omitted")
	f.BoolVar(&cfg.elect, "leader-elect", true, "only the replica that holds the Lease reconciles")
	f.StringVar(&cfg.election.Namespace, "lease-namespace", "", "namespace of the leader Lease (default: this Pod's namespace)")
	f.StringVar(&cfg.election.Name, "lease-name", cfg.election.Name, "name of the leader Lease")
	f.StringVar(&cfg.election.Identity, "identity", "", "this replica's identity (default: POD_NAME, then the host name)")
	f.DurationVar(&cfg.election.LeaseDuration, "lease-duration", cfg.election.LeaseDuration, "how long a Lease is valid without renewal")
	f.DurationVar(&cfg.election.RenewDeadline, "renew-deadline", cfg.election.RenewDeadline, "how long the leader keeps trying to renew")
	f.DurationVar(&cfg.election.RetryPeriod, "retry-period", cfg.election.RetryPeriod, "how often a replica tries to take or renew the Lease")
	f.StringVar(&cfg.healthAddr, "health-addr", "", "address of the /healthz and /leader endpoint, for example :8081 (off by default)")
	f.DurationVar(&cfg.interval, "interval", cfg.interval, "how often the leader reconciles")
	if err := f.Parse(args); err != nil {
		return cfg, err
	}
	if *namespaces == "" {
		return cfg, fmt.Errorf("--namespaces is required")
	}
	for _, ns := range strings.Split(*namespaces, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			return cfg, fmt.Errorf("--namespaces has an empty entry")
		}
		cfg.namespaces = append(cfg.namespaces, ns)
	}
	if cfg.interval < time.Second {
		return cfg, fmt.Errorf("--interval must be at least one second")
	}
	if !cfg.elect {
		return cfg, nil
	}
	if cfg.election.Identity == "" {
		cfg.election.Identity = env("POD_NAME")
		if cfg.election.Identity == "" {
			if h, err := hostname(); err == nil {
				cfg.election.Identity = h
			}
		}
	}
	if cfg.election.Namespace == "" {
		cfg.election.Namespace = env("POD_NAMESPACE")
		if cfg.election.Namespace == "" {
			if b, err := readFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
				cfg.election.Namespace = strings.TrimSpace(string(b))
			}
		}
	}
	if err := cfg.election.Validate(); err != nil {
		return cfg, fmt.Errorf("%w (set --lease-namespace and --identity, or run with --leader-elect=false)", err)
	}
	return cfg, nil
}

func networkController(args []string) error {
	cfg, err := parseControllerArgs(args, os.Getenv, os.ReadFile, os.Hostname)
	if err != nil {
		return err
	}
	var rc *rest.Config
	if cfg.kubeconfig == "" {
		rc, err = rest.InClusterConfig()
	} else {
		rc, err = clientcmd.BuildConfigFromFlags("", cfg.kubeconfig)
	}
	if err != nil {
		return err
	}
	rc.Timeout = 10 * time.Second
	if cfg.elect && cfg.election.RenewDeadline/2 < rc.Timeout {
		rc.Timeout = cfg.election.RenewDeadline / 2 // one slow renewal must not use up the whole renew deadline
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	c := &servicenet.Controller{Client: client, Namespaces: cfg.namespaces}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	health := servicenet.NewHealth(30 * time.Second)
	if cfg.healthAddr != "" {
		srv := &http.Server{Addr: cfg.healthAddr, Handler: health.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("health endpoint: %v", err)
			}
		}()
		defer srv.Close()
	}
	work := servicenet.ReconcileLoop(c, health, cfg.interval, logf)
	if !cfg.elect {
		go func() {
			for ctx.Err() == nil {
				health.Beat()
				time.Sleep(time.Second)
			}
		}()
		health.SetLeader(true)
		work(ctx)
		return nil
	}
	return servicenet.RunElected(ctx, client, cfg.election, health, work, logf)
}

// runNode is a foreground worker with an isolated state directory, useful for
// validating upgrades before replacing a live background agent.
func runNode(args []string) error {
	f := flag.NewFlagSet("run-node", flag.ContinueOnError)
	config := f.String("kubeconfig", "", "node kubeconfig")
	dir := f.String("state-dir", "", "isolated worker state directory")
	name := f.String("node-name", "", "Kubernetes node name")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *config == "" || *dir == "" || *name == "" {
		return fmt.Errorf("kubeconfig, state-dir and node-name required")
	}
	if err := cluster.ValidateNodeName(*name); err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", *config)
	if err != nil {
		return err
	}
	options, err := cluster.LoadNodeOptions(*dir)
	if err != nil {
		return err
	}
	engine, err := runtime.NewWithOptions(filepath.Join(*dir, "runtime"), "", runtime.Options{NativeBackend: options.NativeBackend, LaunchdUID: options.LaunchdUID})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Stopping the node leaves its workloads running; the next run takes them over.
		if r, ok := engine.(runtime.Releaser); ok {
			r.Release(ctx)
		} else {
			engine.Close(ctx)
		}
	}()
	id, err := loadInstallationID(*dir)
	if err != nil {
		return err
	}
	uid, _ := os.ReadFile(filepath.Join(*dir, "node-uid"))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if !power.ValidMode(options.KeepAwake) {
		return fmt.Errorf("keepAwake must be when-workloads, always or never")
	}
	go newKeepAwakeLoop(options.KeepAwake).Run(ctx, func(c context.Context) int { return running(c, engine) })
	return cluster.Run(ctx, cfg, *name, id, engine, func(phase, message string) { fmt.Println(phase, message) }, cluster.NodeIdentity{UID: string(uid), Options: options, Save: func(uid string) error { return os.WriteFile(filepath.Join(*dir, "node-uid"), []byte(uid), 0600) }})
}

// networkControllerManifests prints the cluster objects that install the controller (docs/service-controller-plan.md).
// It only prints; applying them is a separate, deliberate step.
func networkControllerManifests(args []string) error {
	f := flag.NewFlagSet("network-controller-manifests", flag.ContinueOnError)
	o := servicenet.ManifestOptions{Replicas: 2}
	namespaces := f.String("namespaces", "", "comma-separated namespaces the controller may manage (required)")
	f.StringVar(&o.Namespace, "namespace", "maccluster-system", "namespace the controller runs in")
	f.StringVar(&o.Image, "image", "", "controller image, pinned by digest (name@sha256:...)")
	f.BoolVar(&o.AllowTag, "allow-tag", false, "accept an image that is not pinned by digest")
	replicas := f.Int("replicas", 2, "number of replicas (1 to 5)")
	if err := f.Parse(args); err != nil {
		return err
	}
	o.Replicas = int32(*replicas)
	for _, ns := range strings.Split(*namespaces, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			o.ManagedNamespaces = append(o.ManagedNamespaces, ns)
		}
	}
	out, err := servicenet.ManifestsYAML(o)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(os.Stdout, out)
	return err
}
