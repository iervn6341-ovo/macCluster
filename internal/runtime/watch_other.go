//go:build !darwin

package runtime

import (
	"sync/atomic"
	"syscall"
	"time"
)

// watchExit calls onExit once when pid exits. Without kqueue (non-macOS builds, which only exist so the Linux-side
// commands of the agent can be built) the process is polled.
func watchExit(pid int, onExit func()) (stop func(), err error) {
	var stopped atomic.Bool
	go func() {
		for !stopped.Load() {
			if syscall.Kill(pid, 0) == syscall.ESRCH {
				onExit()
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return func() { stopped.Store(true) }, nil
}
