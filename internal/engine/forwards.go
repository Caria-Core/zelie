package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// A forward opens a port of the host to a port of an app's container, for
// players who are not on Zelie's networks: game servers, which cannot go
// through the web proxy. Nothing else of the container is reachable this
// way.
type Forward struct {
	// IP is the host address the port is open on. The zero address, or
	// 0.0.0.0, means all of them.
	IP     netip.Addr `json:"ip,omitzero"`
	Port   uint16     `json:"port"`
	Proto  string     `json:"proto"` // tcp or udp
	Target uint16     `json:"target"`
}

// PortForwardedError means another app already forwards the port.
type PortForwardedError struct {
	Port  uint16
	Proto string
}

func (e *PortForwardedError) Error() string {
	return fmt.Sprintf("port %d/%s is forwarded to another app", e.Port, e.Proto)
}

const maxForwards = 64

// Forwards below this number would let an app take over SSH, DNS and the
// like from the host, so they are refused whoever asks.
const minForwardPort = 1024

func (f Forward) everyAddress() bool { return !f.IP.IsValid() || f.IP.IsUnspecified() }

// clashes reports whether the two open the same host port.
func (f Forward) clashes(g Forward) bool {
	return f.Port == g.Port && f.Proto == g.Proto && (f.everyAddress() || g.everyAddress() || f.IP == g.IP)
}

// CheckForwards reports whether forwards may be set on app.
func CheckForwards(app string, forwards []Forward) error {
	if !validID.MatchString(app) {
		return fmt.Errorf("app id %q must be lowercase letters, digits and dashes", app)
	}
	if len(forwards) > maxForwards {
		return fmt.Errorf("an app can have at most %d forwards", maxForwards)
	}
	for i, f := range forwards {
		switch {
		case f.Proto != "tcp" && f.Proto != "udp":
			return fmt.Errorf("forward protocol %q must be tcp or udp", f.Proto)
		case f.Port < minForwardPort:
			return fmt.Errorf("host port %d is below %d", f.Port, minForwardPort)
		case f.Target == 0:
			return errors.New("a forward needs a container port")
		case f.IP.IsValid() && !f.IP.Is4():
			return fmt.Errorf("forward address %s must be IPv4", f.IP)
		}
		for _, g := range forwards[:i] {
			if f.clashes(g) {
				return fmt.Errorf("port %d/%s is forwarded twice", f.Port, f.Proto)
			}
		}
	}
	return nil
}

func (e *Engine) forwardsFile() string { return filepath.Join(e.paths.Data, "forwards.json") }

func (e *Engine) loadForwards() (map[string][]Forward, error) {
	b, err := os.ReadFile(e.forwardsFile())
	if errors.Is(err, os.ErrNotExist) {
		return map[string][]Forward{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string][]Forward{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("read %s: %w", e.forwardsFile(), err)
	}
	return m, nil
}

// SetForwards replaces the forwards of an app; none clears them. They are
// saved, so they come back when the core restarts, and follow the app's
// container to its new address whenever it has one.
func (e *Engine) SetForwards(ctx context.Context, app string, forwards []Forward) error {
	if err := CheckForwards(app, forwards); err != nil {
		return err
	}
	e.peers.mu.Lock()
	defer e.peers.mu.Unlock()
	m, err := e.loadForwards()
	if err != nil {
		return err
	}
	for other, list := range m {
		if other == app {
			continue
		}
		for _, f := range forwards {
			if slices.ContainsFunc(list, f.clashes) {
				return &PortForwardedError{Port: f.Port, Proto: f.Proto}
			}
		}
	}
	if len(forwards) == 0 {
		delete(m, app)
	} else {
		m[app] = slices.Clone(forwards)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.paths.Data, 0o700); err != nil {
		return err
	}
	if _, err := writeIfChanged(e.forwardsFile(), b, 0o600); err != nil {
		return err
	}
	return e.refreshLocked(ctx)
}

// portMap is a Forward with the address its app's container has now.
type portMap struct {
	Forward
	to netip.Addr
}

// forwardRule is the iptables rule that lets one forward's traffic through
// FORWARD, for firewalls like ufw that drop what nothing allows. It only
// matches connections the DNAT rules changed, coming from outside Zelie's
// networks, and only to that container's port.
func forwardRule(f portMap) []string {
	return []string{
		"!", "-i", "zelie+", "-o", "zelie+",
		"-d", f.to.String() + "/32", "-p", f.Proto, "--dport", strconv.Itoa(int(f.Target)),
		"-m", "conntrack", "--ctstate", "DNAT", "-j", "ACCEPT",
	}
}
