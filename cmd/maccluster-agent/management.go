package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"maccluster/internal/cluster"
	"maccluster/internal/envs"
	runtime "maccluster/internal/runtime"
)

type profile struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Endpoint             string `json:"endpoint"`
	NodeName             string `json:"nodeName"`
	ManagementKubeconfig string `json:"managementKubeconfig,omitempty"`
	Leaving              bool   `json:"leaving,omitempty"`
	NodeUID              string `json:"nodeUID,omitempty"`
	RemovedAt            string `json:"removedAt,omitempty"`
	RemovalReason        string `json:"removalReason,omitempty"`
}
type clusterSession struct {
	profile        profile
	phase, message string // guarded by server.mu
	engine         runtime.Engine
	cfg            *rest.Config
	cancel         context.CancelFunc
	done           chan struct{}
}
type clusterStatus struct {
	ID                   string             `json:"id"`
	Name                 string             `json:"name"`
	Endpoint             string             `json:"endpoint"`
	NodeName             string             `json:"nodeName"`
	Phase                string             `json:"phase"`
	Message              string             `json:"message"`
	ManagementConfigured bool               `json:"managementConfigured"`
	Workloads            []runtime.Instance `json:"workloads"`
}
type status struct {
	Events         []lifecycleEvent   `json:"events"`
	NodeName       string             `json:"nodeName"`
	Cluster        string             `json:"cluster"`
	Phase          string             `json:"phase"`
	Message        string             `json:"message"`
	NetworkMode    string             `json:"networkMode"`
	Workloads      []runtime.Instance `json:"workloads"`
	Clusters       []clusterStatus    `json:"clusters"`
	LocalWorkloads []runtime.Instance `json:"localWorkloads"`
	KeepAwake      keepAwakeStatus    `json:"keepAwake"`
}
type server struct {
	journal             eventJournal
	eventsDirty         bool
	mu                  sync.Mutex
	operations          sync.Mutex
	localMu             sync.Mutex
	ctx                 context.Context
	dir, installationID string
	sessions            map[string]*clusterSession
	local               runtime.Engine
	definitions         map[string]runtime.Spec
	wg                  sync.WaitGroup
	// stopWorkloads is set by /v1/drain: the next shutdown stops every workload instead of leaving them running.
	stopWorkloads atomic.Bool
	shutdown      func() // asks serve to shut down
	awake         *keepAwakeLoop
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func privateJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.Rename(tmp, path)
}
func readJSON(path string, value any) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}
func (s *server) profileDir(id string) string {
	if id == "legacy" {
		return s.dir
	}
	return filepath.Join(s.dir, "clusters", id)
}
func newServer(ctx context.Context, dir, id string) (*server, error) {
	s := &server{ctx: ctx, dir: dir, installationID: id, sessions: map[string]*clusterSession{}, definitions: map[string]runtime.Spec{}}
	if err := s.loadEvents(); err != nil {
		return nil, err
	}
	var profiles []profile
	if err := readJSON(filepath.Join(dir, "profiles.json"), &profiles); err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles.json")); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(dir, "node.kubeconfig")); err == nil {
			cfg, e := clientcmd.BuildConfigFromFlags("", filepath.Join(dir, "node.kubeconfig"))
			name, ne := os.ReadFile(filepath.Join(dir, "node-name"))
			if e != nil || ne != nil {
				return nil, fmt.Errorf("legacy profile invalid")
			}
			profiles = append(profiles, profile{ID: "legacy", Name: cfg.Host, Endpoint: cfg.Host, NodeName: string(name)})
			if err = privateJSON(filepath.Join(dir, "profiles.json"), profiles); err != nil {
				return nil, err
			}
		}
	}
	if err := readJSON(filepath.Join(dir, "local-services.json"), &s.definitions); err != nil {
		return nil, fmt.Errorf("local service definitions: %w", err)
	}
	if s.definitions == nil {
		s.definitions = map[string]runtime.Spec{}
	}
	engine, err := runtime.NewWithEnvironments(filepath.Join(dir, "local-runtime"), filepath.Join(dir, "environments"))
	if err != nil {
		return nil, err
	}
	s.local = engine
	for _, p := range profiles {
		if p.ID != "legacy" {
			b, e := hex.DecodeString(p.ID)
			if e != nil || len(b) != 16 {
				engine.Close(context.Background())
				return nil, fmt.Errorf("invalid cluster profile id")
			}
		}
		session := &clusterSession{profile: p, phase: "Joining"}
		s.sessions[p.ID] = session
		cfg, e := clientcmd.BuildConfigFromFlags("", filepath.Join(s.profileDir(p.ID), "node.kubeconfig"))
		if e != nil {
			session.phase = "Error"
			session.message = "No valid saved node credentials; remove this incomplete profile and join again"
			continue
		}
		session.cfg = cfg
		eng, e := s.newProfileEngine(p.ID)
		if e != nil {
			session.phase = "Error"
			session.message = e.Error()
			continue
		}
		session.engine = eng
		if p.Leaving {
			session.phase = "Error"
			session.message = "Cluster exit incomplete; retry leave to finish remote cleanup"
		}
	}
	for _, session := range s.sessions {
		if session.profile.Leaving && session.profile.RemovedAt == "" && session.cfg != nil {
			verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := cluster.VerifyNode(verifyCtx, session.cfg, session.profile.NodeName, session.profile.NodeUID)
			cancel()
			if errors.Is(err, cluster.ErrNodeRemoved) {
				s.markRemoved(session, err.Error())
			}
		}
		if session.profile.RemovedAt != "" && session.engine != nil {
			s.markRemoved(session, session.profile.RemovalReason)
		}
		if session.engine != nil && !session.profile.Leaving && session.profile.RemovedAt == "" {
			s.launch(session)
		}
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			s.localMu.Lock()
			for _, spec := range s.definitions {
				s.local.Ensure(ctx, spec)
			}
			s.localMu.Unlock()
			s.mu.Lock()
			s.saveEventsLocked()
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.rotateCredentials(ctx)
	}()
	s.awake = newKeepAwakeLoop(loadSettings(dir).KeepAwake)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.awake.Run(ctx, func(c context.Context) int { return running(c, s.allEngines()...) })
	}()
	return s, nil
}
func (s *server) saveProfilesLocked() error {
	profiles := []profile{}
	for _, c := range s.sessions {
		profiles = append(profiles, c.profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return privateJSON(filepath.Join(s.dir, "profiles.json"), profiles)
}
func (s *server) launch(c *clusterSession) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	c.cancel = cancel
	c.done = make(chan struct{})
	cfg, name, id := c.cfg, c.profile.NodeName, c.profile.ID
	done := c.done
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		options, err := cluster.LoadNodeOptions(s.profileDir(id))
		if err != nil {
			s.mu.Lock()
			c.phase = "Error"
			c.message = err.Error()
			s.eventLocked(c.profile, "", "", "JoinFailed", "節點連線初始化失敗")
			s.saveEventsLocked()
			s.mu.Unlock()
			return
		}
		err = cluster.Run(ctx, cfg, name, s.installationID, c.engine, func(phase, message string) {
			s.mu.Lock()
			if !c.profile.Leaving {
				c.phase = phase
				c.message = message
				if phase == "Ready" {
					s.joinedLocked(c.profile)
				}
			}
			s.mu.Unlock()
		}, cluster.NodeIdentity{ObservePods: func(pods []cluster.PodObservation) { s.observePods(c, pods) }, UID: c.profile.NodeUID, Options: options, ShareCount: func() int {
			s.mu.Lock()
			defer s.mu.Unlock()
			count := 0
			for _, session := range s.sessions {
				if !session.profile.Leaving && session.profile.RemovedAt == "" {
					count++
				}
			}
			return count
		}, Save: func(uid string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			c.profile.NodeUID = uid
			return s.saveProfilesLocked()
		}})
		if errors.Is(err, cluster.ErrNodeRemoved) {
			s.markRemoved(c, err.Error())
			return
		}
		if err != nil && ctx.Err() == nil {
			s.mu.Lock()
			c.phase = "Error"
			c.message = fmt.Sprintf("%s: %v", id, err)
			s.eventLocked(c.profile, "", "", "JoinFailed", "節點連線控制器已停止")
			s.saveEventsLocked()
			s.mu.Unlock()
		}
	}()
}

