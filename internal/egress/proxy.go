// Package egress is the cluster side of "Mac workloads reaching the cluster network": a small forward proxy that runs
// inside the cluster (where cluster DNS and ClusterIPs work) and that Mac workloads use through HTTP_PROXY /
// ALL_PROXY. One port speaks both SOCKS5 (CONNECT, user/password) and HTTP (CONNECT and plain forwarding, Basic
// proxy authentication). Names are resolved here, in the cluster, so `socks5h`/HTTP proxies resolve
// `*.svc.cluster.local` correctly even though the Mac cannot.
//
// The proxy is reachable from the LAN (it has to be, the Mac is not on the pod network), so it is not an open relay:
// every connection must authenticate, and by default only destinations inside the cluster (names under the
// allowed suffixes, addresses inside the allowed CIDRs) are served.
package egress

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	User, Pass    string
	AllowSuffixes []string     // DNS names must end with one of these (lower case, leading dot)
	AllowCIDRs    []*net.IPNet // every address a name resolves to, and every literal IP, must be inside one
	AllowExternal bool         // also serve destinations outside the cluster (dialed directly from here)
	MaxConns      int
	// Test seams.
	Lookup func(ctx context.Context, host string) ([]net.IP, error)
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	Logf   func(format string, args ...any)
}

const (
	handshakeTimeout = 15 * time.Second
	dialTimeout      = 10 * time.Second
)

func (c *Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Serve accepts connections until ctx ends.
func Serve(ctx context.Context, l net.Listener, cfg Config) error {
	if cfg.User == "" || cfg.Pass == "" {
		return errors.New("egress proxy: a user and password are required")
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 256
	}
	if cfg.Lookup == nil {
		cfg.Lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, err
		}
	}
	if cfg.Dial == nil {
		d := &net.Dialer{Timeout: dialTimeout}
		cfg.Dial = d.DialContext
	}
	go func() { <-ctx.Done(); l.Close() }()
	sem := make(chan struct{}, cfg.MaxConns)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			conn.Close() // over the connection limit
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer conn.Close()
			handle(ctx, conn, &cfg)
		}()
	}
}

func handle(ctx context.Context, conn net.Conn, cfg *Config) {
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 0x05 {
		handleSocks(ctx, conn, br, cfg)
		return
	}
	handleHTTP(ctx, conn, br, cfg)
}

