package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"maccluster/internal/power"
	runtime "maccluster/internal/runtime"
)

// agentSettings are the agent-wide settings the app can change (agent-settings.json in the state directory).
type agentSettings struct {
	KeepAwake string `json:"keepAwake,omitempty"` // power.WhenWorkloads (default), power.Always or power.Never
}

type keepAwakeStatus struct {
	Mode string `json:"mode"`
	Held bool   `json:"held"`
}

// keepAwakeLoop holds the Mac awake according to the mode while workloads run (docs/keep-awake-design.md).
type keepAwakeLoop struct {
	keeper *power.Keeper
	mu     sync.Mutex
	mode   string
}

func newKeepAwakeLoop(mode string) *keepAwakeLoop {
	if mode == "" {
		mode = power.WhenWorkloads
	}
	return &keepAwakeLoop{keeper: power.New(), mode: mode}
}

func (k *keepAwakeLoop) Mode() string {
	if k == nil {
		return power.WhenWorkloads
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.mode
}

func (k *keepAwakeLoop) SetMode(mode string) {
	k.mu.Lock()
	k.mode = mode
	k.mu.Unlock()
}

func (k *keepAwakeLoop) Status() keepAwakeStatus {
	if k == nil {
		return keepAwakeStatus{Mode: power.WhenWorkloads}
	}
	return keepAwakeStatus{Mode: k.Mode(), Held: k.keeper.Held()}
}

// running counts the workloads that are running right now.
func running(ctx context.Context, engines ...runtime.Engine) int {
	n := 0
	for _, e := range engines {
		if e == nil {
			continue
		}
		for _, i := range e.List(ctx) {
			if i.Phase == "Running" {
				n++
			}
		}
	}
	return n
}

// Run evaluates every few seconds until ctx ends; count says how many workloads run.
func (k *keepAwakeLoop) Run(ctx context.Context, count func(context.Context) int) {
	defer k.keeper.Close()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		k.keeper.Update(k.Mode(), count(ctx))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func loadSettings(dir string) agentSettings {
	var s agentSettings
	readJSON(filepath.Join(dir, "agent-settings.json"), &s)
	if !power.ValidMode(s.KeepAwake) {
		s.KeepAwake = ""
	}
	return s
}

// settings changes the agent-wide settings.
func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KeepAwake *string `json:"keepAwake"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.KeepAwake != nil {
		if !power.ValidMode(*req.KeepAwake) || *req.KeepAwake == "" {
			http.Error(w, "keepAwake must be when-workloads, always or never", 400)
			return
		}
		if err := privateJSON(filepath.Join(s.dir, "agent-settings.json"), agentSettings{KeepAwake: *req.KeepAwake}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.awake.SetMode(*req.KeepAwake)
		// Apply it now rather than at the next tick.
		s.awake.keeper.Update(*req.KeepAwake, running(r.Context(), s.allEngines()...))
	}
	json.NewEncoder(w).Encode(map[string]any{"keepAwake": s.awake.Status()})
}

func (s *server) allEngines() []runtime.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	engines := []runtime.Engine{s.local}
	for _, c := range s.sessions {
		engines = append(engines, c.engine)
	}
	return engines
}
