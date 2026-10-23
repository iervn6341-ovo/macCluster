//go:build !darwin && !linux

package hostinfo

import (
	"context"
	"fmt"
	"runtime"
)

func collect(_ context.Context, _ string, s *Snapshot) error {
	s.CPUCount = int64(runtime.NumCPU())
	s.OSVersion = runtime.GOOS
	s.MemoryPressureError = "memory pressure collection is unsupported on this OS"
	s.DiskError = "filesystem capacity collection is unsupported on this OS"
	return fmt.Errorf("host capacity collection is unsupported on %s", runtime.GOOS)
}
