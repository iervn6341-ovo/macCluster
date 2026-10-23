package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	"k8s.io/apimachinery/pkg/types"
	"maccluster/internal/runtime"
)

// exitStatusError is how kubectl learns a command's exit code: the streaming server reports an error that has
// ExitStatus() and Exited() as "command terminated with exit code N".
type exitStatusError struct{ code int }

func (e *exitStatusError) Error() string {
	return fmt.Sprintf("command terminated with exit code %d", e.code)
}
func (e *exitStatusError) String() string  { return e.Error() }
func (e *exitStatusError) Exited() bool    { return true }
func (e *exitStatusError) ExitStatus() int { return e.code }

func exitError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code < 0 {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
		}
		return &exitStatusError{code}
	}
	return err
}

func (k *kubeletAPI) acquire() (release func(), err error) {
	select {
	case k.streams <- struct{}{}:
		return func() { <-k.streams }, nil
	default:
		return nil, errors.New("too many active streams")
	}
}

// runInContainer implements `kubectl exec` (and, through tar, `kubectl cp`).
func (k *kubeletAPI) runInContainer(ctx context.Context, uid types.UID, ns, name, container string, argv []string, attach vkapi.AttachIO) error {
	p, _, err := k.ownedPod(ctx, ns, name, container, uid)
	if err != nil {
		return err
	}
	commander, ok := k.engine.(runtime.Commander)
	if !ok {
		return errdefs.AsInvalidInput(errors.New("this runtime cannot run commands in a workload"))
	}
	release, err := k.acquire()
	if err != nil {
		return err
	}
	defer release()
	stdin := attach.Stdin()
	cmd, err := commander.Command(ctx, string(p.UID), argv, stdin != nil, attach.TTY())
	if err != nil {
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	if attach.TTY() {
		return runWithTerminal(cmd, attach)
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if out := attach.Stdout(); out != nil {
		cmd.Stdout = out
	}
	if out := attach.Stderr(); out != nil {
		cmd.Stderr = out
	}
	return exitError(cmd.Run())
}

// runWithTerminal runs the command on a pseudo-terminal: one stream carries both output and input, and terminal
// size changes from the client are applied to it.
func runWithTerminal(cmd *exec.Cmd, attach vkapi.AttachIO) error {
	master, slave, err := openPTY()
	if err != nil {
		return err
	}
	defer master.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if err := cmd.Start(); err != nil {
		slave.Close()
		return err
	}
	slave.Close()
	if in := attach.Stdin(); in != nil {
		go func() { io.Copy(master, in) }()
	}
	if resize := attach.Resize(); resize != nil {
		go func() {
			for size := range resize {
				setWindowSize(master, size.Width, size.Height)
			}
		}()
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		if out := attach.Stdout(); out != nil {
			io.Copy(out, master) // ends with EIO once the command has exited and everything has been read
		} else {
			io.Copy(io.Discard, master)
		}
	}()
	waitErr := cmd.Wait()
	select {
	case <-copied:
	case <-time.After(2 * time.Second):
	}
	return exitError(waitErr)
}

// attachToContainer implements `kubectl attach`. A Docker container is attached to for real; a native program's
// output goes to a file, so attaching follows its output from now on (it has no input to attach to).
func (k *kubeletAPI) attachToContainer(ctx context.Context, uid types.UID, ns, name, container string, attach vkapi.AttachIO) error {
	p, _, err := k.ownedPod(ctx, ns, name, container, uid)
	if err != nil {
		return err
	}
	release, err := k.acquire()
	if err != nil {
		return err
	}
	defer release()
	if attacher, ok := k.engine.(runtime.Attacher); ok {
		cmd, err := attacher.AttachCommand(ctx, string(p.UID), attach.Stdin() != nil)
		if err == nil {
			cmd.WaitDelay = 2 * time.Second
			if in := attach.Stdin(); in != nil {
				cmd.Stdin = in
			}
			if out := attach.Stdout(); out != nil {
				cmd.Stdout = out
			}
			if out := attach.Stderr(); out != nil {
				cmd.Stderr = out
			}
			return exitError(cmd.Run())
		}
		if !errors.Is(err, runtime.ErrAttachViaLogs) {
			return err
		}
	}
	streamer, ok := k.engine.(interface {
		StreamLogs(context.Context, string, runtime.LogOptions) (io.ReadCloser, error)
	})
	if !ok {
		return errdefs.AsInvalidInput(errors.New("this runtime cannot attach"))
	}
	if attach.Stdin() != nil {
		if errw := attach.Stderr(); errw != nil {
			fmt.Fprintln(errw, "stdin is not available for a native program: showing its output only")
		}
	}
	zero := 0
	r, err := streamer.StreamLogs(ctx, string(p.UID), runtime.LogOptions{Follow: true, Tail: &zero})
	if err != nil {
		return err
	}
	defer r.Close()
	out := attach.Stdout()
	if out == nil {
		out = nopWriteCloser{io.Discard}
	}
	stop := context.AfterFunc(ctx, func() { r.Close() })
	defer stop()
	_, err = io.Copy(out, r)
	if err != nil && ctx.Err() == nil && !errors.Is(err, io.ErrClosedPipe) {
		return err
	}
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
