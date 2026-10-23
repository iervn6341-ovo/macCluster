package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// ReasonSpecChangedSinceStart is the Warning event for a program that was taken over although it was started from
// a spec that differs from the current one (an agent upgrade).
const ReasonSpecChangedSinceStart = "SpecChangedSinceStart"

// The agent can restart without the workloads noticing (docs/agent-restart-design.md). What a restart must not
// reset is what the Pod's readiness depends on: whether postStart has run, whether the startup probe passed and
// whether the readiness probe last passed. Without it a restart would run postStart again and show every Pod as
// not ready for a few seconds, which removes it from its Service's endpoints. Liveness starts over (healthy).
// Everything is keyed by the container start, so a program that was restarted in the meantime starts clean.

type probeSnapshot struct {
	StartedAt   string `json:"startedAt"`
	PostDone    bool   `json:"postDone,omitempty"`
	StartupOK   bool   `json:"startupOK,omitempty"`
	ReadinessOK bool   `json:"readinessOK,omitempty"`
}

type probeStore struct {
	path string
	last []byte
}

func newProbeStore(stateDir string) *probeStore {
	if stateDir == "" {
		return &probeStore{}
	}
	return &probeStore{path: filepath.Join(stateDir, "probe-state.json")}
}

// load puts the saved state into probes (which is empty at that point).
func (ps *probeStore) load(probes map[string]*probeState) {
	if ps.path == "" {
		return
	}
	data, err := os.ReadFile(ps.path)
	if err != nil {
		return
	}
	var saved map[string]probeSnapshot
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	for uid, snap := range saved {
		if snap.StartedAt == "" {
			continue
		}
		st := &probeState{}
		st.post = hookRun{runFor: snap.StartedAt, done: snap.PostDone}
		st.startup.reset(snap.StartedAt, snap.StartupOK)
		st.liveness.reset(snap.StartedAt, true)
		st.readiness.reset(snap.StartedAt, snap.ReadinessOK)
		probes[uid] = st
	}
}

// save writes the state if it changed since the last save.
func (ps *probeStore) save(probes map[string]*probeState) {
	if ps.path == "" {
		return
	}
	out := map[string]probeSnapshot{}
	uids := make([]string, 0, len(probes))
	for uid := range probes {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		st := probes[uid]
		st.mu.Lock()
		started := st.readiness.started
		if started == "" {
			started = st.startup.started
		}
		if started == "" {
			started = st.post.runFor
		}
		if started != "" {
			snap := probeSnapshot{StartedAt: started}
			if st.post.runFor == started {
				snap.PostDone = st.post.done && st.post.err == ""
			}
			if st.startup.started == started {
				snap.StartupOK = st.startup.ok
			}
			if st.readiness.started == started {
				snap.ReadinessOK = st.readiness.ok
			}
			out[uid] = snap
		}
		st.mu.Unlock()
	}
	data, err := json.Marshal(out)
	if err != nil || string(data) == string(ps.last) {
		return
	}
	tmp := ps.path + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil && os.Rename(tmp, ps.path) == nil {
		ps.last = data
	}
}
