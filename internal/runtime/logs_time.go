package runtime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// A native program writes its output straight into a file, so nothing records when each line was written. The agent
// follows the file every 200 ms and notes, in a side file next to each log (<log>.ts), the time at which each batch
// of new bytes was first seen. A line's time is the time of the batch that contained its first byte: accurate to
// about a second, which is what `kubectl logs --timestamps` and `--since` need.
//
// Run boundaries make `--previous` possible: when a program is started again, the previous run's log and index
// are moved to <key>.prev.log(.ts) (with their rotated generation), replacing whatever was kept before.

var ErrNoPreviousLogs = errors.New("there is no previous run of this container")

const indexRecordSize = 16

type indexEntry struct {
	end int64 // bytes of the log up to here had been seen ...
	at  int64 // ... at this time (Unix nanoseconds)
}

func indexPath(logPath string) string { return logPath + ".ts" }

func readIndex(logPath string) []indexEntry {
	data, err := os.ReadFile(indexPath(logPath))
	if err != nil {
		return nil
	}
	out := make([]indexEntry, 0, len(data)/indexRecordSize)
	for i := 0; i+indexRecordSize <= len(data); i += indexRecordSize {
		out = append(out, indexEntry{end: int64(binary.LittleEndian.Uint64(data[i:])), at: int64(binary.LittleEndian.Uint64(data[i+8:]))})
	}
	return out
}

// timeAt is the time of the batch that contained the byte at offset; ok is false when the byte is beyond what the
// index knows (written after its last record).
func timeAt(idx []indexEntry, offset int64) (time.Time, bool) {
	i := sort.Search(len(idx), func(i int) bool { return idx[i].end > offset })
	if i == len(idx) {
		return time.Time{}, false
	}
	return time.Unix(0, idx[i].at), true
}

// stampedLine is one line of a log, with the time it was first seen.
type stampedLine struct {
	at   time.Time
	text []byte // including its newline, if it has one
}

// linesOf splits one log file into lines and gives each its time: from the index, else (output that predates the
// index, or that the agent has not seen yet) from fallback.
func linesOf(data []byte, idx []indexEntry, fallback time.Time) []stampedLine {
	var lines []stampedLine
	var offset int64
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n') + 1
		if n == 0 {
			n = len(data)
		}
		at, ok := timeAt(idx, offset)
		if !ok {
			at = fallback
		}
		lines = append(lines, stampedLine{at: at, text: data[:n]})
		offset += int64(n)
		data = data[n:]
	}
	return lines
}

// loadRunLines reads one run of a workload's native log: the rotated generation followed by the live file. liveLimit
// cuts the live file at that many bytes (-1: no cut), so a follower can start from exactly what has been published.
func loadRunLines(logPath string, liveLimit int64) []stampedLine {
	var lines []stampedLine
	for _, p := range []string{logPath + ".1", logPath} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if p == logPath && liveLimit >= 0 && int64(len(data)) > liveLimit {
			data = data[:liveLimit]
		}
		fallback := time.Now()
		if st, serr := os.Stat(p); serr == nil {
			fallback = st.ModTime()
		}
		lines = append(lines, linesOf(data, readIndex(p), fallback)...)
	}
	return lines
}

func prevLogPath(dir, uid string) string { return filepath.Join(dir, key(uid)+".prev.log") }

// startNewRun moves the log of the run that just ended aside so the next one starts a file of its own.
func startNewRun(dir, uid string) {
	live := logFilePath(dir, uid)
	if st, err := os.Stat(live); err != nil || st.Size() == 0 {
		return
	}
	prev := prevLogPath(dir, uid)
	for _, suffix := range []string{"", ".ts", ".1", ".1.ts"} {
		os.Remove(prev + suffix)
	}
	for _, move := range [][2]string{{live + ".1", prev + ".1"}, {live + ".1.ts", prev + ".1.ts"}, {live, prev}, {live + ".ts", prev + ".ts"}} {
		os.Rename(move[0], move[1])
	}
}

// renderLines applies --since, --tail and --timestamps, in that order, as kubectl does.
func renderLines(lines []stampedLine, since time.Time, tail *int, timestamps bool) []byte {
	if !since.IsZero() {
		kept := lines[:0:0]
		for _, l := range lines {
			if !l.at.Before(since) {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	if tail != nil && *tail < len(lines) {
		if *tail <= 0 {
			return nil
		}
		lines = lines[len(lines)-*tail:]
	}
	var out bytes.Buffer
	for _, l := range lines {
		if timestamps {
			out.WriteString(l.at.UTC().Format(time.RFC3339Nano))
			out.WriteByte(' ')
		}
		out.Write(l.text)
	}
	return out.Bytes()
}

// sinceOf resolves --since-time and --since into a time.
func sinceOf(opts LogOptions, now time.Time) time.Time {
	if !opts.SinceTime.IsZero() {
		return opts.SinceTime
	}
	if opts.SinceSeconds > 0 {
		return now.Add(-time.Duration(opts.SinceSeconds) * time.Second)
	}
	return time.Time{}
}

// lineStamper prefixes the lines of a live feed with the time they arrive. A chunk may end in the middle of a line;
// the rest of that line must not get a second prefix.
type lineStamper struct{ midLine bool }

func (s *lineStamper) stamp(chunk []byte, at time.Time) []byte {
	if len(chunk) == 0 {
		return chunk
	}
	prefix := []byte(at.UTC().Format(time.RFC3339Nano) + " ")
	var out bytes.Buffer
	for len(chunk) > 0 {
		if !s.midLine {
			out.Write(prefix)
		}
		n := bytes.IndexByte(chunk, '\n') + 1
		if n == 0 {
			n = len(chunk)
			s.midLine = true
		} else {
			s.midLine = false
		}
		out.Write(chunk[:n])
		chunk = chunk[n:]
	}
	return out.Bytes()
}