// Persist the tombstone before stopping workloads. Keep the profile as an audit
// record; neither process restart nor a replacement Node may resume execution.
func (s *server) markRemoved(c *clusterSession, reason string) {
	s.mu.Lock()
	if c.profile.RemovedAt == "" {
		c.profile.RemovedAt = time.Now().UTC().Format(time.RFC3339)
		s.eventLocked(c.profile, "", "", "NodeRemoved", "偵測到節點已從叢集移除或身分已被取代")
		s.retirePodsLocked(c.profile)
		s.saveEventsLocked()
	}
	c.profile.RemovalReason = reason
	c.phase = "Removed"
	c.message = "節點已從叢集移除（" + c.profile.RemovedAt + "）：" + reason
	persistErr := s.saveProfilesLocked()
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var errs []error
	if persistErr != nil {
		errs = append(errs, fmt.Errorf("保存移除狀態失敗: %w", persistErr))
	}
	if c.engine != nil {
		for _, w := range c.engine.List(ctx) {
			if err := c.engine.Stop(ctx, w.UID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		s.mu.Lock()
		c.message += "；清理尚未完成：" + err.Error()
		s.mu.Unlock()
	}
}

func (s *server) close(ctx context.Context) error {
	s.mu.Lock()
	sessions := []*clusterSession{}
	for _, c := range s.sessions {
		sessions = append(sessions, c)
		if c.cancel != nil {
			c.cancel()
		}
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	// A normal stop leaves every workload running (programs, containers, launchd jobs): the next agent takes them
	// over, like a kubelet restart. Only a drain stops them (docs/agent-restart-design.md).
	finish := func(e runtime.Engine) error {
		if r, ok := e.(runtime.Releaser); ok && !s.stopWorkloads.Load() {
			return r.Release(ctx)
		}
		return e.Close(ctx)
	}
	var errs []error
	for _, c := range sessions {
		if c.engine != nil {
			errs = append(errs, finish(c.engine))
		}
	}
	errs = append(errs, finish(s.local))
	return errors.Join(errs...)
}

// drain stops every workload and then the agent: "stop the background service and everything it runs".
func (s *server) drain(w http.ResponseWriter, r *http.Request) {
	s.stopWorkloads.Store(true)
	jsonReply(w, map[string]bool{"draining": true})
	if s.shutdown != nil {
		go s.shutdown()
	}
}
func (s *server) getStatus(w http.ResponseWriter, r *http.Request) {
	result := status{Phase: "Ready", Message: "本地服務管理已啟動", NetworkMode: "host-proxy (no Pod overlay)", Clusters: []clusterStatus{}, Workloads: []runtime.Instance{}}
	s.mu.Lock()
	result.Events = append([]lifecycleEvent{}, s.journal.Events...)
	sessions := []*clusterSession{}
	for _, c := range s.sessions {
		sessions = append(sessions, c)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].profile.ID < sessions[j].profile.ID })
	engines := []runtime.Engine{}
	for _, c := range sessions {
		result.Clusters = append(result.Clusters, clusterStatus{ID: c.profile.ID, Name: c.profile.Name, Endpoint: c.profile.Endpoint, NodeName: c.profile.NodeName, Phase: c.phase, Message: c.message, ManagementConfigured: c.profile.ManagementKubeconfig != "", Workloads: []runtime.Instance{}})
		engines = append(engines, c.engine)
	}
	s.mu.Unlock()
	// Collect independently so a slow Docker daemon does not serialize cluster reads.
	var wg sync.WaitGroup
	for i, e := range engines {
		if e != nil {
			wg.Add(1)
			go func(i int, e runtime.Engine) { defer wg.Done(); result.Clusters[i].Workloads = e.List(r.Context()) }(i, e)
		}
	}
	result.LocalWorkloads = s.local.List(r.Context())
	wg.Wait()
	for _, c := range result.Clusters {
		result.Workloads = append(result.Workloads, c.Workloads...)
	}
	if len(result.Clusters) == 1 {
		c := result.Clusters[0]
		result.NodeName = c.NodeName
		result.Cluster = c.Endpoint
		result.Phase = c.Phase
		result.Message = c.Message
	}
	result.KeepAwake = s.awake.Status()
	jsonReply(w, result)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "Invalid request JSON", 400)
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "Expected one JSON object", 400)
		return false
	}
	return true
}
func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func (s *server) logs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("clusterID")
	uid := r.URL.Query().Get("uid")
	engine := s.local
	if id != "" {
		s.mu.Lock()
		c := s.sessions[id]
		if c != nil {
			engine = c.engine
		} else {
			engine = nil
		}
		s.mu.Unlock()
		if engine == nil {
			http.Error(w, "Cluster unavailable", 404)
			return
		}
	}
	b, err := engine.Logs(r.Context(), uid)
	if err != nil && id == "" { // v1 compatibility, only unambiguous cluster UID
		s.mu.Lock()
		engines := []runtime.Engine{}
		for _, c := range s.sessions {
			engines = append(engines, c.engine)
		}
		s.mu.Unlock()
		for _, engine := range engines {
			if engine != nil {
				if text, e := engine.Logs(r.Context(), uid); e == nil {
					b = text
					err = nil
					break
				}
			}
		}
	}
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, b)
}
func (s *server) join(w http.ResponseWriter, r *http.Request) {
	var req struct {
		cluster.JoinRequest
		Name                 string `json:"name"`
		ManagementKubeconfig string `json:"managementKubeconfig"`
	}
	if !decode(w, r, &req) {
		return
	}
	attempt := profile{ID: randomID(), Name: req.Name}
	accepted := false
	info, err := cluster.ParseJoin(req.Command)
	if attempt.Name == "" {
		attempt.Name = info.Endpoint
	}
	s.recordEvent(attempt, "JoinRequested", "收到加入叢集請求")
	defer func() {
		if !accepted {
			s.recordEvent(attempt, "JoinFailed", "加入叢集請求未完成")
		}
	}()
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Reject invalid explicit names before accepting an asynchronous join or saving a profile.
	if req.NodeName != "" {
		if err := cluster.ValidateNodeName(req.NodeName); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	for _, c := range s.sessions {
		if strings.EqualFold(c.profile.Endpoint, info.Endpoint) {
			s.mu.Unlock()
			http.Error(w, "This API server already has a cluster profile", 409)
			return
		}
	}
	s.mu.Unlock()
	id := attempt.ID
	if req.NodeName == "" {
		req.NodeName = "maccluster-" + s.installationID[:8] + "-" + id[:6]
	}
	if req.Name == "" {
		req.Name = info.Endpoint
	}
	dir := s.profileDir(id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	c := &clusterSession{profile: profile{ID: id, Name: req.Name, Endpoint: info.Endpoint, NodeName: req.NodeName}, phase: "Joining", message: "Verifying CA pin"}
	joinCtx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	c.cancel = cancel
	c.done = make(chan struct{})
	s.mu.Lock()
	s.sessions[id] = c
	if err = s.saveProfilesLocked(); err != nil {
		delete(s.sessions, id)
		s.mu.Unlock()
		cancel()
		http.Error(w, err.Error(), 500)
		return
	}
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		cfg, e := cluster.Bootstrap(joinCtx, dir, req.JoinRequest, func(message string) { s.mu.Lock(); c.message = message; s.mu.Unlock() })
		cancel()
		req.Command = ""
		// Serialize transition with leave. Signal bootstrap completion first so leave
		// cannot deadlock waiting while it owns the operations mutex.
		s.mu.Lock()
		close(c.done)
		s.mu.Unlock()
		s.operations.Lock()
		defer s.operations.Unlock()
		s.mu.Lock()
		leaving := c.profile.Leaving || s.sessions[id] != c
		s.mu.Unlock()
		if leaving || s.ctx.Err() != nil {
			return
		}
		if e == nil && req.ManagementKubeconfig != "" {
			_, e = cluster.ManagementConfig(s.ctx, cfg, req.ManagementKubeconfig)
		}
		var eng runtime.Engine
		if e == nil {
			eng, e = s.newProfileEngine(id)
		}
		s.mu.Lock()
		if e != nil {
			c.phase = "Error"
			c.message = e.Error()
			s.eventLocked(c.profile, "", "", "JoinFailed", "加入叢集失敗")
			s.saveEventsLocked()
			s.mu.Unlock()
			return
		}
		c.cfg = cfg
		c.engine = eng
		c.profile.ManagementKubeconfig = req.ManagementKubeconfig
		c.message = "Registering node"
		err := s.saveProfilesLocked()
		if err != nil {
			c.phase = "Error"
			c.message = err.Error()
			s.eventLocked(c.profile, "", "", "JoinFailed", "無法保存叢集設定")
			s.saveEventsLocked()
		}
		s.mu.Unlock()
		if err == nil {
			s.launch(c)
		}
	}()
	accepted = true
	w.WriteHeader(http.StatusAccepted)
	jsonReply(w, map[string]any{"accepted": true, "id": id})
}
func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var spec runtime.Spec
	if !decode(w, r, &spec) {
		return
	}
	if strings.TrimSpace(spec.Name) == "" {
		http.Error(w, "Service name is required", 400)
		return
	}
	if spec.UID != "" || spec.Namespace != "" {
		http.Error(w, "UID and namespace are assigned by the platform", 400)
		return
	}
	if spec.Executor == "" {
		spec.Executor = "docker"
	}
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = "Never"
	}
	if spec.Executor != "docker" && spec.Executor != "native" {
		http.Error(w, "Unknown executor", 400)
		return
	}
	if spec.Environment != "" && spec.Executor != "native" {
		http.Error(w, "Environment is only supported by native services", 400)
		return
	}
	if spec.Executor == "native" && (len(spec.Command) == 0 || (!filepath.IsAbs(spec.Command[0]) && (spec.Environment == "" || strings.ContainsRune(spec.Command[0], '/')))) {
		http.Error(w, "Native executable must be an absolute path (or a bare command name when an environment is selected)", 400)
		return
	}
	if spec.Environment != "" {
		if _, err := envs.Get(filepath.Join(s.dir, "environments"), spec.Environment); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	if spec.Executor == "docker" && spec.Image == "" {
		http.Error(w, "Docker image is required", 400)
		return
	}
	if spec.ContainerPort < 0 || spec.ContainerPort > 65535 {
		http.Error(w, "Invalid port", 400)
		return
	}
	if spec.RestartPolicy != "Never" && spec.RestartPolicy != "Always" && spec.RestartPolicy != "OnFailure" {
		http.Error(w, "Invalid restart policy", 400)
		return
	}
	s.localMu.Lock()
	defer s.localMu.Unlock()
	for _, existing := range s.definitions {
		if existing.Name == spec.Name {
			http.Error(w, "Local service name already exists", 409)
			return
		}
	}
	spec.UID = "local-" + randomID()
	spec.Namespace = "local"
	s.definitions[spec.UID] = spec
	if err := privateJSON(filepath.Join(s.dir, "local-services.json"), s.definitions); err != nil {
		delete(s.definitions, spec.UID)
		http.Error(w, err.Error(), 500)
		return
	}
	instance, err := s.local.Ensure(s.ctx, spec)
	if err != nil {
		instance.Message = err.Error()
	}
	w.WriteHeader(http.StatusCreated)
	jsonReply(w, instance)
}

