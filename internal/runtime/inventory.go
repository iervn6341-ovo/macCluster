package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// DockerInventory is read-only discovery, separate from owned runtime state.
// Exclude our agents and Kubernetes workloads even if another profile owns them.
func DockerInventory(ctx context.Context) ([]ExternalContainer, error) {
	// Query labels individually: the flattened Labels string is ambiguous for
	// multiple Compose files (their values may contain commas).
	format := `{"ID":{{json .ID}},"Names":{{json .Names}},"Image":{{json .Image}},"State":{{json .State}},"Status":{{json .Status}},"Ports":{{json .Ports}},"Labels":{{json .Labels}},"ComposeProject":{{json (.Label "com.docker.compose.project")}},"ComposeService":{{json (.Label "com.docker.compose.service")}},"ComposeWorkingDir":{{json (.Label "com.docker.compose.project.working_dir")}},"ComposeConfigFiles":{{json (.Label "com.docker.compose.project.config_files")}}}`
	args := []string{"ps", "--all", "--no-trunc", "--format", format}
	out, err := docker(ctx, args...)
	if err != nil {
		return nil, err
	}
	result := []ExternalContainer{}
	var needConfig []int
	decoder := json.NewDecoder(strings.NewReader(out))
	for {
		var row struct{ ID, Names, Image, State, Status, Ports, Labels, ComposeProject, ComposeService, ComposeWorkingDir, ComposeConfigFiles string }
		err = decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid Docker inventory: %w", err)
		}
		excluded := false
		for _, label := range strings.Split(row.Labels, ",") {
			key, _, _ := strings.Cut(label, "=")
			switch key {
			case "maccluster.owner", "io.kubernetes.pod.uid", "io.kubernetes.pod.name", "io.kubernetes.container.name":
				excluded = true
			}
		}
		if excluded {
			continue
		}
		phase := map[string]string{"running": "Running", "exited": "Stopped", "created": "Created", "paused": "Paused", "restarting": "Restarting", "dead": "Dead", "removing": "Removing"}[row.State]
		if phase == "" {
			phase = "Unknown"
		}
		result = append(result, ExternalContainer{Instance: Instance{UID: "docker-external-" + row.ID, Name: row.Names, Namespace: "local", Executor: "docker", Phase: phase, ContainerID: row.ID, Message: row.Status}, External: true, Image: row.Image, Ports: row.Ports, ComposeProject: row.ComposeProject, ComposeService: row.ComposeService, ComposeWorkingDir: row.ComposeWorkingDir, ComposeConfigFiles: row.ComposeConfigFiles})
		if row.Ports == "" && row.State != "running" {
			needConfig = append(needConfig, len(result)-1)
		}
	}
	fillConfiguredPorts(ctx, result, needConfig)
	for i := range result {
		result[i].PortMappings = result[i].Ports
	}
	return result, nil
}

// fillConfiguredPorts supplies what Docker Desktop shows for containers that are not running:
// `docker ps` lists only ports that are published right now, which is nothing for a stopped
// container, so the mappings it was created with are read from its configuration instead.
// Failures leave the ports empty; this is display-only.
func fillConfiguredPorts(ctx context.Context, containers []ExternalContainer, indexes []int) {
	if len(indexes) == 0 {
		return
	}
	args := []string{"inspect", "--type", "container", "--format", "{{.Id}}\t{{json .HostConfig.PortBindings}}\t{{json .Config.ExposedPorts}}"}
	for _, i := range indexes {
		args = append(args, containers[i].ContainerID)
	}
	out, err := docker(ctx, args...)
	if err != nil {
		return
	}
	byID := map[string]int{}
	for _, i := range indexes {
		byID[containers[i].ContainerID] = i
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) != 3 {
			continue
		}
		i, ok := byID[parts[0]]
		if !ok {
			continue
		}
		if ports := configuredPorts(parts[1], parts[2]); ports != "" {
			containers[i].Ports = ports
			containers[i].PortsConfigured = true
		}
	}
}

