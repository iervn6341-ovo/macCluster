// Package power keeps the Mac from going to sleep while the agent has work to do (docs/keep-awake-design.md).
// A sleeping Mac freezes every workload and, after about 40 seconds, makes the node NotReady.
//
// The agent holds an assertion by running `caffeinate -s -w <agent pid>`: -s prevents system sleep (macOS only
// honours it while the Mac is on AC power, so a laptop on battery still sleeps as the user configured), and -w ends
// it when the agent exits, so a crashed agent can never leave the Mac awake.
package power

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

const (
	WhenWorkloads = "when-workloads" // hold the assertion while any workload runs (default)
	Always        = "always"         // hold it as long as the agent runs
	Never         = "never"          // leave power management to the user
)

// ValidMode reports whether name is a known mode ("" means the default).
func ValidMode(name string) bool {
	return name == "" || name == WhenWorkloads || name == Always || name == Never
}

// Keeper holds or releases the assertion according to the mode and the number of running workloads.
type Keeper struct {
	// Command builds the process that holds the assertion (replaceable in tests).
	Command func(pid int) *exec.Cmd

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

func New() *Keeper { return &Keeper{Command: defaultCommand} }

func defaultCommand(pid int) *exec.Cmd {
	return exec.Command("caffeinate", "-s", "-w", strconv.Itoa(pid))
}

// Update applies the mode for the current number of running workloads and says whether the assertion is held.
func (k *Keeper) Update(mode string, running int) bool {
	if mode == "" {
		mode = WhenWorkloads
	}
	want := mode == Always || (mode == WhenWorkloads && running > 0)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reapLocked()
	switch {
	case want && k.cmd == nil:
		cmd := k.Command(os.Getpid())
		if cmd == nil || cmd.Start() != nil {
			return false
		}
		k.cmd, k.done = cmd, make(chan struct{})
		done := k.done
		go func() { cmd.Wait(); close(done) }()
	case !want && k.cmd != nil:
		k.stopLocked()
	}
	return k.cmd != nil
}

// Held says whether the assertion is held right now.
func (k *Keeper) Held() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reapLocked()
	return k.cmd != nil
}

// Close releases the assertion.
func (k *Keeper) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopLocked()
}

// reapLocked forgets a holder that exited on its own, so the next Update starts a new one.
func (k *Keeper) reapLocked() {
	if k.cmd == nil {
		return
	}
	select {
	case <-k.done:
		k.cmd, k.done = nil, nil
	default:
	}
}

func (k *Keeper) stopLocked() {
	if k.cmd == nil {
		return
	}
	k.cmd.Process.Kill()
	select {
	case <-k.done:
	case <-time.After(2 * time.Second):
	}
	k.cmd, k.done = nil, nil
}
