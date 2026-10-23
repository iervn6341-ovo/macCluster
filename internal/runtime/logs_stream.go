package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// LogOptions is accepted by the optional StreamLogs capability. A nil Tail
// means all retained history; Tail pointing to zero means only future output.
type LogOptions struct {
	Tail         *int
	Follow       bool
	LimitBytes   int
	Timestamps   bool
	SinceTime    time.Time
	SinceSeconds int
	Previous     bool
	until        string // internal: end of the run a --previous request asks for (Docker)
}

var ErrLogOptionUnsupported = errors.New("log option is unsupported by this executor")
var ErrSlowLogConsumer = errors.New("log reader is too slow; stream disconnected")

// StreamLogs leaves Engine.Logs and the native on-disk format unchanged.
// A native follow session ends when its current process exits, even if the
// runtime subsequently restarts that UID. It never silently spans generations.
func (e *engine) StreamLogs(ctx context.Context, uid string, opts LogOptions) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Tail != nil && *opts.Tail < 0 || opts.LimitBytes < 0 || opts.SinceSeconds < 0 || (!opts.SinceTime.IsZero() && opts.SinceSeconds > 0) {
		return nil, fmt.Errorf("invalid log options")
	}
	e.mu.Lock()
	r := e.entries[uid]
	if e.closed || r == nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("unknown or closed workload")
	}
	if r.spec.Executor == "docker" {
		id, current, previous := r.instance.ContainerID, r.instance.StartedAt, r.instance.PreviousStartedAt
		e.mu.Unlock()
		if id == "" {
			return nil, fmt.Errorf("Docker container is unavailable")
		}
		// A restarted Docker container keeps one log for all its runs: the run boundaries are the start times.
		if opts.Previous {
			if previous == "" {
				return nil, ErrNoPreviousLogs
			}
			opts.Follow = false
			opts.until = current
			if prev, err := time.Parse(time.RFC3339Nano, previous); err == nil && prev.After(opts.SinceTime) {
				opts.SinceTime, opts.SinceSeconds = prev, 0
			}
		} else if previous != "" {
			if cur, err := time.Parse(time.RFC3339Nano, current); err == nil && cur.After(opts.SinceTime) && opts.SinceSeconds == 0 {
				opts.SinceTime = cur
			}
		}
		reader, err := streamDockerLogs(ctx, id, opts)
		if err != nil {
			return nil, err
		}
		return limitLogReader(reader, opts.LimitBytes), nil
	}
	if opts.Previous {
		e.mu.Unlock()
		lines := loadRunLines(prevLogPath(e.dir, uid), -1)
		if len(lines) == 0 {
			return nil, ErrNoPreviousLogs
		}
		return limitLogReader(io.NopCloser(bytes.NewReader(renderLines(lines, sinceOf(opts, time.Now()), opts.Tail, opts.Timestamps))), opts.LimitBytes), nil
	}
	w := r.logWriter
	if w == nil {
		if opts.Follow {
			e.mu.Unlock()
			return nil, fmt.Errorf("native process has no live log writer")
		}
		path := filepath.Join(e.dir, key(uid)+".log")
		e.mu.Unlock()
		if opts.Timestamps || opts.SinceSeconds > 0 || !opts.SinceTime.IsZero() {
			return limitLogReader(io.NopCloser(bytes.NewReader(renderLines(loadRunLines(path, -1), sinceOf(opts, time.Now()), opts.Tail, opts.Timestamps))), opts.LimitBytes), nil
		}
		data, err := nativeSnapshot(path)
		if err != nil {
			return nil, err
		}
		return limitLogReader(io.NopCloser(bytes.NewReader(tailLogBytes(data, opts.Tail))), opts.LimitBytes), nil
	}
	reader, err := w.subscribe(ctx, opts)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return limitLogReader(reader, opts.LimitBytes), nil
}

func dockerLogArgs(id string, opts LogOptions) []string {
	tail := "all"
	if opts.Tail != nil {
		tail = strconv.Itoa(*opts.Tail)
	}
	args := []string{"logs", "--tail", tail}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Timestamps {
		args = append(args, "--timestamps")
	}
	if !opts.SinceTime.IsZero() {
		args = append(args, "--since", opts.SinceTime.UTC().Format(time.RFC3339Nano))
	} else if opts.SinceSeconds > 0 {
		args = append(args, "--since", strconv.Itoa(opts.SinceSeconds)+"s")
	}
	if opts.until != "" {
		args = append(args, "--until", opts.until)
	}
	return append(args, "--", id)
}

type dockerLogReader struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (r *dockerLogReader) Close() error {
	r.once.Do(func() { r.cancel(); r.PipeReader.Close(); <-r.done })
	return nil
}

func streamDockerLogs(ctx context.Context, id string, opts LogOptions) (io.ReadCloser, error) {
	child, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	cmd := exec.CommandContext(child, "docker", dockerLogArgs(id, opts)...)
	cmd.Stdout = writer
	cmd.Stderr = writer
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	// Unblock an os/exec copy goroutine even when a canceled caller has stopped
	// reading the pipe. The CLI is always reaped by the Wait goroutine below.
	stop := context.AfterFunc(child, func() { reader.CloseWithError(child.Err()) })
	if err := cmd.Start(); err != nil {
		stop()
		cancel()
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("start Docker log reader: %w", err)
	}
	r := &dockerLogReader{PipeReader: reader, cancel: cancel, done: make(chan struct{})}
	go func() { err := cmd.Wait(); stop(); writer.CloseWithError(err); cancel(); close(r.done) }()
	return r, nil
}

