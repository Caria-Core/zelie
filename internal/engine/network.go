package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containernetworking/cni/libcni"
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// NetworkRange holds every container network. Each network gets a /24 out of it. It is rarely used by hosting
// providers or home routers, which keeps clashes with the host's own
// networks unlikely.
var NetworkRange = netip.MustParsePrefix("10.210.0.0/16")

const maxNetworks = 256

// network is one isolated bridge. Containers on the same network can reach
// each other; containers on different networks cannot.
type network struct {
	Name   string `json:"name"`
	Index  int    `json:"index"`
	Subnet string `json:"subnet"`
}

func (n network) bridge() string { return fmt.Sprintf("zelie%d", n.Index) }

// networks keeps the list of networks in a small JSON file. Only the core
// touches it, and the mutex covers concurrent requests inside the core.
type networks struct {
	mu    sync.Mutex
	paths Paths
	cni   *libcni.CNIConfig
}

func newNetworks(p Paths) *networks {
	return &networks{
		paths: p,
		// A cache directory of our own keeps Zelie's CNI state apart from
		// any other CNI user on the machine.
		cni: libcni.NewCNIConfigWithCacheDir([]string{p.CNI}, filepath.Join(p.Data, "cni-cache"), nil),
	}
}

func (n *networks) file() string { return filepath.Join(n.paths.Data, "networks.json") }

func (n *networks) load() (map[string]network, error) {
	b, err := os.ReadFile(n.file())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]network{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]network{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("read %s: %w", n.file(), err)
	}
	return m, nil
}

// ensure returns the named network, creating its entry on first use. The
// bridge itself is created by the CNI plugin when the first container joins.
func (n *networks) ensure(name string) (network, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	m, err := n.load()
	if err != nil {
		return network{}, err
	}
	if nw, ok := m[name]; ok {
		return nw, nil
	}
	used := make(map[int]bool, len(m))
	for _, nw := range m {
		used[nw.Index] = true
	}
	for i := range maxNetworks {
		if used[i] {
			continue
		}
		base := NetworkRange.Addr().As4()
		base[2] = byte(i)
		nw := network{Name: name, Index: i, Subnet: netip.PrefixFrom(netip.AddrFrom4(base), 24).String()}
		m[name] = nw
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return network{}, err
		}
		if _, err := writeIfChanged(n.file(), b, 0o600); err != nil {
			return network{}, err
		}
		return nw, nil
	}
	return network{}, errors.New("no free network left")
}

// confList is the CNI configuration for a network.
//
// The firewall plugin's same-bridge policy drops traffic arriving from other
// bridges, which is what keeps networks apart. ipMasq lets containers reach
// the internet through the host.
func (n *networks) confList(nw network) (*libcni.NetworkConfigList, error) {
	conf := map[string]any{
		"cniVersion": "1.0.0",
		"name":       "zelie-" + nw.Name,
		"plugins": []any{
			map[string]any{
				"type":        "bridge",
				"bridge":      nw.bridge(),
				"isGateway":   true,
				"ipMasq":      true,
				"hairpinMode": true,
				"ipam": map[string]any{
					"type":    "host-local",
					"ranges":  [][]map[string]string{{{"subnet": nw.Subnet}}},
					"routes":  []map[string]string{{"dst": "0.0.0.0/0"}},
					"dataDir": filepath.Join(n.paths.Data, "ipam"),
				},
			},
			map[string]any{
				"type":          "firewall",
				"backend":       "iptables",
				"ingressPolicy": "same-bridge",
			},
		},
	}
	b, err := json.Marshal(conf)
	if err != nil {
		return nil, err
	}
	return libcni.ConfListFromBytes(b)
}

// attach puts the network namespace at netnsPath on the network and returns
// the container's address.
func (n *networks) attach(ctx context.Context, name, id, netnsPath string) (netip.Addr, error) {
	nw, err := n.ensure(name)
	if err != nil {
		return netip.Addr{}, err
	}
	list, err := n.confList(nw)
	if err != nil {
		return netip.Addr{}, err
	}
	res, err := n.cni.AddNetworkList(ctx, list, &libcni.RuntimeConf{ContainerID: id, NetNS: netnsPath, IfName: "eth0"})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("attach %s to network %s: %w", id, name, err)
	}
	r, err := types100.NewResultFromResult(res)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, ip := range r.IPs {
		if a, ok := netip.AddrFromSlice(ip.Address.IP); ok {
			return a.Unmap(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("network %s gave %s no address", name, id)
}

// detach undoes attach. It is safe to call more than once.
func (n *networks) detach(ctx context.Context, name, id, netnsPath string) error {
	n.mu.Lock()
	m, err := n.load()
	n.mu.Unlock()
	if err != nil {
		return err
	}
	nw, ok := m[name]
	if !ok {
		return nil
	}
	list, err := n.confList(nw)
	if err != nil {
		return err
	}
	return n.cni.DelNetworkList(ctx, list, &libcni.RuntimeConf{ContainerID: id, NetNS: netnsPath, IfName: "eth0"})
}

// resolvConf writes the DNS configuration containers get. The host often
// points at a local stub resolver such as 127.0.0.53, which a container
// cannot reach, so loopback servers are dropped and the upstream servers
// behind systemd-resolved are used when they are known.
func resolvConf(p Paths) (string, error) {
	var servers []string
	for _, f := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		servers = nameservers(f)
		if len(servers) > 0 {
			break
		}
	}
	if len(servers) == 0 {
		return "", errors.New("found no DNS server for containers: /etc/resolv.conf only lists loopback addresses")
	}
	var b strings.Builder
	b.WriteString("# Written by Zelie.\n")
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	path := filepath.Join(p.Data, "resolv.conf")
	if _, err := writeIfChanged(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func nameservers(file string) []string {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(fields[1])
		if err != nil || a.IsLoopback() {
			continue
		}
		out = append(out, a.String())
	}
	return out
}
