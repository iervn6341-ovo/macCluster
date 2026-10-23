package runtime

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// A native program may send its own output somewhere else than the pipe the agent gave it: a start-up script
// that runs `vllm serve ... >> /tmp/x/log.txt 2>&1` leaves the agent's log empty. The agent therefore looks, after the
// start, at where the standard output and standard error of the program and its descendants really go. A
// regular file found there is followed and its new content is appended to the service's log, which makes it
// readable with `kubectl logs` and in the app like any other output. Nothing is parsed: the answer comes from
// the open file descriptors, so variables, `exec >` and subshells need no special handling.

const (
	maxRedirectSources     = 4
	redirectReadChunk      = 64 * 1024
	redirectFastDiscovery  = time.Second
	redirectSlowDiscovery  = 10 * time.Second
	redirectFastPhase      = 30 * time.Second
	redirectFollowInterval = 200 * time.Millisecond
	// A file created this long before the program started still counts as this run's own output.
	redirectBirthSlack = 2 * time.Second
)

// processTree lists root and every descendant of it (and the members of root's process group).
func processTree(root int) []int {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return []int{root}
	}
	children := map[int][]int{}
	var group []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		pgid, _ := strconv.Atoi(f[2])
		children[ppid] = append(children[ppid], pid)
		if pgid == root {
			group = append(group, pid)
		}
	}
	seen := map[int]bool{root: true}
	queue := []int{root}
	for _, g := range group {
		if !seen[g] {
			seen[g] = true
			queue = append(queue, g)
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, c := range children[queue[i]] {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	return queue
}

// parseLsofFiles reads `lsof -Fpftn` output and returns the regular files (type REG) found, in order, without
// duplicates. Devices, pipes and sockets are not files to follow.
func parseLsofFiles(out string) []string {
	var files []string
	seen := map[string]bool{}
	typ, name := "", ""
	flush := func() {
		if typ == "REG" && strings.HasPrefix(name, "/") && !seen[name] {
			seen[name] = true
			files = append(files, name)
		}
		typ, name = "", ""
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			flush()
		case 'f':
			flush()
		case 't':
			typ = line[1:]
		case 'n':
			name = line[1:]
		}
	}
	flush()
	return files
}

// redirectTargets are the regular files that fd 1 and fd 2 of the program tree point to.
func redirectTargets(root int) []string {
	pids := processTree(root)
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	// lsof exits non-zero when some pid is already gone; the output it did produce is still valid.
	out, _ := exec.Command("lsof", "-a", "-p", strings.Join(list, ","), "-d", "1,2", "-Fpftn").Output()
	return parseLsofFiles(string(out))
}

// redirectSource follows one file.
type redirectSource struct {
	path   string
	file   *os.File
	offset int64
	info   os.FileInfo
}

// openRedirectSource starts following path. A file created by this run (born after the program started) is
// read from its beginning, so the first lines are not lost to the delay of finding it; a file that existed
// before the run only contributes what is written from now on.
func openRedirectSource(path string, since time.Time) (*redirectSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("not a regular file")
	}
	src := &redirectSource{path: path, file: f, info: info}
	if born := birthTime(info); !born.IsZero() && !since.IsZero() && !born.Before(since.Add(-redirectBirthSlack)) {
		src.offset = 0
	} else {
		src.offset = info.Size()
	}
	return src, nil
}

// next returns what has been appended since the last call. A shrunk or replaced file is followed from its start.
func (s *redirectSource) next() []byte {
	if cur, err := os.Stat(s.path); err == nil && !os.SameFile(cur, s.info) {
		if nf, err := os.Open(s.path); err == nil {
			s.file.Close()
			s.file, s.info, s.offset = nf, cur, 0
		}
	}
	st, err := s.file.Stat()
	if err != nil {
		return nil
	}
	if st.Size() < s.offset {
		s.offset = 0
	}
	if st.Size() == s.offset {
		return nil
	}
	n := st.Size() - s.offset
	if n > redirectReadChunk {
		n = redirectReadChunk
	}
	buf := make([]byte, n)
	m, err := s.file.ReadAt(buf, s.offset)
	if m > 0 {
		s.offset += int64(m)
	}
	if err != nil && err != io.EOF {
		return nil
	}
	return buf[:m]
}

// followRedirects runs until gone is closed: it finds the files the program writes its output to and copies
// their new content into the service's log. The copy is an append to the same file the program itself writes, so
// it goes through the normal publishing (followers, time index, rotation) like any other output.
func followRedirects(root int, w *boundedWriter, gone <-chan struct{}, since time.Time) {
	var sources []*redirectSource
	known := map[string]bool{}
	started := time.Now()
	nextDiscovery := time.Time{}
	ticker := time.NewTicker(redirectFollowInterval)
	defer ticker.Stop()
	defer func() {
		for _, s := range sources {
			s.file.Close()
		}
	}()
	drain := func() {
		for _, s := range sources {
			for {
				p := s.next()
				if len(p) == 0 {
					break
				}
				if _, err := w.file.Write(p); err != nil {
					return
				}
			}
		}
	}
	for {
		select {
		case <-gone:
			drain()
			return
		case <-ticker.C:
		}
		if now := time.Now(); !now.Before(nextDiscovery) {
			interval := redirectSlowDiscovery
			if now.Sub(started) < redirectFastPhase {
				interval = redirectFastDiscovery
			}
			nextDiscovery = now.Add(interval)
			if len(sources) < maxRedirectSources {
				logInfo, _ := w.file.Stat()
				for _, path := range redirectTargets(root) {
					if known[path] || len(sources) >= maxRedirectSources {
						continue
					}
					if st, err := os.Stat(path); err != nil || (logInfo != nil && os.SameFile(st, logInfo)) {
						continue
					}
					known[path] = true
					src, err := openRedirectSource(path, since)
					if err != nil {
						continue
					}
					sources = append(sources, src)
					w.file.Write([]byte("[maccluster] following output redirected to " + path + "\n"))
				}
			}
		}
		drain()
	}
}

// startRedirectFollower follows the redirections of the program with the given pid until it is gone. Several
// writers to the same file are never an issue: the log file is opened for append.
func startRedirectFollower(pid int, w *boundedWriter, gone <-chan struct{}, since time.Time) {
	if pid <= 0 || gone == nil || w == nil {
		return
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		return
	}
	go followRedirects(pid, w, gone, since)
}
