//go:build linux

package hostinfo

import (
	"context"
	"fmt"
	"math"
	"runtime"

	"golang.org/x/sys/unix"
)

// Linux is a development fallback: these are host capacities, not cgroup limits.
func collect(ctx context.Context, path string, s *Snapshot) error {
	s.CPUCount = int64(runtime.NumCPU())
	s.MemoryPressureError = "macOS memory pressure is unavailable on Linux"
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return fmt.Errorf("read physical memory: %w", err)
	}
	unit := uint64(info.Unit)
	if unit == 0 || uint64(info.Totalram) == 0 || uint64(info.Totalram) > math.MaxInt64/unit {
		return fmt.Errorf("invalid physical memory capacity")
	}
	s.MemoryBytes = int64(uint64(info.Totalram) * unit)
	if err := ctx.Err(); err != nil {
		return err
	}
	var name unix.Utsname
	err := unix.Uname(&name)
	if err == nil {
		s.KernelVersion = unix.ByteSliceToString(name.Release[:])
		s.OSVersion = unix.ByteSliceToString(name.Sysname[:]) + " " + s.KernelVersion
	}
	s.KernelVersionError, s.OSVersionError = errorText(err), errorText(err)
	var fs unix.Statfs_t
	err = unix.Statfs(path, &fs)
	if err == nil {
		s.DiskTotalBytes, s.DiskAvailableBytes, err = diskBytes(fs.Blocks, fs.Bavail, uint64(fs.Bsize))
	}
	s.DiskError = errorText(err)
	return nil
}
