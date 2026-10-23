package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"maccluster/internal/egress"
)

// egressProxy runs the cluster-side egress proxy (docs/egress-design.md). It is meant to run as a Pod inside the
// cluster; credentials come from the environment so they never appear on a command line.
func egressProxy(args []string) error {
	f := flag.NewFlagSet("egress-proxy", flag.ContinueOnError)
	listen := f.String("listen", ":1080", "address to listen on (HTTP proxy and SOCKS5 on the same port)")
	external := f.Bool("allow-external", false, "also serve destinations outside the cluster (dialed directly from this Pod)")
	var suffixes, cidrs multiFlag
	f.Var(&suffixes, "allow-suffix", "DNS suffix of names that are served (repeatable; default .cluster.local)")
	f.Var(&cidrs, "allow-cidr", "network that destinations must be in (repeatable; default 10.96.0.0/12 and 10.244.0.0/16)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if len(suffixes) == 0 {
		suffixes = multiFlag{".cluster.local"}
	}
	if len(cidrs) == 0 {
		cidrs = multiFlag{"10.96.0.0/12", "10.244.0.0/16"}
	}
	cfg := egress.Config{User: os.Getenv("EGRESS_USER"), Pass: os.Getenv("EGRESS_PASSWORD"), AllowExternal: *external,
		Logf: func(format string, a ...any) { fmt.Fprintf(os.Stderr, "egress: "+format+"\n", a...) }}
	if cfg.User == "" || cfg.Pass == "" {
		return fmt.Errorf("set EGRESS_USER and EGRESS_PASSWORD")
	}
	for _, s := range suffixes {
		s = strings.ToLower(s)
		if !strings.HasPrefix(s, ".") {
			s = "." + s
		}
		cfg.AllowSuffixes = append(cfg.AllowSuffixes, s)
	}
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("--allow-cidr %q: %w", c, err)
		}
		cfg.AllowCIDRs = append(cfg.AllowCIDRs, n)
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Fprintf(os.Stderr, "egress proxy listening on %s (suffixes %v, networks %v, external=%v)\n", l.Addr(), suffixes, cidrs, *external)
	return egress.Serve(ctx, l, cfg)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
