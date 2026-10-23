package egress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

func startProxy(t *testing.T, mutate func(*Config)) (addr string) {
	t.Helper()
	_, loop, _ := net.ParseCIDR("127.0.0.0/8")
	cfg := Config{
		User: "mac", Pass: "s3cret", AllowSuffixes: []string{".cluster.local"}, AllowCIDRs: []*net.IPNet{loop},
		Lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			switch host {
			case "web.default.svc.cluster.local":
				return []net.IP{net.ParseIP("127.0.0.1")}, nil
			case "external.default.svc.cluster.local": // an ExternalName service: resolves outside the cluster
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			case "example.test":
				return []net.IP{net.ParseIP("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("no such host")
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Serve(ctx, l, cfg); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return l.Addr().String()
}

func backend(t *testing.T) (host, port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "hello from %s %s", r.Host, r.URL.Path) }))
	t.Cleanup(srv.Close)
	h, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	return h, p
}

func socksClient(t *testing.T, addr, user, pass string) proxy.Dialer {
	t.Helper()
	d, err := proxy.SOCKS5("tcp", addr, &proxy.Auth{User: user, Password: pass}, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func get(t *testing.T, c *http.Client, u string) (int, string, error) {
	resp, err := c.Get(u)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestSocks5ResolvesClusterNamesInsideTheProxy(t *testing.T) {
	_, port := backend(t)
	addr := startProxy(t, nil)
	d := socksClient(t, addr, "mac", "s3cret")
	conn, err := d.Dial("tcp", "web.default.svc.cluster.local:"+port)
	if err != nil {
		t.Fatalf("cluster name via socks: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /x HTTP/1.0\r\nHost: web\r\n\r\n")
	b, _ := io.ReadAll(conn)
	if !strings.Contains(string(b), "hello from web /x") {
		t.Fatalf("response %q", b)
	}
}

func TestHTTPProxyConnectAndPlainForwarding(t *testing.T) {
	_, port := backend(t)
	addr := startProxy(t, nil)
	pu, _ := url.Parse("http://mac:s3cret@" + addr)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
	code, body, err := get(t, c, "http://web.default.svc.cluster.local:"+port+"/plain")
	if err != nil || code != 200 || !strings.Contains(body, "hello from web.default.svc.cluster.local:"+port+" /plain") {
		t.Fatalf("plain: %d %q %v", code, body, err)
	}
	// CONNECT: an HTTPS server behind the proxy.
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure") }))
	defer tls.Close()
	_, tport, _ := net.SplitHostPort(strings.TrimPrefix(tls.URL, "https://"))
	tc := tls.Client()
	tc.Transport.(*http.Transport).Proxy = http.ProxyURL(pu)
	tc.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true // the test certificate is for example.com
	tc.Timeout = 5 * time.Second
	code, body, err = get(t, tc, "https://web.default.svc.cluster.local:"+tport+"/")
	if err != nil || code != 200 || body != "secure" {
		t.Fatalf("connect: %d %q %v", code, body, err)
	}
}

func TestAuthenticationIsRequired(t *testing.T) {
	_, port := backend(t)
	addr := startProxy(t, nil)
	if _, err := socksClient(t, addr, "mac", "wrong").Dial("tcp", "web.default.svc.cluster.local:"+port); err == nil {
		t.Fatal("socks accepted a wrong password")
	}
	if _, err := socksClient(t, addr, "other", "s3cret").Dial("tcp", "web.default.svc.cluster.local:"+port); err == nil {
		t.Fatal("socks accepted a wrong user")
	}
	pu, _ := url.Parse("http://" + addr) // no credentials
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
	if code, _, err := get(t, c, "http://web.default.svc.cluster.local:"+port+"/"); err == nil && code != 407 {
		t.Fatalf("http without credentials got %d", code)
	}
	pu, _ = url.Parse("http://mac:nope@" + addr)
	c = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
	if code, _, err := get(t, c, "http://web.default.svc.cluster.local:"+port+"/"); err == nil && code != 407 {
		t.Fatalf("http with a wrong password got %d", code)
	}
	// Garbage is not served either.
	conn, _ := net.Dial("tcp", addr)
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	b, _ := io.ReadAll(conn)
	if !strings.Contains(string(b), "407") {
		t.Fatalf("anonymous request: %q", b)
	}
}

func TestOnlyClusterDestinationsAreServedByDefault(t *testing.T) {
	_, port := backend(t)
	addr := startProxy(t, nil)
	d := socksClient(t, addr, "mac", "s3cret")
	for _, host := range []string{"example.test", "external.default.svc.cluster.local", "93.184.216.34", "other.cluster.local.evil.test"} {
		if c, err := d.Dial("tcp", host+":"+port); err == nil {
			c.Close()
			t.Fatalf("%s was served", host)
		}
	}
	// Literal addresses inside the cluster networks are fine.
	if c, err := d.Dial("tcp", "127.0.0.1:"+port); err != nil {
		t.Fatalf("an address inside the allowed CIDR: %v", err)
	} else {
		c.Close()
	}
	// HTTP: forbidden destinations are answered 403, not connected.
	pu, _ := url.Parse("http://mac:s3cret@" + addr)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
	if code, _, err := get(t, c, "http://example.test:"+port+"/"); err == nil && code != 403 {
		t.Fatalf("outside name over http got %d", code)
	}
}

func TestExternalDestinationsOnlyWhenEnabled(t *testing.T) {
	_, port := backend(t)
	addr := startProxy(t, func(c *Config) { c.AllowExternal = true })
	conn, err := socksClient(t, addr, "mac", "s3cret").Dial("tcp", "example.test:"+port)
	if err != nil {
		t.Fatalf("external name with AllowExternal: %v", err)
	}
	conn.Close()
}

func TestRequiresCredentialsToStart(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	if err := Serve(context.Background(), l, Config{}); err == nil {
		t.Fatal("a proxy without credentials started")
	}
}
