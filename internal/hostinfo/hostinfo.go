// Package hostinfo samples host capacity and health without modifying the host.
package hostinfo

import (
	"context"
	"fmt"
	"math"
	"runtime"
)

const (
	PressureNormal   = "Normal"
	PressureWarning  = "Warning"
	PressureCritical = "Critical"
	PressureUnknown  = "Unknown"
)

// Snapshot describes the host, not the Docker VM or an isolated resource pool.
// A zero optional value is unknown unless its corresponding error is empty.
type Snapshot struct {
	CPUCount              int64
	MemoryBytes           int64
	OSVersion             string
	KernelVersion         string
	Architecture          string
	DiskAvailableBytes    uint64
	DiskTotalBytes        uint64
	MemoryPressure        string
	MetalRecommendedBytes uint64
	MetalAvailable        bool
	DiskError             string
	MemoryPressureError   string
	MetalError            string
	OSVersionError        string
	KernelVersionError    string
	// Process table use, for PIDPressure. Zero MaxProcesses means it could not be read.
	ProcessCount, MaxProcesses int64
	ProcessError               string
	// Stable per machine (derived, never the raw hardware identifier) and per boot, as nodeInfo expects.
	MachineID, SystemUUID, BootID string
}

// Collect returns an error if CPU or physical memory capacity cannot be read.
// Optional health/identity errors are attached to the snapshot; callers must
// preserve Unknown health instead of interpreting missing samples as healthy.
// path must identify an existing directory on the workload's storage volume.
func Collect(ctx context.Context, path string) (Snapshot, error) {
	s := Snapshot{Architecture: runtime.GOARCH, MemoryPressure: PressureUnknown,
		MetalError: "Metal device sampling is not implemented"}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if path == "" {
		return s, fmt.Errorf("storage path is required")
	}
	err := collect(ctx, path, &s)
	if ctx.Err() != nil {
		return s, ctx.Err()
	}
	return s, err
}

// Reservation is a fixed host/agent allowance, not currently used resources.
// CPU is expressed in millicores and memory/disk in bytes.
type Reservation struct {
	CPUMilli    int64
	MemoryBytes int64
	DiskBytes   uint64
}

type Resources struct {
	CPUMilli    int64
	MemoryBytes int64
	DiskBytes   uint64
}

// Allocatable subtracts the configured reservation from total capacity exactly
// once. It never subtracts running pods, current memory use, or disk usage:
// Kubernetes scheduling accounts for pod requests independently. This advertises
// capacity only; it does not enforce CPU or memory limits on native processes.
func Allocatable(s Snapshot, reserved Reservation) (Resources, error) {
	if s.CPUCount <= 0 || s.CPUCount > math.MaxInt64/1000 || s.MemoryBytes <= 0 {
		return Resources{}, fmt.Errorf("invalid host CPU or memory capacity")
	}
	if s.DiskError != "" || s.DiskTotalBytes == 0 {
		return Resources{}, fmt.Errorf("disk capacity is unavailable")
	}
	cpu := s.CPUCount * 1000
	if reserved.CPUMilli < 0 || reserved.MemoryBytes < 0 || reserved.CPUMilli > cpu || reserved.MemoryBytes > s.MemoryBytes || reserved.DiskBytes > s.DiskTotalBytes {
		return Resources{}, fmt.Errorf("reservation must be nonnegative and no greater than host capacity")
	}
	return Resources{CPUMilli: cpu - reserved.CPUMilli, MemoryBytes: s.MemoryBytes - reserved.MemoryBytes, DiskBytes: s.DiskTotalBytes - reserved.DiskBytes}, nil
}

func diskBytes(blocks, available, blockSize uint64) (uint64, uint64, error) {
	if blockSize == 0 || blocks > math.MaxUint64/blockSize || available > blocks {
		return 0, 0, fmt.Errorf("invalid filesystem block counts")
	}
	return blocks * blockSize, available * blockSize, nil
}

// The macOS sysctl exports dispatch notification values, not the kernel's
// internal pressure enum. Source: Apple's sysctl handler and conversion in
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_memorystatus_notify.c
func darwinPressure(level uint32) (string, error) {
	switch level {
	case 1:
		return PressureNormal, nil
	case 2:
		return PressureWarning, nil
	case 4:
		return PressureCritical, nil
	default:
		return PressureUnknown, fmt.Errorf("unrecognized macOS pressure level %d", level)
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Load is how busy the host is right now, for a node's resource metrics.
type Load struct {
	// CPUCores is the number of cores' worth of CPU all processes are using at this moment.
	CPUCores float64
	// MemoryWorkingSetBytes is memory that is in use and not cheaply reclaimable (active, wired and compressed pages).
	MemoryWorkingSetBytes uint64
}
