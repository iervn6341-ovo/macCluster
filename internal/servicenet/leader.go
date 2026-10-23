package servicenet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// ElectionConfig is how replicas of the controller agree on which one reconciles. Only the holder of a Lease works;
// the others wait and take over within about LeaseDuration after it stops renewing, or at once when it lets go.
type ElectionConfig struct {
	Namespace     string
	Name          string
	Identity      string
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// DefaultElection are the timings the design recommends: a failed leader is replaced in about 15 to 20 seconds.
func DefaultElection() ElectionConfig {
	return ElectionConfig{Name: "maccluster-service-controller", LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}
}

func (c ElectionConfig) Validate() error {
	switch {
	case c.Namespace == "":
		return fmt.Errorf("leader election needs the namespace of its Lease")
	case c.Name == "":
		return fmt.Errorf("leader election needs a Lease name")
	case c.Identity == "":
		return fmt.Errorf("leader election needs an identity (the Pod name)")
	case c.RetryPeriod <= 0 || c.RenewDeadline <= c.RetryPeriod || c.LeaseDuration <= c.RenewDeadline:
		return fmt.Errorf("leader election timings must satisfy retry < renew deadline < lease duration")
	}
	return nil
}

// Health is what the probes look at: a heartbeat that proves the process is alive and scheduling work, whether this
// replica currently leads, and how the last reconcile went.
type Health struct {
	mu        sync.Mutex
	now       func() time.Time
	beat      time.Time
	leader    bool
	lastRun   time.Time
	lastError string
	maxAge    time.Duration
}

func NewHealth(maxAge time.Duration) *Health {
	return &Health{now: time.Now, maxAge: maxAge, beat: time.Now()}
}

func (h *Health) Beat() { h.mu.Lock(); h.beat = h.now(); h.mu.Unlock() }
func (h *Health) SetLeader(v bool) {
	h.mu.Lock()
	h.leader = v
	h.mu.Unlock()
}
func (h *Health) ReconcileDone(err error) {
	h.mu.Lock()
	h.lastRun = h.now()
	h.lastError = ""
	if err != nil {
		h.lastError = err.Error()
	}
	h.mu.Unlock()
}

// Handler serves /healthz (liveness: the heartbeat is recent; a standby replica is healthy) and /leader (whether
// this replica leads, with the last reconcile, for people).
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		age := h.now().Sub(h.beat)
		h.mu.Unlock()
		if age > h.maxAge {
			http.Error(w, fmt.Sprintf("no heartbeat for %s", age.Round(time.Second)), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/leader", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		out := map[string]any{"leader": h.leader, "lastReconcile": h.lastRun.UTC().Format(time.RFC3339), "lastError": h.lastError}
		leader := h.leader
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !leader {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(out)
	})
	return mux
}

// Work is what the leader does until its context ends.
func ReconcileLoop(c *Controller, h *Health, every time.Duration, logf func(string, ...any)) func(ctx context.Context) {
	return func(ctx context.Context) {
		for {
			err := c.Reconcile(ctx)
			if h != nil {
				h.ReconcileDone(err)
			}
			if err != nil && logf != nil && ctx.Err() == nil {
				logf("Service controller: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(every):
			}
		}
	}
}

// RunElected runs work only while this replica holds the Lease. Losing it stops the work (its context ends) and the
// replica goes back to waiting; letting go on shutdown hands over at once. It returns when ctx ends.
func RunElected(ctx context.Context, client kubernetes.Interface, cfg ElectionConfig, h *Health, work func(ctx context.Context), logf func(string, ...any)) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: cfg.Name, Namespace: cfg.Namespace},
		Client:     client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: cfg.Identity},
	}
	// The heartbeat belongs to the process, not to leadership: a standby replica is healthy.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			if h != nil {
				h.Beat()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	for ctx.Err() == nil {
		le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock: lock, LeaseDuration: cfg.LeaseDuration, RenewDeadline: cfg.RenewDeadline, RetryPeriod: cfg.RetryPeriod,
			ReleaseOnCancel: true, Name: cfg.Name,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(c context.Context) {
					if h != nil {
						h.SetLeader(true)
					}
					if logf != nil {
						logf("this replica (%s) is now the leader", cfg.Identity)
					}
					work(c)
				},
				OnStoppedLeading: func() {
					if h != nil {
						h.SetLeader(false)
					}
				},
				OnNewLeader: func(id string) {
					if logf != nil && id != cfg.Identity {
						logf("the leader is %s", id)
					}
				},
			},
		})
		if err != nil {
			return err
		}
		le.Run(ctx) // returns when leadership is lost or ctx ends; then try again
	}
	return nil
}
