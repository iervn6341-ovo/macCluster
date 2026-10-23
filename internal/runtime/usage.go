package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Usage is the resource use of one workload at one moment. CPU is cumulative (what metrics-server turns into a rate).
type Usage struct {
	UID                   string
	At                    time.Time
	CPUUsageNanoSeconds   uint64
	MemoryWorkingSetBytes uint64
	StartedAt             time.Time
}

// UsageReader reports the resource use of running workloads. It is an optional capability of an Engine.
type UsageReader interface {
	Usage(ctx context.Context) []Usage
}

// parsePSTime reads the CPU time column of ps: [[dd-]hh:]mm:ss[.cc].
func parsePSTime(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	days := 0
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		days, s = d, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, false
		}
		total = total*60 + v
	}
	return time.Duration((total + float64(days)*86400) * float64(time.Second)), true
}

type groupUse struct {
	cpu time.Duration
	rss uint64
}

// processGroups sums CPU time and resident memory per process group, which is the unit a native workload owns.
func processGroups(ctx context.Context) (map[int]groupUse, error) {
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pgid=,time=,rss=").Output()
	if err != nil {
		return nil, err
	}
	groups := map[int]groupUse{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pgid, err1 := strconv.Atoi(f[0])
		cpu, ok := parsePSTime(f[1])
		rss, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || !ok || err2 != nil {
			continue
		}
		g := groups[pgid]
		g.cpu += cpu
		g.rss += rss * 1024
		groups[pgid] = g
	}
	return groups, nil
}

// dockerSocket finds where the Docker CLI talks to the daemon (Docker Desktop keeps it in the user's home).
var (
	dockerSocketOnce sync.Once
	dockerSocketPath string
)

func dockerSocket(ctx context.Context) string {
	dockerSocketOnce.Do(func() {
		out, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err == nil {
			dockerSocketPath = strings.TrimPrefix(strings.TrimSpace(string(out)), "unix://")
		}
	})
	return dockerSocketPath
}

type dockerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Stats struct {
			InactiveFile      uint64 `json:"inactive_file"`
			TotalInactiveFile uint64 `json:"total_inactive_file"`
		} `json:"stats"`
	} `json:"memory_stats"`
}

// readDockerStats asks the Docker Engine API for one sample: its cumulative CPU counter is what a rate needs, and
// the CLI only prints a percentage.
func readDockerStats(ctx context.Context, socket, id string) (dockerStats, error) {
	var st dockerStats
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/containers/"+id+"/stats?stream=false&one-shot=true", nil)
	if err != nil {
		return st, err
	}
	res, err := client.Do(req)
	if err != nil {
		return st, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return st, fmt.Errorf("docker stats: HTTP %d", res.StatusCode)
	}
	return st, json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&st)
}

// Usage implements UsageReader.
func (e *engine) Usage(ctx context.Context) []Usage {
	type target struct {
		uid, id string
		pgid    int
		started time.Time
	}
	e.mu.Lock()
	var native, dockerTargets []target
	for uid, r := range e.entries {
		if r.exited || r.instance.Phase != "Running" {
			continue
		}
		started, _ := time.Parse(time.RFC3339, r.instance.StartedAt)
		if r.spec.Executor == "docker" {
			dockerTargets = append(dockerTargets, target{uid: uid, id: r.instance.ContainerID, started: started})
			continue
		}
		pgid := r.adoptPID
		if r.cmd != nil {
			pgid = r.cmd.Process.Pid
		}
		if pgid > 0 {
			native = append(native, target{uid: uid, pgid: pgid, started: started})
		}
	}
	e.mu.Unlock()
	var out []Usage
	now := time.Now()
	if len(native) > 0 {
		if groups, err := processGroups(ctx); err == nil {
			for _, t := range native {
				if g, ok := groups[t.pgid]; ok {
					out = append(out, Usage{UID: t.uid, At: now, CPUUsageNanoSeconds: uint64(g.cpu), MemoryWorkingSetBytes: g.rss, StartedAt: t.started})
				}
			}
		}
	}
	if len(dockerTargets) > 0 {
		if socket := dockerSocket(ctx); socket != "" {
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, t := range dockerTargets {
				wg.Add(1)
				go func(t target) {
					defer wg.Done()
					st, err := readDockerStats(ctx, socket, t.id)
					if err != nil {
						return
					}
					inactive := st.MemoryStats.Stats.InactiveFile
					if inactive == 0 {
						inactive = st.MemoryStats.Stats.TotalInactiveFile
					}
					ws := st.MemoryStats.Usage
					if inactive < ws {
						ws -= inactive
					}
					mu.Lock()
					out = append(out, Usage{UID: t.uid, At: time.Now(), CPUUsageNanoSeconds: st.CPUStats.CPUUsage.TotalUsage, MemoryWorkingSetBytes: ws, StartedAt: t.started})
					mu.Unlock()
				}(t)
			}
			wg.Wait()
		}
	}
	return out
}
