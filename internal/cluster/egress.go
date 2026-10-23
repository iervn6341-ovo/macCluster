package cluster

import (
	"fmt"
	"net/url"
	"strings"
)

// EgressAnnotation opts a Pod in to the cluster egress proxy: a Mac workload cannot reach ClusterIPs or cluster DNS,
// so the agent hands it proxy environment variables for the proxy that runs inside the cluster
// (docs/egress-design.md). Off unless asked, because every HTTP(S) request of that Pod then goes through the proxy.
const EgressAnnotation = "maccluster.example.com/cluster-egress"

// egressEnv builds the proxy variables for proxyURL ("http://user:password@host:port"; the same port serves HTTP and
// SOCKS5). Variables the Pod sets itself are left to the caller to keep.
func egressEnv(proxyURL, hostIP string) (map[string]string, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return nil, fmt.Errorf("egressProxy must look like http://USER:PASSWORD@HOST:PORT")
	}
	if u.User == nil || u.Port() == "" {
		return nil, fmt.Errorf("egressProxy needs credentials and a port: http://USER:PASSWORD@HOST:PORT")
	}
	httpURL := (&url.URL{Scheme: "http", User: u.User, Host: u.Host}).String()
	socksURL := (&url.URL{Scheme: "socks5h", User: u.User, Host: u.Host}).String()
	no := []string{"localhost", "127.0.0.1", "::1", "host.docker.internal"}
	if hostIP != "" {
		no = append(no, hostIP)
	}
	noProxy := strings.Join(no, ",")
	return map[string]string{
		"HTTP_PROXY": httpURL, "HTTPS_PROXY": httpURL, "ALL_PROXY": socksURL,
		"http_proxy": httpURL, "https_proxy": httpURL, "all_proxy": socksURL,
		"NO_PROXY": noProxy, "no_proxy": noProxy,
	}, nil
}

// applyEgress merges the proxy variables into the environment that only the program sees (never persisted, not in
// the spec hash, passed to Docker by name so the credentials are not on its command line).
func applyEgress(literal, resolved map[string]string, vars map[string]string) map[string]string {
	if resolved == nil {
		resolved = map[string]string{}
	}
	for k, v := range vars {
		if _, set := literal[k]; set {
			continue
		}
		if _, set := resolved[k]; set {
			continue
		}
		resolved[k] = v
	}
	return resolved
}
