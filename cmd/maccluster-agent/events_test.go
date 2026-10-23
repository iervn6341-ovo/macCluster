package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maccluster/internal/cluster"
)

func TestEventJournalReconcileDeduplicatesAcrossRestart(t *testing.T) {
	s, _ := managementServer(t)
	c := &clusterSession{profile: profile{ID: "one", Name: "cluster one"}}
	pods := []cluster.PodObservation{{UID: "uid", Namespace: "test", Name: "worker", Phase: "Pending"}}
	s.observePods(c, pods)
	s.observePods(c, pods)
	if len(s.journal.Events) != 2 {
		t.Fatalf("duplicate observation: %+v", s.journal.Events)
	}
	s2, _ := managementServer(t)
	s2.dir = s.dir
	if err := s2.loadEvents(); err != nil {
		t.Fatal(err)
	}
	s2.observePods(c, pods)
	if len(s2.journal.Events) != 2 {
		t.Fatal("restart duplicated observations")
	}
	pods[0].Phase = "Running"
	s2.observePods(c, pods)
	if got := s2.journal.Events[2]; got.Kind != "PodPhaseChanged" || got.Message != "Pod 執行狀態：Running" {
		t.Fatalf("phase: %+v", got)
	}
	s2.observePods(c, nil)
	s2.observePods(c, nil)
	if len(s2.journal.Events) != 4 || s2.journal.Events[3].Kind != "PodRemoved" {
		t.Fatalf("removal: %+v", s2.journal.Events)
	}
	info, err := os.Stat(filepath.Join(s.dir, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %o", info.Mode().Perm())
	}
	for _, e := range s2.journal.Events {
		if _, err := time.Parse(time.RFC3339, e.Time); err != nil || !strings.HasSuffix(e.Time, "Z") || e.ID == "" {
			t.Fatalf("event identity/time: %+v", e)
		}
	}
}

func TestEventJournalRetentionAndSafePhase(t *testing.T) {
	s, _ := managementServer(t)
	p := profile{ID: "one", Name: "cluster"}
	for i := 0; i < eventLimit+20; i++ {
		s.eventLocked(p, "", "", "JoinRequested", "收到加入叢集請求")
	}
	firstKept := s.journal.Events[0].ID
	s.saveEventsLocked()
	s2, _ := managementServer(t)
	s2.dir = s.dir
	if err := s2.loadEvents(); err != nil {
		t.Fatal(err)
	}
	if len(s2.journal.Events) != eventLimit || s2.journal.Events[0].ID != firstKept {
		t.Fatal("retention/reload")
	}
	s2.observePods(&clusterSession{profile: p}, []cluster.PodObservation{{UID: "uid", Name: "pod", Phase: "secret-token"}})
	b, _ := json.Marshal(s2.journal)
	if strings.Contains(string(b), "secret-token") {
		t.Fatal("unknown phase leaked into journal")
	}
}

func TestEventJournalLeaveRetainsHistoryAndStatusDoesNotCollect(t *testing.T) {
	s, _ := managementServer(t)
	c := &clusterSession{profile: profile{ID: "one", Name: "cluster"}}
	s.sessions[c.profile.ID] = c
	s.observePods(c, []cluster.PodObservation{{UID: "uid", Name: "pod", Phase: "Running"}})
	s.joinedLocked(c.profile)
	s.joinedLocked(c.profile)
	if len(s.journal.Events) != 3 {
		t.Fatal("Ready ticks duplicated join")
	}
	w := managementRequest(s.leave, `{"clusterID":"one"}`)
	if w.Code != 200 {
		t.Fatalf("leave: %d %s", w.Code, w.Body.String())
	}
	if len(s.journal.Events) != 6 || s.journal.Events[5].Kind != "LeaveSucceeded" {
		t.Fatalf("leave events: %+v", s.journal.Events)
	}
	if len(s.journal.Pods) != 0 {
		t.Fatal("left cluster observations retained")
	}
	s2, _ := managementServer(t)
	s2.dir = s.dir
	if err := s2.loadEvents(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		w = managementRequest(s2.getStatus, "")
		var got status
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Events) != 6 {
			t.Fatal("status changed journal")
		}
	}
}

func TestEventJournalRemovedAndLeaveFailure(t *testing.T) {
	s, _ := managementServer(t)
	c := &clusterSession{profile: profile{ID: "one"}, engine: &managementEngine{stopErr: nil}}
	s.sessions["one"] = c
	s.markRemoved(c, "raw sensitive diagnostic")
	s.markRemoved(c, "raw sensitive diagnostic")
	if len(s.journal.Events) != 1 || s.journal.Events[0].Kind != "NodeRemoved" {
		t.Fatal("duplicate external removal")
	}
	// A cancelled controller prevents completion; record the failure without its details.
	done := make(chan struct{})
	c.done = done
	// Persistence failure gives a synchronous leave failure without touching a cluster.
	s.dir = filepath.Join(s.dir, "missing")
	w := managementRequest(s.leave, `{"clusterID":"one"}`)
	if w.Code != 500 || s.journal.Events[len(s.journal.Events)-1].Kind != "LeaveFailed" {
		t.Fatal("missing leave failure")
	}
	b, _ := json.Marshal(s.journal)
	if strings.Contains(string(b), "raw sensitive diagnostic") {
		t.Fatal("raw error leaked")
	}
}

func TestEventJournalLevelsAndLegacyMigration(t *testing.T) {
	s, _ := managementServer(t)
	cases := []struct{ kind, message, want string }{
		{"JoinFailed", "加入叢集失敗", "error"},
		{"LeaveFailed", "退出叢集未完成", "error"},
		{"NodeRemoved", "節點已移除", "warning"},
		{"PodPhaseChanged", "Pod 執行狀態：Failed", "error"},
		{"PodPhaseChanged", "Pod 執行狀態：Unknown", "warning"},
		{"PodPhaseChanged", "Pod 執行狀態：Running", "info"},
		{"JoinSucceeded", "已加入", "info"},
	}
	legacy := eventJournal{}
	for _, c := range cases {
		legacy.Events = append(legacy.Events, lifecycleEvent{ID: randomID(), Kind: c.kind, Message: c.message})
	}
	if err := privateJSON(filepath.Join(s.dir, "events.json"), legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.loadEvents(); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		if got := s.journal.Events[i].Level; got != c.want {
			t.Fatalf("%s: %s want %s", c.kind, got, c.want)
		}
	}
	var saved eventJournal
	if err := readJSON(filepath.Join(s.dir, "events.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Events[0].Level != "error" {
		t.Fatal("migration was not persisted")
	}
	b, _ := json.Marshal(saved)
	if strings.Contains(string(b), "\\u001b") {
		t.Fatal("ANSI escape persisted")
	}
}

func TestEventJournalRejectedJoinIsSafe(t *testing.T) {
	s, _ := managementServer(t)
	w := managementRequest(s.join, `{"command":"not-a-join --token secret-token"}`)
	if w.Code != 400 {
		t.Fatalf("join status: %d", w.Code)
	}
	if len(s.journal.Events) != 2 || s.journal.Events[0].Kind != "JoinRequested" || s.journal.Events[1].Kind != "JoinFailed" {
		t.Fatalf("events: %+v", s.journal.Events)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "events.json"))
	if strings.Contains(string(b), "secret-token") {
		t.Fatal("command leaked")
	}
}
