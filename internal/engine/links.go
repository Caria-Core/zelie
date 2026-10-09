package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Apps cannot reach each other: each has its own network, and the host
// firewall drops traffic between networks. A link opens one way through:
// the app's containers may connect to one TCP port of another app's
// containers, and find them by the link's name through the DNS server.
type Link struct {
	Name string `json:"name"` // what the app looks up, such as "db"
	To   string `json:"to"`   // the app it reaches
	Port uint16 `json:"port"`
}

const maxLinks = 32

// CheckLinks reports whether links may be set on app.
func CheckLinks(app string, links []Link) error {
	if !validID().MatchString(app) {
		return fmt.Errorf("app id %q must be lowercase letters, digits and dashes", app)
	}
	if len(links) > maxLinks {
		return fmt.Errorf("an app can have at most %d links", maxLinks)
	}
	names := map[string]bool{}
	for _, l := range links {
		switch {
		case !validID().MatchString(l.Name):
			return fmt.Errorf("link name %q must be lowercase letters, digits and dashes", l.Name)
		case l.Name == "localhost" || l.Name == app:
			return fmt.Errorf("link name %q is taken", l.Name)
		case !validID().MatchString(l.To):
			return fmt.Errorf("app id %q must be lowercase letters, digits and dashes", l.To)
		case l.To == app:
			return errors.New("an app cannot link to itself")
		case l.Port == 0:
			return errors.New("a link needs a port")
		case names[l.Name]:
			return fmt.Errorf("two links are named %s", l.Name)
		}
		names[l.Name] = true
	}
	return nil
}

func (e *Engine) linksFile() string { return filepath.Join(e.paths.Data, "links.json") }

