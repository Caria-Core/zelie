package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strconv"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/proxy"
)

var errUsage = errors.New("usage")

func debugProxy(ctx context.Context, c *core.Client, args []string, stdout io.Writer) error {
	pc := proxy.NewClient(proxySocket)
	cfg, err := pc.Config(ctx)
	if err != nil {
		return err
	}
	switch {
	case len(args) == 2 && args[0] == "route" && args[1] == "ls":
		fmt.Fprintf(stdout, "tls: %s %s\n", cfg.TLS, cfg.Email)
		if cfg.Panel != "" {
			fmt.Fprintf(stdout, "%s -> panel\n", cfg.Panel)
		}
		for _, r := range cfg.Routes {
			fmt.Fprintf(stdout, "%s -> %s\n", r.Host, r.Upstream)
		}
		return nil
	case len(args) == 5 && args[0] == "route" && args[1] == "add":
		port, err := strconv.ParseUint(args[4], 10, 16)
		if err != nil {
			return fmt.Errorf("bad port %q", args[4])
		}
		list, err := c.List(ctx)
		if err != nil {
			return err
		}
		var ip netip.Addr
		for _, s := range list {
			if s.ID == args[3] {
				ip = s.IP
			}
		}
		if !ip.IsValid() {
			return fmt.Errorf("container %s has no network address", args[3])
		}
		cfg.Routes = slices.DeleteFunc(cfg.Routes, func(r proxy.Route) bool { return r.Host == args[2] })
		cfg.Routes = append(cfg.Routes, proxy.Route{Host: args[2], Upstream: netip.AddrPortFrom(ip, uint16(port)).String()})
	case len(args) == 3 && args[0] == "route" && args[1] == "rm":
		cfg.Routes = slices.DeleteFunc(cfg.Routes, func(r proxy.Route) bool { return r.Host == args[2] })
	case len(args) == 3 && args[0] == "tls" && args[1] == proxy.TLSACME:
		cfg.TLS, cfg.Email = proxy.TLSACME, args[2]
	case len(args) == 2 && args[0] == "panel":
		cfg.Panel = args[1]
	case len(args) == 2 && args[0] == "tls" && args[1] == proxy.TLSSelfSigned:
		cfg.TLS = proxy.TLSSelfSigned
	default:
		return errUsage
	}
	return pc.Apply(ctx, cfg)
}
