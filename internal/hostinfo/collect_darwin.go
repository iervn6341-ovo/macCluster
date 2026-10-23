//go:build darwin

package hostinfo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var (
	identityOnce sync.Once
	machineID    string
	systemUUID   string

	procMu      sync.Mutex
	procCount   int64
	procChecked time.Time
	procErr     error
)

// machineIdentity derives the IDs from the hardware UUID with a fixed prefix and a hash, so the cluster
// gets an identifier that is stable for this Mac but is not the hardware identifier itself.
func machineIdentity() (string, string) {
	identityOnce.Do(func() {
		out, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, "IOPlatformUUID") {
				continue
			}
			parts := strings.Split(line, "\"")
			if len(parts) < 4 {
				return
			}
			sum := sha256.Sum256([]byte("maccluster-machine-id:" + parts[3]))
			h := hex.EncodeToString(sum[:16])
			machineID = h
			systemUUID = h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
			return
		}
	})
	return machineID, systemUUID
}

// processCount counts the host's processes, at most once every 30 seconds: it runs `ps`, and the node status
// is refreshed every few seconds.
func processCount(ctx context.Context) (int64, error) {
	procMu.Lock()
	defer procMu.Unlock()
	if !procChecked.IsZero() && time.Since(procChecked) < 30*time.Second {
		return procCount, procErr
	}
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pid=").Output()
	procChecked = time.Now()
	if err != nil {
		procCount, procErr = 0, err
		return 0, err
	}
	procCount, procErr = int64(len(strings.Fields(string(out)))), nil
	return procCount, nil
}

func collect(ctx context.Context, path string, s *Snapshot) error {
	var required []error
	cpu, err := unix.SysctlUint32("hw.logicalcpu")
	if err != nil || cpu == 0 {
		required = append(required, fmt.Errorf("read logical CPU capacity: %v (value %d)", err, cpu))
	} else {
		s.CPUCount = int64(cpu)
	}
	memory, err := unix.SysctlUint64("hw.memsize")
	if err != nil || memory == 0 || memory > math.MaxInt64 {
		required = append(required, fmt.Errorf("read physical memory capacity: %v (value %d)", err, memory))
	} else {
		s.MemoryBytes = int64(memory)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.OSVersion, err = unix.Sysctl("kern.osproductversion")
	s.OSVersionError = errorText(err)
	s.KernelVersion, err = unix.Sysctl("kern.osrelease")
	s.KernelVersionError = errorText(err)
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err == nil {
		s.MemoryPressure, err = darwinPressure(level)
	}
	s.MemoryPressureError = errorText(err)
	var fs unix.Statfs_t
	err = unix.Statfs(path, &fs)
	if err == nil {
		s.DiskTotalBytes, s.DiskAvailableBytes, err = diskBytes(fs.Blocks, fs.Bavail, uint64(fs.Bsize))
	}
	s.DiskError = errorText(err)
	if max, perr := unix.SysctlUint32("kern.maxproc"); perr == nil && max > 0 {
		s.MaxProcesses = int64(max)
		s.ProcessCount, perr = processCount(ctx)
		s.ProcessError = errorText(perr)
	} else {
		s.ProcessError = errorText(perr)
	}
	s.MachineID, s.SystemUUID = machineIdentity()
	s.BootID, _ = unix.Sysctl("kern.bootsessionuuid")
	return errors.Join(required...)
}

// CurrentLoad samples CPU and memory use of the whole host. It is cheap enough to call on every metrics scrape.
func CurrentLoad(ctx context.Context) (Load, error) {
	var l Load
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "%cpu=").Output()
	if err != nil {
		return l, err
	}
	for _, f := range strings.Fields(string(out)) {
		if v, perr := strconv.ParseFloat(f, 64); perr == nil {
			l.CPUCores += v / 100
		}
	}
	vm, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return l, err
	}
	page := uint64(16384)
	var pages uint64
	for i, line := range strings.Split(string(vm), "\n") {
		if i == 0 {
			if a := strings.Index(line, "page size of "); a >= 0 {
				if n, perr := strconv.ParseUint(strings.Fields(line[a+len("page size of "):])[0], 10, 64); perr == nil {
					page = n
				}
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Pages active", "Pages wired down", "Pages occupied by compressor":
			if n, perr := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(val), "."), 10, 64); perr == nil {
				pages += n
			}
		}
	}
	l.MemoryWorkingSetBytes = pages * page
	return l, nil
}
