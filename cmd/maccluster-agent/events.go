package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"maccluster/internal/cluster"
)

const eventLimit = 1000

type lifecycleEvent struct {
	ID          string `json:"id"`
	Time        string `json:"time"`
	ClusterID   string `json:"clusterID"`
	ClusterName string `json:"clusterName"`
	Namespace   string `json:"namespace"`
	Workload    string `json:"workload"`
	Kind        string `json:"kind"`
	Level       string `json:"level"`
	Message     string `json:"message"`
}

// Observations and history are committed together so a daemon restart does not
// announce the same assigned Pods again. This file lives outside profile dirs.
type eventJournal struct {
	Events []lifecycleEvent                             `json:"events"`
	Pods   map[string]map[string]cluster.PodObservation `json:"pods"`
	Joined map[string]bool                              `json:"joined"`
}

func (s *server) loadEvents() error {
	if err := readJSON(filepath.Join(s.dir, "events.json"), &s.journal); err != nil {
		return fmt.Errorf("event journal: %w", err)
	}
	if len(s.journal.Events) > eventLimit {
		s.journal.Events = s.journal.Events[len(s.journal.Events)-eventLimit:]
	}
	for i := range s.journal.Events {
		e := &s.journal.Events[i]
		if e.Level == "" {
			e.Level = eventLevel(e.Kind, e.Message)
			s.eventsDirty = true
		}
	}
	s.saveEventsLocked()
	return nil
}

func eventLevel(kind, message string) string {
	switch kind {
	case "JoinFailed", "LeaveFailed":
		return "error"
	case "NodeRemoved":
		return "warning"
	case "PodPhaseChanged":
		if message == "Pod 執行狀態：Failed" {
			return "error"
		}
		if message == "Pod 執行狀態：Unknown" {
			return "warning"
		}
	}
	return "info"
}

// All journal methods hold server.mu. Persistence failures are reported without
// echoing any underlying path/error; the next reconcile retries the dirty state.
func (s *server) saveEventsLocked() {
	if !s.eventsDirty {
		return
	}
	if err := privateJSON(filepath.Join(s.dir, "events.json"), s.journal); err != nil {
		fmt.Fprintln(os.Stderr, "event journal persistence failed; will retry on reconciliation")
		return
	}
	s.eventsDirty = false
}
func (s *server) eventLocked(p profile, ns, workload, kind, message string) {
	s.journal.Events = append(s.journal.Events, lifecycleEvent{ID: randomID(), Time: time.Now().UTC().Format(time.RFC3339Nano), ClusterID: p.ID, ClusterName: p.Name, Namespace: ns, Workload: workload, Kind: kind, Level: eventLevel(kind, message), Message: message})
	if len(s.journal.Events) > eventLimit {
		s.journal.Events = append([]lifecycleEvent(nil), s.journal.Events[len(s.journal.Events)-eventLimit:]...)
	}
	s.eventsDirty = true
}
func (s *server) recordEvent(p profile, kind, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventLocked(p, "", "", kind, message)
	s.saveEventsLocked()
}
func (s *server) joinedLocked(p profile) {
	if s.journal.Joined == nil {
		s.journal.Joined = map[string]bool{}
	}
	if !s.journal.Joined[p.ID] {
		s.journal.Joined[p.ID] = true
		s.eventLocked(p, "", "", "JoinSucceeded", "節點已連線並完成註冊")
	}
	s.saveEventsLocked()
}
func safePodPhase(phase string) string {
	switch phase {
	case "Pending", "Running", "Succeeded", "Failed", "Unknown", "Terminating":
		return phase
	}
	return "Unknown"
}
func (s *server) observePods(c *clusterSession, pods []cluster.PodObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.profile.Leaving || c.profile.RemovedAt != "" {
		return
	}
	if s.journal.Pods == nil {
		s.journal.Pods = map[string]map[string]cluster.PodObservation{}
	}
	previous := s.journal.Pods[c.profile.ID]
	next := map[string]cluster.PodObservation{}
	for _, p := range pods {
		p.Phase = safePodPhase(p.Phase)
		next[p.UID] = p
		old, seen := previous[p.UID]
		if !seen {
			s.eventLocked(c.profile, p.Namespace, p.Name, "PodAssigned", "收到 Pod 指派")
		}
		if !seen || old.Phase != p.Phase {
			s.eventLocked(c.profile, p.Namespace, p.Name, "PodPhaseChanged", "Pod 執行狀態："+p.Phase)
		}
	}
	ids := make([]string, 0, len(previous))
	for id := range previous {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, ok := next[id]; !ok {
			p := previous[id]
			s.eventLocked(c.profile, p.Namespace, p.Name, "PodRemoved", "Pod 已撤除指派")
		}
	}
	s.journal.Pods[c.profile.ID] = next
	s.saveEventsLocked()
}
func (s *server) retirePodsLocked(p profile) {
	pods := s.journal.Pods[p.ID]
	ids := make([]string, 0, len(pods))
	for id := range pods {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		pod := pods[id]
		s.eventLocked(p, pod.Namespace, pod.Name, "PodRemoved", "Pod 已停止由此叢集管理")
	}
	delete(s.journal.Pods, p.ID)
	delete(s.journal.Joined, p.ID)
	s.eventsDirty = true
}
