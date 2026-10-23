// Package servicenet implements opt-in TCP service connectivity. It does not
// allocate Pod IPs or claim CNI/NetworkPolicy conformance.
package servicenet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	core "k8s.io/api/core/v1"
)

const NetworkLabel = "maccluster.example.com/network"

// PortsCondition is the Pod status condition that carries the Pod's published ports (a JSON map of container port or
// port name to the Mac's entry port in its message). It lives in the Pod's *status* because that is what a node is
// allowed to write: the Node authorizer refuses a node's changes to a Pod's annotations.
const PortsCondition = "maccluster.example.com/service-ports"

// PortsAnnotation is where older agents published the same map. Only read, as a fallback for a Pod whose condition
// has not been written yet; nothing writes it any more.
const PortsAnnotation = "maccluster.example.com/service-ports"
const SelectorAnnotation = "maccluster.example.com/service-selector"
const ManagedBy = "maccluster-service-controller"

type Forwarder struct {
	mu     sync.Mutex
	routes map[string]*route
	access *access
	// Logf, when set, is told about refused sources (once a minute per source).
	Logf func(format string, args ...any)
}

// access is the source allow list shared by every route of a Forwarder. The entry ports are reachable by every
// host that can reach this Mac, so an allow list restricts who may use them: only the cluster's nodes need to.
// An empty list allows everyone (the behavior before the list existed).
type access struct {
	mu      sync.RWMutex
	nets    []*net.IPNet
	denied  atomic.Int64
	lastLog map[string]time.Time
}

// ParseAllowFrom reads CIDRs ("192.0.2.0/24") and single addresses ("192.0.2.82").
func ParseAllowFrom(list []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("serviceNetworkAllowFrom: %q is not an address or a CIDR", item)
			}
			if v4 := ip.To4(); v4 != nil {
				out = append(out, &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)})
			} else {
				out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)})
			}
			continue
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("serviceNetworkAllowFrom: %q is not a valid CIDR", item)
		}
		out = append(out, n)
	}
	return out, nil
}

func (a *access) allows(remote net.Addr) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.nets) == 0 {
		return true
	}
	tcp, ok := remote.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip := tcp.IP
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range a.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// SetAllowFrom replaces the allow list; it applies to every route at once, existing and future. Connections that are
// already open are not cut. An empty list allows everyone.
func (f *Forwarder) SetAllowFrom(nets []*net.IPNet) {
	f.access.mu.Lock()
	f.access.nets = nets
	f.access.mu.Unlock()
}

// Denied is how many connections were refused because of their source address.
func (f *Forwarder) Denied() int64 { return f.access.denied.Load() }

func (f *Forwarder) refuse(c net.Conn) {
	f.access.denied.Add(1)
	src := ""
	if tcp, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		src = tcp.IP.String()
	}
	if f.Logf != nil {
		f.access.mu.Lock()
		last := f.access.lastLog[src]
		now := time.Now()
		log := now.Sub(last) > time.Minute
		if log {
			f.access.lastLog[src] = now
		}
		f.access.mu.Unlock()
		if log {
			f.Logf("service forwarding refused a connection from %s: not in serviceNetworkAllowFrom", src)
		}
	}
	c.Close()
}

type route struct {
	owner       *Forwarder
	listener    net.Listener
	target      string
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]struct{}
}

func NewForwarder() *Forwarder {
	return &Forwarder{routes: map[string]*route{}, access: &access{lastLog: map[string]time.Time{}}}
}
func (f *Forwarder) Ensure(key, bindIP string, targetPort int) (int, error) {
	ip := net.ParseIP(bindIP)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || targetPort < 1 || targetPort > 65535 {
		return 0, fmt.Errorf("invalid service forwarding address")
	}
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort))
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.routes[key]; r != nil {
		if r.target == target && r.listener.Addr().(*net.TCPAddr).IP.Equal(ip) {
			return r.listener.Addr().(*net.TCPAddr).Port, nil
		}
		r.close()
		delete(f.routes, key)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return 0, err
	}
	r := &route{owner: f, listener: ln, target: target, connections: map[net.Conn]struct{}{}}
	f.routes[key] = r
	go r.serve()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
func (r *route) track(c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		c.Close()
		return false
	}
	r.connections[c] = struct{}{}
	return true
}
func (r *route) untrack(c net.Conn) { c.Close(); r.mu.Lock(); delete(r.connections, c); r.mu.Unlock() }
func (r *route) serve() {
	slots := make(chan struct{}, 128)
	for {
		c, err := r.listener.Accept()
		if err != nil {
			return
		}
		if !r.owner.access.allows(c.RemoteAddr()) {
			r.owner.refuse(c)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		if !r.track(c) {
			<-slots
			return
		}
		go func() {
			defer func() { <-slots; r.untrack(c) }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			upstream, err := (&net.Dialer{}).DialContext(ctx, "tcp", r.target)
			if err != nil {
				return
			}
			if !r.track(upstream) {
				return
			}
			defer r.untrack(upstream)
			done := make(chan struct{})
			go func() {
				io.Copy(upstream, c)
				if t, ok := upstream.(*net.TCPConn); ok {
					t.CloseWrite()
				}
				close(done)
			}()
			io.Copy(c, upstream)
			if t, ok := c.(*net.TCPConn); ok {
				t.CloseWrite()
			}
			c.Close()
			upstream.Close()
			<-done
		}()
	}
}
func (r *route) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.listener.Close()
	for c := range r.connections {
		c.Close()
	}
}
func (f *Forwarder) Retain(wanted map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, r := range f.routes {
		if !wanted[k] {
			r.close()
			delete(f.routes, k)
		}
	}
}
func (f *Forwarder) Close() { f.Retain(nil) }

// PublishedPorts returns the ports a Pod has published: from its status condition when there is one (a True condition
// with a JSON message), otherwise from the legacy annotation. ok is false when nothing usable is published.
func PublishedPorts(p *core.Pod) (ports map[string]int32, ok bool) {
	for _, c := range p.Status.Conditions {
		if string(c.Type) != PortsCondition {
			continue
		}
		if c.Status != core.ConditionTrue {
			return nil, false // the node says: nothing published (not ready)
		}
		ports = map[string]int32{}
		if json.Unmarshal([]byte(c.Message), &ports) != nil {
			return nil, false
		}
		return ports, true
	}
	if a := p.Annotations[PortsAnnotation]; a != "" {
		ports = map[string]int32{}
		if json.Unmarshal([]byte(a), &ports) == nil {
			return ports, true
		}
	}
	return nil, false
}
