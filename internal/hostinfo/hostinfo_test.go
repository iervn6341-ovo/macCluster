package hostinfo

import (
	"context"
	"errors"
	"math"
	"os"
	"testing"
)

// Opt-in because sandboxed CI may prohibit host sysctls.
func TestCollectLiveHost(t *testing.T) {
	if os.Getenv("MACCLUSTER_TEST_HOSTINFO") != "1" {
		t.Skip("set MACCLUSTER_TEST_HOSTINFO=1 for read-only host sampling")
	}
	s, err := Collect(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUCount <= 0 || s.MemoryBytes <= 0 || s.Architecture == "" || s.DiskError != "" || s.DiskTotalBytes == 0 || s.DiskAvailableBytes > s.DiskTotalBytes {
		t.Fatalf("invalid host capacity sample: %+v", s)
	}
	if s.MemoryPressure == PressureUnknown && s.MemoryPressureError == "" {
		t.Fatal("unknown pressure must carry a reason")
	}
	if s.MetalError == "" && !s.MetalAvailable && s.MetalRecommendedBytes != 0 {
		t.Fatal("unavailable Metal device cannot have a memory recommendation")
	}
	t.Logf("CPU=%d memory=%d OS=%s kernel=%s arch=%s disk-total=%d disk-available=%d pressure=%s metal-known=%t",
		s.CPUCount, s.MemoryBytes, s.OSVersion, s.KernelVersion, s.Architecture, s.DiskTotalBytes, s.DiskAvailableBytes, s.MemoryPressure, s.MetalError == "")
}

func TestPressureUsesDispatchValuesAndRejectsUnknown(t *testing.T) {
	for _, tc := range []struct {
		value uint32
		want  string
	}{
		{1, PressureNormal}, {2, PressureWarning}, {4, PressureCritical},
		{0, PressureUnknown}, {3, PressureUnknown}, {8, PressureUnknown},
	} {
		got, err := darwinPressure(tc.value)
		if got != tc.want || (err != nil) != (tc.want == PressureUnknown) {
			t.Errorf("level %d: got %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
}

func TestAllocatableUsesTotalNotCurrentlyAvailable(t *testing.T) {
	s := Snapshot{CPUCount: 8, MemoryBytes: 16 << 30, DiskTotalBytes: 100 << 30, DiskAvailableBytes: 5 << 30}
	got, err := Allocatable(s, Reservation{CPUMilli: 750, MemoryBytes: 3 << 30, DiskBytes: 10 << 30})
	if err != nil {
		t.Fatal(err)
	}
	want := Resources{CPUMilli: 7250, MemoryBytes: 13 << 30, DiskBytes: 90 << 30}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	s.DiskAvailableBytes = 1
	again, err := Allocatable(s, Reservation{CPUMilli: 750, MemoryBytes: 3 << 30, DiskBytes: 10 << 30})
	if err != nil || again != want {
		t.Fatalf("usage changed allocatable: %+v, %v", again, err)
	}
}

func TestAllocatableRejectsInvalidReservationsAndCapacity(t *testing.T) {
	s := Snapshot{CPUCount: 2, MemoryBytes: 4096, DiskTotalBytes: 8192}
	for _, reserved := range []Reservation{
		{CPUMilli: -1}, {MemoryBytes: -1}, {CPUMilli: 2001}, {MemoryBytes: 4097}, {DiskBytes: 8193},
	} {
		if _, err := Allocatable(s, reserved); err == nil {
			t.Errorf("accepted invalid reservation %+v", reserved)
		}
	}
	for _, invalid := range []Snapshot{
		{CPUCount: 0, MemoryBytes: 4096, DiskTotalBytes: 8192},
		{CPUCount: math.MaxInt64, MemoryBytes: 4096, DiskTotalBytes: 8192},
		{CPUCount: 2, MemoryBytes: 0, DiskTotalBytes: 8192},
		{CPUCount: 2, MemoryBytes: 4096},
		{CPUCount: 2, MemoryBytes: 4096, DiskTotalBytes: 8192, DiskError: "unavailable"},
	} {
		if _, err := Allocatable(invalid, Reservation{}); err == nil {
			t.Errorf("accepted invalid capacity %+v", invalid)
		}
	}
	if got, err := Allocatable(s, Reservation{CPUMilli: 2000, MemoryBytes: 4096, DiskBytes: 8192}); err != nil || got != (Resources{}) {
		t.Fatalf("fully reserved host should yield zero: %+v, %v", got, err)
	}
}

func TestDiskConversionRejectsOverflowAndImpossibleValues(t *testing.T) {
	total, available, err := diskBytes(100, 3, 4096)
	if err != nil || total != 409600 || available != 12288 {
		t.Fatalf("got %d, %d, %v", total, available, err)
	}
	for _, args := range [][3]uint64{{1, 1, 0}, {math.MaxUint64, 1, 4096}, {100, 101, 4096}} {
		if _, _, err := diskBytes(args[0], args[1], args[2]); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if _, available, err := diskBytes(100, 0, 4096); err != nil || available != 0 {
		t.Fatalf("full filesystem: %d, %v", available, err)
	}
}

func TestCollectCanceledAndMissingPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := Collect(ctx, ".")
	if !errors.Is(err, context.Canceled) || s.MemoryPressure != PressureUnknown {
		t.Fatalf("got %+v, %v", s, err)
	}
	if _, err := Collect(context.Background(), ""); err == nil {
		t.Fatal("accepted missing storage path")
	}
}