// configuredPorts renders create-time port mappings the way `docker ps` renders live ones
// ("0.0.0.0:3000->8080/tcp"); ports that are only declared by the image appear as "8080/tcp".
func configuredPorts(bindingsJSON, exposedJSON string) string {
	var bindings map[string][]struct{ HostIp, HostPort string }
	var exposed map[string]json.RawMessage
	_ = json.Unmarshal([]byte(bindingsJSON), &bindings)
	_ = json.Unmarshal([]byte(exposedJSON), &exposed)
	keys := map[string]bool{}
	for k := range bindings {
		keys[k] = true
	}
	for k := range exposed {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, aproto := splitPort(ordered[i])
		b, bproto := splitPort(ordered[j])
		if a != b {
			return a < b
		}
		return aproto < bproto
	})
	var parts []string
	for _, key := range ordered {
		port, proto := splitPort(key)
		published := false
		for _, b := range bindings[key] {
			if b.HostPort == "" {
				continue
			}
			host := b.HostIp
			if host == "" {
				host = "0.0.0.0"
			}
			parts = append(parts, fmt.Sprintf("%s:%s->%d/%s", host, b.HostPort, port, proto))
			published = true
		}
		if !published {
			parts = append(parts, fmt.Sprintf("%d/%s", port, proto))
		}
	}
	return strings.Join(parts, ", ")
}

func splitPort(key string) (int, string) {
	number, proto, _ := strings.Cut(key, "/")
	n, _ := strconv.Atoi(number)
	if proto == "" {
		proto = "tcp"
	}
	return n, proto
}

type ExternalContainer struct {
	Instance
	ComposeProject     string `json:"composeProject,omitempty"`
	ComposeService     string `json:"composeService,omitempty"`
	ComposeWorkingDir  string `json:"composeWorkingDir,omitempty"`
	ComposeConfigFiles string `json:"composeConfigFiles,omitempty"`
	External           bool   `json:"external"`
	Image              string `json:"image"`
	Ports              string `json:"ports"`
	// PortsConfigured means Ports are the mappings the container was created with, because it is
	// not running and Docker publishes nothing right now.
	PortsConfigured bool `json:"portsConfigured,omitempty"`
}

// ControlExternalDocker verifies ownership again immediately before acting.
// Managed containers must go through their owning reconciler instead.
func ControlExternalDocker(ctx context.Context, id, action string) error {
	if action != "start" && action != "stop" {
		return fmt.Errorf("unsupported Docker action")
	}
	if len(id) != 64 || strings.IndexFunc(id, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0 {
		return fmt.Errorf("full Docker container ID required")
	}
	d, err := inspect(ctx, id)
	if err != nil {
		return err
	}
	for _, key := range []string{"maccluster.owner", "io.kubernetes.pod.uid", "io.kubernetes.pod.name", "io.kubernetes.container.name"} {
		if _, exists := d.Config.Labels[key]; exists {
			return fmt.Errorf("container is managed by MacCluster or Kubernetes; use its owning controller")
		}
	}
	if d.ID != id {
		return fmt.Errorf("Docker container identity mismatch")
	}
	args := []string{action}
	if action == "stop" {
		args = append(args, "--time", "10")
	}
	_, err = docker(ctx, append(args, id)...)
	return err
}

// LogsDocker reads a bounded snapshot of stdout and stderr, including stopped containers.
func LogsDocker(ctx context.Context, id string) (string, error) {
	if len(id) != 64 || strings.IndexFunc(id, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0 {
		return "", fmt.Errorf("full Docker container ID required")
	}
	return docker(ctx, "logs", "--tail", "500", id)
}

// InspectDocker returns the unmodified read-only container inspection JSON.
func InspectDocker(ctx context.Context, id string) (string, error) {
	if len(id) != 64 || strings.IndexFunc(id, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0 {
		return "", fmt.Errorf("full Docker container ID required")
	}
	return docker(ctx, "inspect", "--type", "container", id)
}

// StatsDocker samples only the requested container and never starts it.
func StatsDocker(ctx context.Context, id string) (string, error) {
	if len(id) != 64 || strings.IndexFunc(id, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0 {
		return "", fmt.Errorf("full Docker container ID required")
	}
	d, err := inspect(ctx, id)
	if err != nil {
		return "", err
	}
	if d.ID != id {
		return "", fmt.Errorf("Docker container identity mismatch")
	}
	if !d.State.Running {
		return `{"running":false,"metrics":{}}`, nil
	}
	out, err := docker(ctx, "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}", id)
	if err != nil {
		return "", err
	}
	var metrics map[string]string
	if err := json.Unmarshal([]byte(out), &metrics); err != nil {
		return "", fmt.Errorf("invalid Docker stats: %w", err)
	}
	if metrics["ID"] != id {
		return "", fmt.Errorf("Docker stats identity mismatch")
	}
	result, err := json.Marshal(struct {
		Running bool              `json:"running"`
		Metrics map[string]string `json:"metrics"`
	}{true, metrics})
	return string(result), err
}