func (e *Engine) loadLinks() (map[string][]Link, error) {
	b, err := os.ReadFile(e.linksFile())
	if errors.Is(err, os.ErrNotExist) {
		return map[string][]Link{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string][]Link{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("read %s: %w", e.linksFile(), err)
	}
	return m, nil
}

// Links returns the links of an app.
func (e *Engine) Links(app string) ([]Link, error) {
	e.peers.mu.Lock()
	defer e.peers.mu.Unlock()
	m, err := e.loadLinks()
	if err != nil {
		return nil, err
	}
	return m[app], nil
}

// SetLinks replaces the links of an app. They apply at once, to containers
// already running too.
func (e *Engine) SetLinks(ctx context.Context, app string, links []Link) error {
	if err := CheckLinks(app, links); err != nil {
		return err
	}
	e.peers.mu.Lock()
	defer e.peers.mu.Unlock()
	m, err := e.loadLinks()
	if err != nil {
		return err
	}
	if len(links) == 0 {
		delete(m, app)
	} else {
		m[app] = slices.Clone(links)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.paths.Data, 0o700); err != nil {
		return err
	}
	if _, err := writeIfChanged(e.linksFile(), b, 0o600); err != nil {
		return err
	}
	return e.refreshLocked(ctx)
}

// peers is what the firewall and the DNS server know about containers:
// which app each address belongs to.
type peers struct {
	mu    sync.Mutex
	at    time.Time
	links map[string][]Link
	// app of each container address, and the addresses of each app, the
	// running ones first.
	appOf map[netip.Addr]string
	// the host's address on the network of each app that has host access
	hostAddr map[string]netip.Addr
	running  map[string][]netip.Addr
	others   map[string][]netip.Addr
	// what the firewall was last given, to skip rewriting it unchanged
	applied *firewall
	// when the host's INPUT chain was last checked
	inputAt time.Time
}

// rulesIntact tells whether Zelie's nftables rules are still in the kernel.
// Tests replace it.
var rulesIntact = firewallIntact

// hostInputEvery is how often the host's INPUT chain is checked again: a
// firewall reloading its own rules can take Zelie's jump away.
const hostInputEvery = time.Minute

// refresh reads the containers again and brings the firewall up to date.
func (e *Engine) refresh(ctx context.Context) error {
	e.peers.mu.Lock()
	defer e.peers.mu.Unlock()
	return e.refreshLocked(ctx)
}

func (e *Engine) refreshLocked(ctx context.Context) error {
	list, err := e.List(ctx)
	if err != nil {
		return err
	}
	links, err := e.loadLinks()
	if err != nil {
		return err
	}
	forwards, err := e.loadForwards()
	if err != nil {
		return err
	}
	hostApps, err := e.loadHostAccess()
	if err != nil {
		return err
	}
	nets, err := e.networks.all()
	if err != nil {
		return err
	}
	p := &e.peers
	p.links = links
	p.appOf = map[netip.Addr]string{}
	p.running = map[string][]netip.Addr{}
	p.others = map[string][]netip.Addr{}
	for _, c := range list {
		if !c.IP.IsValid() || c.App == "" {
			continue
		}
		p.appOf[c.IP] = c.App
		if c.State == "running" {
			p.running[c.App] = append(p.running[c.App], c.IP)
		} else {
			p.others[c.App] = append(p.others[c.App], c.IP)
		}
	}
	p.at = time.Now()

	fw := &firewall{}
	for _, nw := range nets {
		fw.bridges = append(fw.bridges, nw.bridge())
	}
	fw.host, p.hostAddr = hostAccessRules(nets, hostApps)
	for app, ls := range links {
		for _, l := range ls {
			for _, from := range p.addrs(app) {
				for _, to := range p.addrs(l.To) {
					fw.allow = append(fw.allow, allowed{from, to, l.Port})
				}
			}
		}
	}
	// A forward goes to a running container, or to one about to start:
	// the firewall is refreshed just before a task starts, not after. Its
	// address is the one that changes when the app restarts, and this is
	// where it is followed.
	starting := map[string]netip.Addr{}
	for _, c := range list {
		if c.IP.IsValid() && c.State == "created" {
			starting[c.App] = c.IP
		}
	}
	for app, fs := range forwards {
		to, ok := starting[app]
		if r := p.running[app]; len(r) > 0 {
			to, ok = r[0], true
		}
		if !ok {
			continue
		}
		for _, f := range fs {
			fw.forwards = append(fw.forwards, portMap{f, to})
		}
	}
	fw.sort()
	if err := e.checkHostRules(ctx); err != nil {
		return err
	}
	if p.applied != nil && p.applied.equal(fw) {
		return nil
	}
	old := p.applied
	if err := applyFirewall(fw); err != nil {
		return err
	}
	if err := ensureHostForward(ctx, fw.forwards, true); err != nil {
		return err
	}
	// Emptying the chain also drops the rules for DNS for a moment, so it is
	// only done when the openings to the host changed.
	if old == nil || !slices.Equal(old.host, fw.host) {
		if err := ensureHostInput(ctx, fw.host, true); err != nil {
			return err
		}
	}
	p.applied = fw
	e.endStaleFlows(old, fw)
	return nil
}

// checkHostRules runs once a minute and makes sure the rules other firewalls
// can undo are still there. Missing iptables jumps are put back. When the
// nftables tables are gone, the applied rules are forgotten so that the caller
// applies everything again; otherwise they would stay missing until the
// containers happen to change. The caller holds peers.mu.
func (e *Engine) checkHostRules(ctx context.Context) error {
	p := &e.peers
	if time.Since(p.inputAt) < hostInputEvery {
		return nil
	}
	var host []hostRule
	if p.applied != nil {
		host = p.applied.host
	}
	if err := ensureHostInput(ctx, host, false); err != nil {
		return err
	}
	if p.applied != nil {
		intact, err := rulesIntact()
		if err != nil {
			return err
		}
		if !intact {
			e.logger().Warn("the firewall rules for containers were removed, putting them back")
			p.applied = nil
		}
	}
	if p.applied != nil {
		if err := ensureHostForward(ctx, p.applied.forwards, false); err != nil {
			return err
		}
	}
	p.inputAt = time.Now()
	return nil
}

// keepHostRules checks the host's rules without a container having changed or
// asked for a name, and puts back what is missing. The caller decides when;
// refreshing the containers is the price of applying the rules again.
func (e *Engine) keepHostRules(ctx context.Context) {
	p := &e.peers
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.inputAt) < hostInputEvery {
		return
	}
	if err := e.refreshLocked(ctx); err != nil {
		e.logger().Error("check the firewall rules for containers", "err", err)
	}
}

