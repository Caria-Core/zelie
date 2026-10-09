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

// Containers cannot open connections to the host. Host access makes one
// exception for an app: its containers may reach TCP port 3306 of the host,
// where a MariaDB installed on the server itself listens. The port is not
// configurable, and nothing else of the host opens.
const (
	HostName = "host.zelie.internal"
	HostPort = 3306
)

// hostRule is one opening to the host: the bridge of an app with host access
// on, and the host's address on it. Only that address is let through, so the
// host's other addresses stay closed.
type hostRule struct {
	bridge string
	gw     netip.Addr
}

// iptablesRule is the rule for firewalls like ufw that drop what nothing
// allows (see inputChain).
func (r hostRule) iptablesRule() []string {
	return []string{"-i", r.bridge, "-d", r.gw.String() + "/32", "-p", "tcp", "--dport", strconv.Itoa(HostPort), "-j", "ACCEPT"}
}

func (e *Engine) hostAccessFile() string { return filepath.Join(e.paths.Data, "host-access.json") }

// loadHostAccess reads the apps that have host access, as a list of names.
func (e *Engine) loadHostAccess() ([]string, error) {
	b, err := os.ReadFile(e.hostAccessFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var apps []string
	if err := json.Unmarshal(b, &apps); err != nil {
		return nil, fmt.Errorf("read %s: %w", e.hostAccessFile(), err)
	}
	return apps, nil
}

// HostAccess reports whether the app may reach the host's port 3306.
func (e *Engine) HostAccess(app string) (bool, error) {
	e.peers.mu.Lock()
	defer e.peers.mu.Unlock()
	apps, err := e.loadHostAccess()
	return slices.Contains(apps, app), err
}

// SetHostAccess turns the app's access to the host on or off. It applies at
// once, to containers already running too, and is saved, so it comes back
// when the core restarts.
func (e *Engine) SetHostAccess(ctx context.Context, app string, on bool) error {
	if !validID().MatchString(app) {
		return fmt.Errorf("app id %q must be lowercase letters, digits and dashes", app)
	}
	e.peers.mu.Lock()
	defer e.peers.mu.Unlock()
	if err := e.saveHostAccess(app, on); err != nil {
		return err
	}
	return e.refreshLocked(ctx)
}

func (e *Engine) saveHostAccess(app string, on bool) error {
	apps, err := e.loadHostAccess()
	if err != nil {
		return err
	}
	apps = slices.DeleteFunc(apps, func(a string) bool { return a == app })
	if on {
		apps = append(apps, app)
	}
	slices.Sort(apps)
	b, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.paths.Data, 0o700); err != nil {
		return err
	}
	_, err = writeIfChanged(e.hostAccessFile(), b, 0o600)
	return err
}

// hostAccessRules derives the openings from the networks as they are now: a
// network is named after its app, and its index and address can change when
// the app has no container left, so nothing here is remembered. The second
// result is the host's address on the network of each app, for the DNS
// server.
func hostAccessRules(nets []network, apps []string) ([]hostRule, map[string]netip.Addr) {
	var rules []hostRule
	addrs := map[string]netip.Addr{}
	for _, nw := range nets {
		if slices.Contains(apps, nw.Name) {
			rules = append(rules, hostRule{nw.bridge(), nw.gateway()})
			addrs[nw.Name] = nw.gateway()
		}
	}
	return rules, addrs
}