type limitedLogReader struct {
	reader    io.ReadCloser
	remaining int
}

func limitLogReader(reader io.ReadCloser, limit int) io.ReadCloser {
	if limit == 0 {
		return reader
	}
	return &limitedLogReader{reader: reader, remaining: limit}
}
func (r *limitedLogReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= n
	if r.remaining == 0 {
		r.reader.Close()
	}
	return n, err
}
func (r *limitedLogReader) Close() error { return r.reader.Close() }

func nativeSnapshot(path string) ([]byte, error) {
	return readLogHistory(path)
}

func tailLogBytes(data []byte, tail *int) []byte {
	if tail == nil {
		return data
	}
	if *tail == 0 {
		return nil
	}
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	for lines := 1; lines < *tail && end > 0; lines++ {
		i := bytes.LastIndexByte(data[:end], '\n')
		if i < 0 {
			end = 0
			break
		}
		end = i
	}
	if end > 0 {
		return data[bytes.LastIndexByte(data[:end], '\n')+1:]
	}
	return data
}

const logStreamChunk = 16 * 1024
const logStreamQueue = logLimit / logStreamChunk
const maxLogSubscribers = 32

type nativeLogReader struct {
	owner   *boundedWriter
	chunks  chan []byte
	closed  chan struct{}
	once    sync.Once
	readMu  sync.Mutex
	buffer  []byte
	stamper *lineStamper // set for --timestamps: live lines are stamped as they arrive
	err     error        // guarded by owner.mu; terminal after chunks is closed
	stop    func() bool
}

func (w *boundedWriter) subscribe(ctx context.Context, opts LogOptions) (io.ReadCloser, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Follow && !w.closed && len(w.subscribers) >= maxLogSubscribers {
		return nil, fmt.Errorf("native log subscription limit reached")
	}
	// Publish what is pending to the current followers first, then snapshot exactly the published bytes:
	// the new follower gets them once and everything after through the normal feed.
	w.pumpLocked()
	var buffer []byte
	var stamper *lineStamper
	if opts.Timestamps || opts.SinceSeconds > 0 || !opts.SinceTime.IsZero() {
		lines := loadRunLines(w.file.Name(), w.offset)
		buffer = renderLines(lines, sinceOf(opts, time.Now()), opts.Tail, opts.Timestamps)
		if opts.Timestamps {
			stamper = &lineStamper{midLine: len(buffer) > 0 && buffer[len(buffer)-1] != '\n'}
		}
	} else {
		data, err := readLogHistoryUpTo(w.file.Name(), w.offset)
		if err != nil {
			return nil, err
		}
		buffer = tailLogBytes(data, opts.Tail)
	}
	r := &nativeLogReader{owner: w, chunks: make(chan []byte, logStreamQueue), closed: make(chan struct{}), buffer: buffer, stamper: stamper}
	if opts.Follow && !w.closed {
		if w.subscribers == nil {
			w.subscribers = map[*nativeLogReader]struct{}{}
		}
		w.subscribers[r] = struct{}{}
	} else {
		close(r.chunks)
	}
	// This callback does not touch r.stop, so cancellation can race registration.
	r.stop = context.AfterFunc(ctx, func() { r.closeWithError(ctx.Err()) })
	return r, nil
}

func (r *nativeLogReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	select {
	case <-r.closed:
		return 0, r.terminalError()
	default:
	}
	if len(r.buffer) > 0 {
		n := copy(p, r.buffer)
		r.buffer = r.buffer[n:]
		return n, nil
	}
	select {
	case <-r.closed:
		return 0, r.terminalError()
	case data, ok := <-r.chunks:
		if !ok {
			r.stop()
			return 0, r.terminalError()
		}
		if r.stamper != nil {
			data = r.stamper.stamp(data, time.Now())
		}
		r.buffer = data
		n := copy(p, r.buffer)
		r.buffer = r.buffer[n:]
		return n, nil
	}
}
func (r *nativeLogReader) terminalError() error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	return io.EOF
}
func (r *nativeLogReader) closeWithError(err error) {
	r.once.Do(func() {
		r.owner.mu.Lock()
		defer r.owner.mu.Unlock()
		if _, ok := r.owner.subscribers[r]; ok {
			delete(r.owner.subscribers, r)
			close(r.chunks)
		}
		r.err = err
		close(r.closed)
	})
}
func (r *nativeLogReader) Close() error { r.stop(); r.closeWithError(io.ErrClosedPipe); return nil }

// publishLocked observes every successful write before disk truncation can
// hide it from followers. Slow readers are explicitly disconnected, never
// allowed to block the process's stdout/stderr or silently skip bytes.
func (w *boundedWriter) publishLocked(data []byte) {
	if len(w.subscribers) == 0 {
		return
	}
	for len(data) > 0 {
		n := len(data)
		if n > logStreamChunk {
			n = logStreamChunk
		}
		chunk := append([]byte(nil), data[:n]...)
		data = data[n:]
		for r := range w.subscribers {
			select {
			case r.chunks <- chunk:
			default:
				r.err = ErrSlowLogConsumer
				delete(w.subscribers, r)
				close(r.chunks)
			}
		}
		if len(w.subscribers) == 0 {
			return
		}
	}
}

func (w *boundedWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.idx != nil {
		w.idx.Close()
		w.idx = nil
	}
	for r := range w.subscribers {
		close(r.chunks)
		delete(w.subscribers, r)
	}
	return w.file.Close()
}
