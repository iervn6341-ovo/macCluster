package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// The checks run inside the agent process because that is the process which has to reach the control plane: on
// macOS the "Local Network" privacy setting is granted per program, so what the app (or a shell) can reach says
// nothing about what the background agent can.

const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

type netCheck struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // ok, warn, fail or skip
	Detail string `json:"detail,omitempty"`
	// Hint is what to do about it. Action names a button the app can show ("open-local-network-settings").
	Hint   string `json:"hint,omitempty"`
	Action string `json:"action,omitempty"`
}

type netCheckResult struct {
	CheckedAt string     `json:"checkedAt"`
	Checks    []netCheck `json:"checks"`
}

// netTarget is one joined cluster to check.
type netTarget struct {
	Name       string
	Endpoint   string // https://host:port
	Kubeconfig string // the node's kubeconfig, "" if there is none
}

// netProbes are the operations the checks use, so tests can replace them.
type netProbes struct {
	Dial func(ctx context.Context, address string) error
	Ping func(ctx context.Context, host string) bool
	Now  func() time.Time
}

func systemProbes() netProbes {
	return netProbes{
		Dial: func(ctx context.Context, address string) error {
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
			if err == nil {
				c.Close()
			}
			return err
		},
		// ping is an Apple-signed tool, which macOS does not hold to the Local Network setting: if it gets
		// through where this process does not, the setting is what stands in the way.
		Ping: func(ctx context.Context, host string) bool {
			return exec.CommandContext(ctx, "/sbin/ping", "-c", "1", "-t", "2", host).Run() == nil
		},
		Now: time.Now,
	}
}

func hostPort(endpoint string) (string, string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("invalid endpoint %q", endpoint)
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	return host, port, nil
}

