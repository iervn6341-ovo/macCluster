package runtime

import (
	"errors"
	"sync/atomic"
	"syscall"
	"time"
)

// watchExit calls onExit once when pid exits (kqueue NOTE_EXIT works for processes that are not our children).
// It returns a stop function; an already-gone process is reported as exited immediately.
func watchExit(pid int, onExit func()) (stop func(), err error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}
	ev := syscall.Kevent_t{Ident: uint64(pid), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT}
	if _, err = syscall.Kevent(kq, []syscall.Kevent_t{ev}, nil, nil); err != nil {
		syscall.Close(kq)
		if errors.Is(err, syscall.ESRCH) {
			go onExit()
			return func() {}, nil
		}
		return nil, err
	}
	var stopped atomic.Bool
	go func() {
		defer syscall.Close(kq)
		events := make([]syscall.Kevent_t, 1)
		timeout := syscall.NsecToTimespec(int64(500 * time.Millisecond))
		for !stopped.Load() {
			n, e := syscall.Kevent(kq, nil, events, &timeout)
			if e != nil && !errors.Is(e, syscall.EINTR) {
				return
			}
			if n > 0 {
				onExit()
				return
			}
		}
	}()
	return func() { stopped.Store(true) }, nil
}
