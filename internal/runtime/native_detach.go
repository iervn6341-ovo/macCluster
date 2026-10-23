package runtime

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Native programs outlive the agent. They write straight to their log file (no pipe to the agent, so no
// SIGPIPE and no lost output while the agent is away) and leave a marker that identifies the process well
// enough for a restarted agent to recognise it again and take it over instead of refusing to start.

// logRotateAt is how large the live log file may grow before it is copied to <log>.1 and truncated.
// Together with the previous generation a service keeps between logRotateAt and 2*logRotateAt of history.
var logRotateAt int64 = logLimit

// errDetachedExit is delivered for an adopted process: it is not our child, so its exit status is unknown.
var errDetachedExit = errors.New("process exited while detached from the agent; exit status unknown")

type nativeMarker struct {
	UID       string `json:"uid"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Intent    bool   `json:"intent,omitempty"` // written before Start; no process is known yet
	PID       int    `json:"pid,omitempty"`
	Start     string `json:"start,omitempty"`   // ps lstart of the process: guards against PID reuse
	Label     string `json:"label,omitempty"`   // launchd job label, when the launchd backend started it
	Command   string `json:"command,omitempty"` // command line when it was recorded; diagnostic only (it can change on exec)
	Spec      string `json:"spec,omitempty"`    // hash of the immutable spec it was started from
	Port      int    `json:"port,omitempty"`
	Ports     []int  `json:"ports,omitempty"`
	// Listeners is where the program was found to listen (listeners.go): remembered so a restarted agent need not ask.
	Listeners []Listener `json:"listeners,omitempty"`
	StartedAt string     `json:"startedAt,omitempty"`
}

// specHash is the fingerprint older versions wrote into markers: a hash of the whole Spec, so any field a later
// version adds changes it. It is only kept to read those markers.
func specHash(s Spec) string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// specFingerprint identifies what a running program was started with: the fields that decide what runs, and only
// those, so that adding an unrelated field to Spec in a later version does not make every running program look
// like it was started from a different spec. The "v2:" prefix tells it from the older whole-Spec hash.
func specFingerprint(s Spec) string {
	stable := struct {
		Executor      string
		Command, Args []string
		Env           map[string]string
		Environment   string
		FixedPorts    []int
		WorkingDir    string
		ContainerPort int
	}{s.Executor, s.Command, s.Args, s.Env, s.Environment, s.FixedPorts, s.WorkingDir, s.ContainerPort}
	b, _ := json.Marshal(stable)
	h := sha256.Sum256(b)
	return "v2:" + hex.EncodeToString(h[:])
}

// fingerprintMatches compares a marker's fingerprint with a spec, in whichever format the marker has.
func fingerprintMatches(recorded string, s Spec) bool {
	if strings.HasPrefix(recorded, "v2:") {
		return recorded == specFingerprint(s)
	}
	return recorded == specHash(s)
}

func writeMarker(path string, m nativeMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readMarker returns ok=false for records that are not this format (older versions wrote "pid=N uid=U").
func readMarker(path string) (m nativeMarker, legacyUID string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64*1024 {
		return m, "", false
	}
	if json.Unmarshal(data, &m) == nil && m.UID != "" {
		return m, "", true
	}
	for _, field := range strings.Fields(string(data)) {
		if v, found := strings.CutPrefix(field, "uid="); found {
			legacyUID = v
		}
	}
	return nativeMarker{}, legacyUID, false
}

// processIdentity is the start time and command line of a live process, as `ps` reports them.
func processIdentity(pid int) (start, command string, err error) {
	run := func(field string) (string, error) {
		out, e := exec.Command("ps", "-ww", "-o", field+"=", "-p", fmt.Sprint(pid)).Output()
		return strings.TrimSpace(string(out)), e
	}
	if start, err = run("lstart"); err != nil || start == "" {
		return "", "", errors.New("process not found")
	}
	if command, err = run("command"); err != nil || command == "" {
		return "", "", errors.New("process not found")
	}
	return start, command, nil
}

// matchesMarker reports whether pid is still the process the marker describes. The identity is the PID with
// its start time (a reused PID has a different one) and being the leader of its own process group (we start
// every program with Setpgid, so its group ID is its PID). The command line is deliberately not compared:
// a launcher such as /usr/bin/python3 re-executes the real interpreter, so it changes after the start.
func matchesMarker(m nativeMarker) bool {
	if m.PID <= 0 || m.Start == "" {
		return false
	}
	start, _, err := processIdentity(m.PID)
	if err != nil || start != m.Start {
		return false
	}
	out, err := exec.Command("ps", "-o", "pgid=", "-p", fmt.Sprint(m.PID)).Output()
	return err == nil && strings.TrimSpace(string(out)) == fmt.Sprint(m.PID)
}

// pump publishes what the process has appended to its log file since the last call, handles truncation
// by others and rotates the file (copy to .1, then truncate) once it passes logRotateAt.
func (w *boundedWriter) pump() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pumpLocked()
}

func (w *boundedWriter) pumpLocked() {
	if w.closed {
		return
	}
	st, err := w.file.Stat()
	if err != nil {
		return
	}
	size := st.Size()
	if size < w.offset {
		w.offset = 0
	}
	for w.offset < size {
		n := size - w.offset
		if n > 4*logStreamChunk {
			n = 4 * logStreamChunk
		}
		buf := make([]byte, n)
		m, err := w.file.ReadAt(buf, w.offset)
		if m > 0 {
			w.publishLocked(buf[:m])
			w.offset += int64(m)
			w.noteLocked(w.offset)
		}
		if err != nil {
			break
		}
	}
	if size >= logRotateAt && w.offset >= size {
		prev, err := os.OpenFile(w.file.Name()+".1", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		_, err = io.Copy(prev, io.NewSectionReader(w.file, 0, size))
		prev.Close()
		if err == nil && w.file.Truncate(0) == nil {
			w.offset = 0
			w.rotateIndexLocked()
		}
	}
}

// noteLocked records that the log has been seen up to end, now. Batches are coalesced into one record per second at
// most (keeping the time the second began), so the index stays small however chatty the program is.
func (w *boundedWriter) noteLocked(end int64) {
	if w.idx == nil {
		// Not O_APPEND: the last record is updated in place, which WriteAt refuses on an append-only file.
		f, err := os.OpenFile(indexPath(w.file.Name()), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return
		}
		w.idx, w.idxSize = f, st.Size()-st.Size()%indexRecordSize
	}
	now := time.Now()
	if w.lastNote.IsZero() || now.Sub(w.lastNote) >= time.Second || w.idxSize < indexRecordSize {
		w.lastNote = now
		var b [indexRecordSize]byte
		binary.LittleEndian.PutUint64(b[0:], uint64(end))
		binary.LittleEndian.PutUint64(b[8:], uint64(now.UnixNano()))
		if _, err := w.idx.WriteAt(b[:], w.idxSize); err == nil {
			w.idxSize += indexRecordSize
		}
		return
	}
	// Within the same second: move the end of the last record forward instead of adding one.
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(end))
	w.idx.WriteAt(b[:], w.idxSize-indexRecordSize)
}

// rotateIndexLocked moves the index along with the log it describes.
func (w *boundedWriter) rotateIndexLocked() {
	if w.idx != nil {
		w.idx.Close()
		w.idx, w.idxSize = nil, 0
	}
	os.Rename(indexPath(w.file.Name()), indexPath(w.file.Name()+".1"))
	w.lastNote = time.Time{}
}

// tail follows the log file until the writer is closed.
func (w *boundedWriter) tail() {
	for {
		time.Sleep(200 * time.Millisecond)
		w.mu.Lock()
		closed := w.closed
		w.mu.Unlock()
		if closed {
			return
		}
		w.pump()
	}
}

// finish drains what is left in the file and ends every follower.
func (w *boundedWriter) finish() {
	w.pump()
	w.Close()
}

// readLogHistory returns up to logLimit bytes of a native log: the previous generation followed by the live file.
func readLogHistory(path string) ([]byte, error) { return readLogHistoryUpTo(path, -1) }

// readLogHistoryUpTo is readLogHistory with the live file cut at curLimit bytes (-1: no cut), so a follower
// can start from exactly the bytes that have been published and miss or repeat nothing.
func readLogHistoryUpTo(path string, curLimit int64) ([]byte, error) {
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if curLimit >= 0 && int64(len(cur)) > curLimit {
		cur = cur[:curLimit]
	}
	data := cur
	if len(data) < logLimit {
		if prev, perr := os.ReadFile(path + ".1"); perr == nil {
			data = append(prev, cur...)
		} else if err != nil {
			return nil, err
		}
	}
	if len(data) > logLimit {
		data = data[len(data)-logLimit:]
	}
	return data, nil
}

func logFilePath(dir, uid string) string { return filepath.Join(dir, key(uid)+".log") }

// openLog opens a native log for the child to append to and for the agent to follow.
func openLog(path string) (*boundedWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &boundedWriter{file: f, offset: st.Size()}, nil
}

// adoptMarkers runs when the engine opens its state directory. A native process whose marker still
// identifies a live process is taken over; a dead one is forgotten; anything that cannot be verified is
// kept as a blocked entry so that UID is never started twice, while everything else carries on.
func (e *engine) adoptMarkers() {
	markers, _ := filepath.Glob(filepath.Join(e.dir, "*.native"))
	for _, path := range markers {
		m, legacyUID, ok := readMarker(path)
		switch {
		case ok && !m.Intent && m.PID > 0 && matchesMarker(m):
			e.adopt(path, m)
		case ok && !m.Intent && m.PID > 0 && m.Start != "":
			// The process is gone (or the PID now belongs to something else): nothing to take over.
			os.Remove(path)
		default:
			uid := m.UID
			if uid == "" {
				uid = legacyUID
			}
			if uid == "" {
				continue
			}
			r := &entry{spec: Spec{UID: uid, Executor: "native"}, specUnknown: true, marker: path,
				blocked:  "A previous run left a launch record that cannot be verified; check for a leftover process of this service and remove " + filepath.Base(path),
				instance: Instance{UID: uid, Name: m.Name, Namespace: m.Namespace, Executor: "native", Phase: "Unknown"}}
			r.instance.Message = r.blocked
			e.entries[uid] = r
		}
	}
}

func (e *engine) adopt(path string, m nativeMarker) {
	w, err := openLog(logFilePath(e.dir, m.UID))
	if err != nil {
		return
	}
	r := &entry{spec: Spec{UID: m.UID, Executor: "native"}, specUnknown: true, marker: path, adoptPID: m.PID, specHash: m.Spec,
		logWriter: w, done: make(chan error, 1),
		instance: Instance{UID: m.UID, Name: m.Name, Namespace: m.Namespace, Executor: "native", Phase: "Running", PID: m.PID, Port: m.Port, Ports: m.Ports, StartedAt: m.StartedAt}}
	e.restoreHistory(r)
	done := r.done
	r.gone, r.goneOnce = make(chan struct{}), &sync.Once{}
	gone, goneOnce := r.gone, r.goneOnce
	r.launchLabel = m.Label
	stop, err := watchExit(m.PID, func() {
		var exit error = errDetachedExit
		if m.Label != "" {
			exit = e.waitLaunchdExit(m.Label)
		}
		w.finish()
		done <- exit
		goneOnce.Do(func() { close(gone) })
	})
	if err != nil {
		w.Close()
		return
	}
	r.stopWatch = stop
	go w.tail()
	// An adopted program's redirections are followed from now on; what it wrote while the agent was away stays
	// wherever it went.
	startRedirectFollower(m.PID, w, gone, time.Now())
	declared := m.Ports
	if len(declared) == 0 && m.Port > 0 {
		declared = []int{m.Port}
	}
	r.instance.Listening = m.Listeners
	if !listeningComplete(m.Listeners, declared) {
		e.startListenerDiscovery(r, m.PID, declared, gone)
	}
	e.entries[m.UID] = r
}
