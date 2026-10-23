package runtime

import (
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Where a native program listens. The agent asks `lsof` once the program has opened its declared ports and
// remembers the answer (in the process marker, so it survives an agent restart): `*` means every interface, so the
// LAN can reach it; `127.0.0.1` or `::1` means only this Mac. Nothing is polled once every declared port has been
// seen, and a program that is taken over again after an agent restart is not asked at all.

// Listener is one declared port and the addresses the program has it open on.
type Listener struct {
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
}

// These are variables so tests can shorten them.
var (
	listenerFirstDelay = time.Second
	listenerFastEvery  = 2 * time.Second
	listenerFastPhase  = time.Minute
	listenerSlowEvery  = 10 * time.Second
	// A program that has not opened a declared port within this time is given up on. Fixed ports belong to services
	// that may load a model for a long time; a dynamic port the program never uses is not worth asking about for long.
	listenerGiveUp        = time.Hour
	listenerGiveUpDynamic = 10 * time.Minute
)

// lsofListening lists the TCP sockets in LISTEN state of the given processes, in `-Fn` form.
var lsofListening = func(pids []int) string {
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	path, err := lsofPath()
	if err != nil {
		return ""
	}
	out, _ := exec.Command(path, "-a", "-p", strings.Join(list, ","), "-iTCP", "-sTCP:LISTEN", "-nP", "-Fn").Output()
	return string(out)
}

func lsofPath() (string, error) {
	if path, err := exec.LookPath("lsof"); err == nil {
		return path, nil
	}
	// GitHub-hosted macOS runners and GUI-launched apps may omit /usr/sbin from PATH.
	for _, path := range []string{"/usr/sbin/lsof", "/usr/bin/lsof"} {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}

// parseListening reads `lsof -Fn` output and returns, for each wanted port, the addresses it is open on.
func parseListening(out string, wanted []int) []Listener {
	want := map[int]bool{}
	for _, p := range wanted {
		want[p] = true
	}
	found := map[int]map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		name := strings.TrimPrefix(line, "n")
		i := strings.LastIndex(name, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(name[i+1:])
		if err != nil || !want[port] {
			continue
		}
		addr := strings.Trim(name[:i], "[]")
		if addr == "" {
			continue
		}
		if found[port] == nil {
			found[port] = map[string]bool{}
		}
		found[port][addr] = true
	}
	var out2 []Listener
	for port, addrs := range found {
		l := Listener{Port: port}
		for a := range addrs {
			l.Addresses = append(l.Addresses, a)
		}
		sort.Strings(l.Addresses)
		out2 = append(out2, l)
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].Port < out2[j].Port })
	return out2
}

// startListenerDiscovery finds out where the program with this pid listens, then stops. It looks soon after the start
// and then at growing intervals until every declared port has been seen, the program is gone or it gives up.
func (e *engine) startListenerDiscovery(r *entry, pid int, ports []int, gone <-chan struct{}) {
	if pid <= 0 || len(ports) == 0 || gone == nil {
		return
	}
	if _, err := lsofPath(); err != nil {
		return
	}
	giveUp := listenerGiveUpDynamic
	if len(r.instance.Ports) > 0 {
		giveUp = listenerGiveUp
	}
	// Snapshot test-overridable globals before launching the goroutine. Tests restore these values
	// during cleanup, while discovery may still be polling after an earlier assertion fails.
	firstDelay, fastEvery := listenerFirstDelay, listenerFastEvery
	fastPhase, slowEvery := listenerFastPhase, listenerSlowEvery
	findListeners := lsofListening
	go func() {
		begin := time.Now()
		delay := firstDelay
		for {
			select {
			case <-gone:
				return
			case <-time.After(delay):
			}
			got := parseListening(findListeners(processTree(pid)), ports)
			complete := len(got) == len(ports)
			expired := time.Since(begin) > giveUp
			if len(got) > 0 || expired {
				e.mu.Lock()
				if r.instance.PID == pid { // still the same run
					r.instance.Listening = got
					if (complete || expired) && r.marker != "" {
						updateMarker(r.marker, func(m *nativeMarker) { m.Listeners = got })
					}
				}
				e.mu.Unlock()
			}
			if complete || expired {
				return
			}
			delay = slowEvery
			if time.Since(begin) < fastPhase {
				delay = fastEvery
			}
		}
	}()
}

// updateMarker rewrites a process marker in place.
func updateMarker(path string, change func(*nativeMarker)) {
	if m, _, ok := readMarker(path); ok {
		change(&m)
		writeMarker(path, m)
	}
}

// listeningComplete says whether every declared port has an answer.
func listeningComplete(l []Listener, ports []int) bool {
	if len(ports) == 0 {
		return true
	}
	have := map[int]bool{}
	for _, x := range l {
		have[x.Port] = true
	}
	for _, p := range ports {
		if !have[p] {
			return false
		}
	}
	return true
}
