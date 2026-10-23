package runtime

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// Spec is a validated, single-container Pod translated by the cluster adapter.
// Native commands must be absolute paths. $(MACCLUSTER_PORT) in arguments is
// expanded by the executor; no implicit shell evaluation is performed.
type Spec struct {
	UID           string            `json:"uid"`
	Name          string            `json:"name"`
	Namespace     string            `json:"namespace"`
	Executor      string            `json:"executor"`
	Image         string            `json:"image"`
	Command       []string          `json:"command"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	WorkingDir    string            `json:"workingDir"`
	Environment   string            `json:"environment,omitempty"`
	ContainerPort int               `json:"containerPort"`
	// FixedPorts are ports the program binds itself (native only). When set the
	// agent allocates nothing and exposes them as MACCLUSTER_PORT(S).
	FixedPorts    []int  `json:"fixedPorts,omitempty"`
	RestartPolicy string `json:"restartPolicy"`
	// EnvResolved holds environment values that came from the cluster (Secrets, ConfigMaps, fieldRef) or were
	// built from them. They are kept out of the JSON form, so they never take part in the spec hash that
	// identifies an adopted process and are never persisted; a Secret that changes while a program runs
	// cannot make the program look like a different workload.
	EnvResolved map[string]string `json:"-"`
	// Limits are hard limits, enforced by Docker only (cgroups). Native programs have none.
	Limits *ResourceSpec `json:"limits,omitempty"`
	// Security applies to Docker containers only.
	Security *SecuritySpec `json:"security,omitempty"`
	// Mounts are the Pod's volumes as resolved paths (Docker only; docs/archive/volumes-design.md). The cluster layer
	// writes the content, the engine only mounts it.
	Mounts []Mount `json:"mounts,omitempty"`
}

// Mount is one volume mounted into a Docker container: a directory or file of this Mac (bind), or a tmpfs.
type Mount struct {
	Name      string `json:"name"`                // the volume's name in the Pod, for messages
	HostPath  string `json:"hostPath,omitempty"`  // absolute path on this Mac (bind mounts)
	Container string `json:"container"`           // absolute path inside the container
	ReadOnly  bool   `json:"readOnly,omitempty"`  // enforced by Docker
	Tmpfs     bool   `json:"tmpfs,omitempty"`     // memory-backed, no host path
	SizeBytes int64  `json:"sizeBytes,omitempty"` // tmpfs size limit; 0 = no limit
}

// ResourceSpec is a container's hard limits; zero means no limit.
type ResourceSpec struct {
	CPUMilli    int64 `json:"cpuMilli,omitempty"`
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
}

// SecuritySpec is the subset of a Kubernetes securityContext that maps onto `docker create`.
type SecuritySpec struct {
	User            string   `json:"user,omitempty"` // "uid" or "uid:gid"
	GroupAdd        []string `json:"groupAdd,omitempty"`
	ReadOnlyRootFS  bool     `json:"readOnlyRootFS,omitempty"`
	NoNewPrivileges bool     `json:"noNewPrivileges,omitempty"`
	CapAdd          []string `json:"capAdd,omitempty"`
	CapDrop         []string `json:"capDrop,omitempty"`
}

type Instance struct {
	UID         string `json:"uid"`
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Executor    string `json:"executor"`
	Environment string `json:"environment,omitempty"`
	Phase       string `json:"phase"`
	Message     string `json:"message"`
	PID         int    `json:"pid"`
	ContainerID string `json:"containerID"`
	Port        int    `json:"port"`
	Ports       []int  `json:"fixedPorts,omitempty"`
	// PortMappings lists published Docker ports in `docker ps` notation ("127.0.0.1:51435->80/tcp, ..."),
	// the same for Pods Docker runs for the cluster and for local containers, so the UI shows both alike.
	PortMappings string `json:"portMappings,omitempty"`
	ImageID      string `json:"imageID,omitempty"` // Docker: the resolved image (sha256:...)
	ExitCode     int    `json:"exitCode"`
	Restarts     int    `json:"restarts"`
	StartedAt    string `json:"startedAt"`
	// PreviousStartedAt is when the run before this one started (Docker keeps one log for all runs of a container,
	// so the start times are the boundaries of --previous).
	PreviousStartedAt string `json:"previousStartedAt,omitempty"`
	LastExitAt        string `json:"lastExitAt"`
	LastExitCode      int    `json:"lastExitCode"`
	LastExitReason    string `json:"lastExitReason"`
	FailureCount      int    `json:"failureCount"`
	// AdoptNote is set when a program that an earlier agent started was taken over although its recorded spec differs
	// from the current one; the Pod gets a Warning event.
	AdoptNote string `json:"adoptNote,omitempty"`
	// Listening is where a native program listens on its declared ports (empty until it has opened them).
	Listening []Listener `json:"listening,omitempty"`
}

// ExecResult is the outcome of running a command inside a workload.
type ExecResult struct {
	ExitCode int
	Output   string // combined stdout and stderr, truncated
}

// Optional capabilities of an Engine, found by type assertion like StreamLogs, so an engine that lacks one (a test
// double, an older implementation) still works.

// Execer runs a command in a running workload: `docker exec` for Docker; for a native program, a new process in
// the program's environment and working directory (not the same process or namespace).
type Execer interface {
	Exec(ctx context.Context, uid string, argv []string, timeout time.Duration) (ExecResult, error)
}

// Commander returns a command that runs argv in a running workload, for the caller to connect to its own streams
// and start (an interactive `kubectl exec`). Docker: `docker exec`; native: a new process in the program's
// environment and working directory, as its own session when tty is true.
type Commander interface {
	Command(ctx context.Context, uid string, argv []string, interactive, tty bool) (*exec.Cmd, error)
}

// Attacher returns a command that attaches to the main process of a running workload (`docker attach`). A native
// program's output is a file, not a stream to attach to: it returns ErrAttachViaLogs and the caller follows the log.
type Attacher interface {
	AttachCommand(ctx context.Context, uid string, stdin bool) (*exec.Cmd, error)
}

// ErrAttachViaLogs says a workload cannot be attached to, but its output can be followed.
var ErrAttachViaLogs = errors.New("attach is only available as a follow of the output")

// Killer ends a running workload's process or container with a TERM, then a KILL after grace, but keeps the
// workload so that its restart policy decides what happens next (a failed liveness probe).
type Killer interface {
	Kill(ctx context.Context, uid, reason string, grace time.Duration) error
}

// GraceStopper stops and removes a workload, giving it grace to handle SIGTERM first, without holding the
// engine's lock while it waits.
type GraceStopper interface {
	StopGrace(ctx context.Context, uid string, grace time.Duration) error
}

// Releaser is implemented by engines that can let go of their workloads without stopping them: the agent is
// restarting, the programs and containers are not. Close stops everything; Release leaves it all running, to be
// taken over again by the next agent (markers, container labels and launchd jobs identify them).
type Releaser interface {
	Release(ctx context.Context) error
}

type Engine interface {
	Ensure(context.Context, Spec) (Instance, error)
	Stop(context.Context, string) error
	List(context.Context) []Instance
	Logs(context.Context, string) (string, error)
	Close(context.Context) error
}