type targetRequest struct {
	UID       string `json:"uid"`
	ClusterID string `json:"clusterID"`
	Mode      string `json:"mode"`
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	var req targetRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ClusterID == "" {
		s.localMu.Lock()
		defer s.localMu.Unlock()
		spec, ok := s.definitions[req.UID]
		if !ok {
			http.Error(w, "Unknown local service", 404)
			return
		}
		// Remove desired state first; if runtime cleanup fails restore it and report.
		delete(s.definitions, req.UID)
		if err := privateJSON(filepath.Join(s.dir, "local-services.json"), s.definitions); err != nil {
			s.definitions[req.UID] = spec
			http.Error(w, err.Error(), 500)
			return
		}
		if err := s.local.Stop(r.Context(), req.UID); err != nil {
			s.definitions[req.UID] = spec
			persistErr := privateJSON(filepath.Join(s.dir, "local-services.json"), s.definitions)
			http.Error(w, errors.Join(err, persistErr).Error(), 500)
			return
		}
		jsonReply(w, map[string]bool{"deleted": true})
		return
	}
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	c := s.sessions[req.ClusterID]
	s.mu.Unlock()
	if c == nil || c.cfg == nil {
		http.Error(w, "Cluster unavailable", 404)
		return
	}
	if req.Mode != "pod" && req.Mode != "workload" {
		http.Error(w, "Choose pod or workload deletion mode", 400)
		return
	}
	cfg := c.cfg
	if req.Mode == "workload" {
		if c.profile.ManagementKubeconfig == "" {
			http.Error(w, "Deleting the owning workload requires a management kubeconfig; deleting one Pod may recreate it", 403)
			return
		}
		var err error
		cfg, err = cluster.ManagementConfig(r.Context(), c.cfg, c.profile.ManagementKubeconfig)
		if err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
	}
	if err := cluster.DeleteAssigned(r.Context(), cfg, c.profile.NodeName, req.UID, req.Mode); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	jsonReply(w, map[string]bool{"deleted": true})
}
func (s *server) configure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClusterID            string `json:"clusterID"`
		ManagementKubeconfig string `json:"managementKubeconfig"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	c := s.sessions[req.ClusterID]
	s.mu.Unlock()
	if c == nil || c.cfg == nil {
		http.Error(w, "Cluster unavailable", 404)
		return
	}
	if _, err := cluster.ManagementConfig(r.Context(), c.cfg, req.ManagementKubeconfig); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	old := c.profile.ManagementKubeconfig
	c.profile.ManagementKubeconfig = req.ManagementKubeconfig
	err := s.saveProfilesLocked()
	if err != nil {
		c.profile.ManagementKubeconfig = old
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonReply(w, map[string]bool{"configured": true})
}
func (s *server) leave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClusterID string `json:"clusterID"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	c := s.sessions[req.ClusterID]
	if c == nil {
		s.mu.Unlock()
		http.Error(w, "Unknown cluster", 404)
		return
	}
	leaveProfile := c.profile
	left := false
	defer func() {
		if !left {
			s.recordEvent(leaveProfile, "LeaveFailed", "退出叢集未完成，請檢查狀態後重試")
		}
	}()
	s.eventLocked(leaveProfile, "", "", "LeaveRequested", "收到退出叢集請求")
	s.saveEventsLocked()
	oldLeaving, oldPhase, oldMessage := c.profile.Leaving, c.phase, c.message
	c.profile.Leaving = true
	c.phase = "Leaving"
	c.message = "Stopping this cluster's workloads"
	err := s.saveProfilesLocked()
	if err != nil {
		c.profile.Leaving, c.phase, c.message = oldLeaving, oldPhase, oldMessage
		s.mu.Unlock()
		http.Error(w, err.Error(), 500)
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	done := c.done
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-r.Context().Done():
			http.Error(w, "Timed out stopping cluster controller; retry exit", 504)
			return
		}
	}
	// Bootstrap may finish while leave owns operations; load its saved result.
	if c.cfg == nil {
		path := filepath.Join(s.profileDir(req.ClusterID), "node.kubeconfig")
		if cfg, e := clientcmd.BuildConfigFromFlags("", path); e == nil {
			s.mu.Lock()
			c.cfg = cfg
			s.mu.Unlock()
		} else if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			http.Error(w, "Cannot validate saved credentials for cluster cleanup", 409)
			return
		}
	}
	var errs []error
	if c.cfg != nil && c.engine == nil {
		eng, e := runtime.New(filepath.Join(s.profileDir(req.ClusterID), "runtime"))
		if e != nil {
			http.Error(w, "Cannot safely stop runtime: "+e.Error(), 409)
			return
		}
		s.mu.Lock()
		c.engine = eng
		s.mu.Unlock()
	}
	if c.engine != nil {
		c.engine.List(r.Context())
		errs = append(errs, c.engine.Close(r.Context()))
	}
	if c.cfg != nil && c.profile.RemovedAt == "" {
		cfg := c.cfg
		if c.profile.ManagementKubeconfig != "" {
			cfg, err = cluster.ManagementConfig(r.Context(), c.cfg, c.profile.ManagementKubeconfig)
			if err != nil {
				errs = append(errs, err)
			}
		}
		if err == nil {
			errs = append(errs, cluster.LeaveNode(r.Context(), cfg, c.profile.NodeName, s.installationID))
		}
	}
	if err = errors.Join(errs...); err != nil {
		s.mu.Lock()
		c.phase = "Error"
		c.message = "Local execution stopped; cluster exit incomplete: " + err.Error()
		s.mu.Unlock()
		http.Error(w, c.message, 409)
		return
	}
	// Persist removal before removing private files. Never remove local runtime or
	// another profile's directory; legacy credentials are retired explicitly.
	s.mu.Lock()
	delete(s.sessions, req.ClusterID)
	err = s.saveProfilesLocked()
	if err != nil {
		s.sessions[req.ClusterID] = c
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for _, name := range []string{"node.kubeconfig", "node-name"} {
		if e := os.Remove(filepath.Join(s.profileDir(req.ClusterID), name)); e != nil && !os.IsNotExist(e) {
			http.Error(w, "Cluster disconnected but credential cleanup failed: "+e.Error(), 500)
			return
		}
	}
	s.mu.Lock()
	s.retirePodsLocked(leaveProfile)
	s.eventLocked(leaveProfile, "", "", "LeaveSucceeded", "已退出叢集")
	s.saveEventsLocked()
	s.mu.Unlock()
	left = true
	jsonReply(w, map[string]bool{"left": true})
}