// checkReachable dials the control plane the way the node does, and says what a failure most likely means.
func checkReachable(ctx context.Context, p netProbes, t netTarget) netCheck {
	c := netCheck{ID: "control-plane:" + t.Name, Title: "連到控制平面 " + t.Name}
	host, port, err := hostPort(t.Endpoint)
	if err != nil {
		c.Status, c.Detail = checkFail, err.Error()
		return c
	}
	address := net.JoinHostPort(host, port)
	dialCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	err = p.Dial(dialCtx, address)
	switch {
	case err == nil:
		c.Status, c.Detail = checkOK, "agent 可以直接連到 "+address
	case errors.Is(err, syscall.EHOSTUNREACH):
		c.Status = checkFail
		c.Detail = fmt.Sprintf("agent 連 %s 失敗：No route to host", address)
		pingCtx, pingCancel := context.WithTimeout(ctx, 4*time.Second)
		defer pingCancel()
		if p.Ping(pingCtx, host) {
			c.Detail += "；但 ping 可以通，網路本身是通的"
			c.Hint = "很可能是 macOS 的「區域網路」權限沒有授予 agent。請到「系統設定 → 隱私權與安全性 → 區域網路」開啟 MacCluster；若清單裡沒有它，請回報。"
			c.Action = "open-local-network-settings"
		} else {
			c.Hint = "這台 Mac 到不了該位址（ping 也不通）。請檢查網路連線、路由或防火牆。"
		}
	case errors.Is(err, syscall.ECONNREFUSED):
		c.Status, c.Detail = checkFail, fmt.Sprintf("%s 拒絕連線", address)
		c.Hint = "位址可達，但沒有服務在該埠監聽。請確認 control plane 與埠號。"
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		c.Status, c.Detail = checkFail, fmt.Sprintf("連 %s 逾時", address)
		c.Hint = "封包沒有回應，可能被防火牆丟棄或位址錯誤。"
	default:
		c.Status, c.Detail = checkFail, err.Error()
	}
	return c
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// checkProxy: a node kubeconfig can send its traffic through a proxy (proxy-url); that proxy has to be running.
func checkProxy(ctx context.Context, p netProbes, t netTarget) netCheck {
	c := netCheck{ID: "proxy:" + t.Name, Title: "節點連線代理 " + t.Name}
	if t.Kubeconfig == "" {
		c.Status, c.Detail = checkSkip, "沒有節點憑證檔"
		return c
	}
	cfg, err := clientcmd.LoadFromFile(t.Kubeconfig)
	if err != nil {
		c.Status, c.Detail = checkFail, "無法讀取節點憑證檔："+err.Error()
		return c
	}
	proxy := ""
	if ctxName := cfg.CurrentContext; ctxName != "" {
		if cc := cfg.Contexts[ctxName]; cc != nil && cfg.Clusters[cc.Cluster] != nil {
			proxy = cfg.Clusters[cc.Cluster].ProxyURL
		}
	}
	if proxy == "" {
		c.Status, c.Detail = checkOK, "沒有設定代理，直接連線"
		return c
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		c.Status, c.Detail = checkFail, "代理設定無法解析"
		return c
	}
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := p.Dial(dialCtx, u.Host); err != nil {
		c.Status = checkFail
		c.Detail = fmt.Sprintf("節點設定要經由代理 %s://%s，但連不上它（%v）", u.Scheme, u.Host, err)
		c.Hint = "請啟動該代理（例如 SSH 隧道），或從節點憑證檔移除 proxy-url 改為直連（先確認直連可達）。"
		return c
	}
	c.Status, c.Detail = checkOK, fmt.Sprintf("經由代理 %s://%s，代理可連", u.Scheme, u.Host)
	return c
}

// checkCertificate: the node's client certificate renews itself, but a long-expired one means the node cannot sign in.
func checkCertificate(p netProbes, t netTarget) netCheck {
	c := netCheck{ID: "certificate:" + t.Name, Title: "節點憑證 " + t.Name}
	if t.Kubeconfig == "" {
		c.Status, c.Detail = checkSkip, "沒有節點憑證檔"
		return c
	}
	cfg, err := clientcmd.LoadFromFile(t.Kubeconfig)
	if err != nil {
		c.Status, c.Detail = checkFail, "無法讀取節點憑證檔："+err.Error()
		return c
	}
	var data []byte
	for _, a := range cfg.AuthInfos {
		data = a.ClientCertificateData
		if len(data) == 0 && a.ClientCertificate != "" {
			data, _ = os.ReadFile(a.ClientCertificate)
		}
		break
	}
	block, _ := pem.Decode(data)
	if block == nil {
		c.Status, c.Detail = checkSkip, "憑證檔沒有內嵌的用戶端憑證"
		return c
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		c.Status, c.Detail = checkFail, "憑證無法解析"
		return c
	}
	left := cert.NotAfter.Sub(p.Now())
	switch {
	case left <= 0:
		c.Status, c.Detail = checkFail, "節點憑證已於 "+cert.NotAfter.Local().Format("2006-01-02 15:04")+" 過期"
		c.Hint = "節點無法再用這張憑證登入。需要重新加入叢集，或等 control plane 恢復後讓 agent 自動更新。"
	case left < 7*24*time.Hour:
		c.Status, c.Detail = checkWarn, "節點憑證將於 "+cert.NotAfter.Local().Format("2006-01-02 15:04")+" 到期"
		c.Hint = "agent 會在到期前自動更新；若 control plane 一直連不上，請先排除連線問題。"
	default:
		c.Status, c.Detail = checkOK, "節點憑證有效至 "+cert.NotAfter.Local().Format("2006-01-02")
	}
	return c
}

// runNetChecks runs every check for every target. With no cluster joined there is nothing to reach.
func runNetChecks(ctx context.Context, p netProbes, targets []netTarget) netCheckResult {
	result := netCheckResult{CheckedAt: p.Now().UTC().Format(time.RFC3339), Checks: []netCheck{}}
	if len(targets) == 0 {
		result.Checks = append(result.Checks, netCheck{ID: "control-plane", Title: "連到控制平面", Status: checkSkip, Detail: "尚未加入任何叢集"})
		return result
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	for _, t := range targets {
		result.Checks = append(result.Checks, checkReachable(ctx, p, t), checkProxy(ctx, p, t), checkCertificate(p, t))
	}
	return result
}

// netcheck answers GET /v1/netcheck.
func (s *server) netcheck(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var targets []netTarget
	for _, c := range s.sessions {
		if c.profile.Leaving || c.profile.RemovedAt != "" {
			continue
		}
		kc := filepath.Join(s.profileDir(c.profile.ID), "node.kubeconfig")
		if _, err := os.Stat(kc); err != nil {
			kc = ""
		}
		name := c.profile.Name
		if strings.TrimSpace(name) == "" {
			name = c.profile.Endpoint
		}
		targets = append(targets, netTarget{Name: name, Endpoint: c.profile.Endpoint, Kubeconfig: kc})
	}
	s.mu.Unlock()
	jsonReply(w, runNetChecks(r.Context(), systemProbes(), targets))
}