// authorize decides where a destination may go. It returns the addresses to dial.
func (c *Config) authorize(ctx context.Context, host string, port int) ([]string, error) {
	if port < 1 || port > 65535 {
		return nil, errors.New("bad port")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return nil, errors.New("empty host")
	}
	inCluster := func(ip net.IP) bool {
		for _, n := range c.AllowCIDRs {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		if !c.AllowExternal && !inCluster(ip) {
			return nil, fmt.Errorf("destination %s is outside the cluster networks", ip)
		}
		return []string{net.JoinHostPort(ip.String(), strconv.Itoa(port))}, nil
	}
	named := false
	for _, s := range c.AllowSuffixes {
		if strings.HasSuffix(host, s) {
			named = true
			break
		}
	}
	if !named && !c.AllowExternal {
		return nil, fmt.Errorf("name %q is not under an allowed cluster suffix", host)
	}
	rctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	ips, err := c.Lookup(rctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve %q", host)
	}
	var out []string
	for _, ip := range ips {
		// A cluster name must resolve into the cluster (an ExternalName service would not).
		if named && !c.AllowExternal && !inCluster(ip) {
			continue
		}
		out = append(out, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%q resolves outside the cluster networks", host)
	}
	return out, nil
}

func (c *Config) dialAny(ctx context.Context, addrs []string) (net.Conn, error) {
	var last error
	for _, a := range addrs {
		conn, err := c.Dial(ctx, "tcp", a)
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

func pipe(a, b net.Conn) {
	a.SetDeadline(time.Time{})
	b.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

func credsOK(c *Config, user, pass string) bool {
	u := subtle.ConstantTimeCompare([]byte(user), []byte(c.User))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(c.Pass))
	return u&p == 1
}

// ---- SOCKS5 (RFC 1928, user/password per RFC 1929, CONNECT only)

func handleSocks(ctx context.Context, conn net.Conn, br *bufio.Reader, cfg *Config) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if !strings.Contains(string(methods), "\x02") {
		conn.Write([]byte{5, 0xff})
		return
	}
	conn.Write([]byte{5, 2})
	// RFC 1929 sub-negotiation.
	var v [2]byte
	if _, err := io.ReadFull(br, v[:]); err != nil || v[0] != 1 {
		return
	}
	user := make([]byte, v[1])
	if _, err := io.ReadFull(br, user); err != nil {
		return
	}
	pl, err := br.ReadByte()
	if err != nil {
		return
	}
	pass := make([]byte, pl)
	if _, err := io.ReadFull(br, pass); err != nil {
		return
	}
	if !credsOK(cfg, string(user), string(pass)) {
		conn.Write([]byte{1, 1})
		cfg.logf("socks: authentication failed from %s", conn.RemoteAddr())
		return
	}
	conn.Write([]byte{1, 0})
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil || req[0] != 5 {
		return
	}
	reply := func(code byte) { conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}) }
	if req[1] != 1 {
		reply(7) // command not supported
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3:
		l, err := br.ReadByte()
		if err != nil {
			return
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = string(b)
	default:
		reply(8)
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(br, pb[:]); err != nil {
		return
	}
	port := int(pb[0])<<8 | int(pb[1])
	addrs, err := cfg.authorize(ctx, host, port)
	if err != nil {
		cfg.logf("socks: denied %s:%d: %v", host, port, err)
		reply(2) // not allowed by ruleset
		return
	}
	up, err := cfg.dialAny(ctx, addrs)
	if err != nil {
		reply(5) // connection refused
		return
	}
	defer up.Close()
	reply(0)
	if br.Buffered() > 0 { // data the client sent right behind the request
		b, _ := br.Peek(br.Buffered())
		up.Write(b)
	}
	pipe(conn, up)
}

// ---- HTTP proxy

func handleHTTP(ctx context.Context, conn net.Conn, br *bufio.Reader, cfg *Config) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	deny := func(code int, msg string) {
		fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n", code, http.StatusText(code), len(msg))
		if code == http.StatusProxyAuthRequired {
			io.WriteString(conn, "Proxy-Authenticate: Basic realm=\"maccluster-egress\"\r\n")
		}
		io.WriteString(conn, "\r\n"+msg)
	}
	user, pass, ok := parseProxyAuth(req.Header.Get("Proxy-Authorization"))
	if !ok || !credsOK(cfg, user, pass) {
		cfg.logf("http: authentication failed from %s", conn.RemoteAddr())
		deny(http.StatusProxyAuthRequired, "proxy authentication required\n")
		return
	}
	target := req.Host
	if req.Method != http.MethodConnect && req.URL != nil && req.URL.Host != "" {
		target = req.URL.Host
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		host = target
		portStr = "80"
		if req.Method == http.MethodConnect {
			portStr = "443"
		}
	}
	port, _ := strconv.Atoi(portStr)
	addrs, err := cfg.authorize(ctx, host, port)
	if err != nil {
		cfg.logf("http: denied %s:%d: %v", host, port, err)
		deny(http.StatusForbidden, err.Error()+"\n")
		return
	}
	up, err := cfg.dialAny(ctx, addrs)
	if err != nil {
		deny(http.StatusBadGateway, "cannot connect\n")
		return
	}
	defer up.Close()
	if req.Method == http.MethodConnect {
		io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if br.Buffered() > 0 {
			b, _ := br.Peek(br.Buffered())
			up.Write(b)
		}
		pipe(conn, up)
		return
	}
	// Plain HTTP: forward this one request in origin form, then relay the rest of the exchange.
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	req.Header.Set("Connection", "close")
	req.RequestURI = ""
	origin := *req.URL
	origin.Scheme, origin.Host = "", ""
	req.URL = &origin
	if err := req.Write(up); err != nil {
		return
	}
	conn.SetDeadline(time.Time{})
	io.Copy(conn, up)
}

func parseProxyAuth(h string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	u, p, found := strings.Cut(string(raw), ":")
	return u, p, found
}