// endStaleFlows ends the tracked UDP flows that still lead to a container a
// forward no longer points at (see dropStaleFlows). It runs after the new
// rules are in, so no flow can start on the old ones. A failure is logged and
// does not undo the change that caused it.
func (e *Engine) endStaleFlows(old, now *firewall) {
	var was []portMap
	if old != nil {
		was = old.forwards
	}
	if old != nil && slices.Equal(was, now.forwards) {
		return
	}
	if !hasUDP(was) && !hasUDP(now.forwards) {
		return
	}
	if err := dropStaleForwards(was, now.forwards); err != nil {
		e.logger().Error("end UDP flows to forwards that moved", "err", err)
	}
}

func hasUDP(list []portMap) bool {
	return slices.ContainsFunc(list, func(f portMap) bool { return f.Proto == "udp" })
}

func (p *peers) addrs(app string) []netip.Addr {
	return append(slices.Clone(p.running[app]), p.others[app]...)
}

// lookup answers a DNS question from a container: where the link called
// name of the asking container's app points. ok is false when the app has
// no such link. The container list is read again when it is older than a
// couple of seconds, which catches containers that stopped or crashed.
func (e *Engine) lookup(ctx context.Context, from netip.Addr, name string) (addrs []netip.Addr, ok bool) {
	e.peers.mu.Lock()
	defer e.peers.mu.Unlock()
	if time.Since(e.peers.at) > 2*time.Second {
		// A failed read answers from what is known.
		e.refreshLocked(ctx)
	}
	app, known := e.peers.appOf[from]
	if !known {
		return nil, false
	}
	if name == HostName {
		if gw, on := e.peers.hostAddr[app]; on {
			return []netip.Addr{gw}, true
		}
		return nil, false
	}
	for _, l := range e.peers.links[app] {
		if l.Name == name {
			if r := e.peers.running[l.To]; len(r) > 0 {
				return slices.Clone(r), true
			}
			return nil, true
		}
	}
	return nil, false
}

// allowed is one opening in the firewall between networks.
type allowed struct {
	from, to netip.Addr
	port     uint16
}

type firewall struct {
	bridges  []string
	allow    []allowed
	forwards []portMap
	host     []hostRule
}

func (f *firewall) sort() {
	slices.Sort(f.bridges)
	slices.SortFunc(f.allow, func(a, b allowed) int {
		if c := a.from.Compare(b.from); c != 0 {
			return c
		}
		if c := a.to.Compare(b.to); c != 0 {
			return c
		}
		return int(a.port) - int(b.port)
	})
	f.allow = slices.Compact(f.allow)
	slices.SortFunc(f.forwards, func(a, b portMap) int {
		return cmp.Or(
			cmp.Compare(a.Port, b.Port),
			cmp.Compare(a.Proto, b.Proto),
			a.IP.Compare(b.IP),
			a.to.Compare(b.to),
			cmp.Compare(a.Target, b.Target),
		)
	})
	f.forwards = slices.Compact(f.forwards)
	slices.SortFunc(f.host, func(a, b hostRule) int { return cmp.Compare(a.bridge, b.bridge) })
}

func (f *firewall) equal(g *firewall) bool {
	return slices.Equal(f.bridges, g.bridges) && slices.Equal(f.allow, g.allow) && slices.Equal(f.forwards, g.forwards) && slices.Equal(f.host, g.host)
}