// rotateCredentials renews each joined cluster's Node client certificate before it expires. The certificate is
// requested as the Node itself (see cluster.RotateClientCert); after a renewal the session is restarted so the
// kubelet endpoint and API clients use the new certificate. Workloads are owned by the engine, not the session,
// so they keep running through the restart.
func (s *server) rotateCredentials(ctx context.Context) {
	interval := 10 * time.Minute
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.mu.Lock()
		sessions := []*clusterSession{}
		for _, c := range s.sessions {
			if c.cfg != nil && c.engine != nil && !c.profile.Leaving && c.profile.RemovedAt == "" {
				sessions = append(sessions, c)
			}
		}
		s.mu.Unlock()
		for _, c := range sessions {
			s.rotateSession(ctx, c)
		}
		timer.Reset(interval)
	}
}

func (s *server) rotateSession(ctx context.Context, c *clusterSession) {
	path := filepath.Join(s.profileDir(c.profile.ID), "node.kubeconfig")
	life, err := cluster.ClientCertLife(path)
	if err != nil || !life.NeedsRotation(time.Now()) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "cluster %s: read client certificate: %v\n", c.profile.ID, err)
		}
		return
	}
	rotateCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err = cluster.RotateClientCert(rotateCtx, path, func(string) {}); err != nil {
		fmt.Fprintf(os.Stderr, "cluster %s: client certificate renewal failed (expires %s): %v\n", c.profile.ID, life.NotAfter.UTC().Format(time.RFC3339), err)
		return
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster %s: load renewed credentials: %v\n", c.profile.ID, err)
		return
	}
	s.mu.Lock()
	cancelSession, done := c.cancel, c.done
	s.mu.Unlock()
	if cancelSession != nil {
		cancelSession()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
	s.mu.Lock()
	c.cfg = cfg
	active := !c.profile.Leaving && c.profile.RemovedAt == ""
	s.mu.Unlock()
	if active && ctx.Err() == nil {
		s.launch(c)
	}
	fmt.Fprintf(os.Stderr, "cluster %s: client certificate renewed\n", c.profile.ID)
}

// newProfileEngine opens a cluster profile's runtime with the native backend its node options select.
func (s *server) newProfileEngine(id string) (runtime.Engine, error) {
	options, err := cluster.LoadNodeOptions(s.profileDir(id))
	if err != nil {
		return nil, err
	}
	return runtime.NewWithOptions(filepath.Join(s.profileDir(id), "runtime"), filepath.Join(s.dir, "environments"),
		runtime.Options{NativeBackend: options.NativeBackend, LaunchdUID: options.LaunchdUID})
}
