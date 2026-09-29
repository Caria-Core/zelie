package engine

import (
	"fmt"
	"strings"
)

// ConfigFile renders containerd's configuration. The CRI plugins are turned
// off because Zelie is not a Kubernetes node and they would only add attack
// surface and memory use.
func ConfigFile(p Paths) string {
	return fmt.Sprintf(`# Managed by Zelie. Changes are overwritten on upgrade.
version = 3
root = %q
state = %q
disabled_plugins = [
  "io.containerd.grpc.v1.cri",
  "io.containerd.cri.v1.images",
  "io.containerd.cri.v1.runtime",
]

[grpc]
  address = %q

[ttrpc]
  address = %q
`, p.Root, p.State, p.Socket, p.Socket+".ttrpc")
}

// UnitFile renders the systemd unit. KillMode=process matters most: when
// containerd stops or restarts, systemd kills only containerd itself and
// leaves the shims, and therefore every running container, alone.
// LimitNOFILE is the ceiling for what containers can ask for: see
// Engine.openFiles.
func UnitFile(p Paths) string {
	path := strings.Join([]string{p.Bin, "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}, ":")
	return fmt.Sprintf(`# Managed by Zelie. Changes are overwritten on upgrade.
[Unit]
Description=containerd for Zelie
Documentation=https://github.com/Caria-Core/zelie
After=network.target local-fs.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=%s --config %s
Environment=PATH=%s
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNOFILE=%d
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
`, p.Bin+"/containerd", p.Config, path, MaxOpenFiles)
}
