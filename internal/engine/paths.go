// Package engine installs and talks to Zelie's own containerd.
package engine

import "path/filepath"

// Paths says where Zelie's containerd lives. None of them overlap with a
// containerd installed by Docker or the distribution, so both can run on the
// same machine without touching each other.
type Paths struct {
	Bin    string // containerd, the shim, ctr and runc
	CNI    string // network plugins
	Config string
	Root   string // images, snapshots, container metadata
	State  string // runtime state, cleared on reboot
	Socket string
	Unit   string // systemd unit file
	Logs   string // container output, one file per container
	Data   string // Zelie's own state: networks, IP leases, generated files
	NetNS  string // network namespaces, one per container
}

var DefaultPaths = Paths{
	Bin:    "/usr/local/lib/zelie/bin",
	CNI:    "/usr/local/lib/zelie/cni",
	Config: "/etc/zelie/containerd.toml",
	Root:   "/var/lib/zelie/containerd",
	State:  "/run/zelie/containerd",
	Socket: "/run/zelie/containerd.sock",
	Unit:   "/etc/systemd/system/zelie-containerd.service",
	Logs:   "/var/lib/zelie/logs",
	Data:   "/var/lib/zelie/engine",
	NetNS:  "/run/zelie/netns",
}

func (p Paths) Runc() string { return filepath.Join(p.Bin, "runc") }
