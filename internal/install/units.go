package install

import (
	"fmt"
	"slices"
)

// Where an installed Zelie lives.
const (
	Binary    = "/usr/local/bin/zelie"
	UnitDir   = "/etc/systemd/system"
	PanelUser = "zelie"
	ProxyUser = "zelie-proxy"
	SFTPUser  = "zelie-sftp"
)

// What the unprivileged processes do not need, systemd takes away.
const hardening = `NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
`

// Services are Zelie's own systemd services, in the order they start. An
// update waits for each of them to answer.
var Services = []string{"zelie-core", "zelie-proxy", "zelie-panel"}

// SFTPService is the SFTP server. It is not in Services: the port it needs
// may be taken by something else on the machine, which must not fail an
// update.
const SFTPService = "zelie-sftp"

// allServices is Services and the SFTP service.
func allServices() []string { return append(slices.Clone(Services), SFTPService) }

// Units returns the systemd units for opts, by file name.
func Units(opts Options) map[string]string {
	httpAddr, httpsAddr := ":80", ":443"
	bind := "AmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE\n"
	if opts.Mode == ModeTunnel {
		// Only the tunnel's connector, on this machine, talks to the proxy.
		httpAddr, httpsAddr = fmt.Sprintf("127.0.0.1:%d", opts.Port), ""
		bind = "CapabilityBoundingSet=\n"
	}
	return map[string]string{
		SFTPService + ".service": SFTPUnit(),
		"zelie-core.service": `[Unit]
Description=Zelie core
Documentation=https://github.com/Caria-Core/zelie
After=network-online.target zelie-containerd.service
Wants=network-online.target
Requires=zelie-containerd.service

[Service]
ExecStart=` + Binary + ` core
Restart=always
RestartSec=2
# Containers belong to containerd, not to this process: stopping the core
# leaves every app running.
KillMode=process

[Install]
WantedBy=multi-user.target
`,
		"zelie-proxy.service": `[Unit]
Description=Zelie web proxy
Documentation=https://github.com/Caria-Core/zelie
After=network-online.target
Wants=network-online.target

[Service]
User=` + ProxyUser + `
ExecStart=` + Binary + ` proxy -http ` + httpAddr + ` -https '` + httpsAddr + `'
Restart=always
RestartSec=2
` + bind + `RuntimeDirectory=zelie-proxy
StateDirectory=zelie-proxy
StateDirectoryMode=0700
UMask=0077
` + hardening + `
[Install]
WantedBy=multi-user.target
`,
		"zelie-panel.service": `[Unit]
Description=Zelie panel
Documentation=https://github.com/Caria-Core/zelie
After=zelie-core.service zelie-proxy.service

[Service]
User=` + PanelUser + `
ExecStart=` + Binary + ` panel
Restart=always
RestartSec=2
CapabilityBoundingSet=
RuntimeDirectory=zelie-panel
StateDirectory=zelie-panel
StateDirectoryMode=0700
UMask=0077
` + hardening + `
[Install]
WantedBy=multi-user.target
`,
	}
}

// SFTPUnit is the unit of the SFTP server. It listens on a port above 1023,
// which the panel chooses, so it needs no capability at all. It only talks
// to the panel and the core over their sockets.
func SFTPUnit() string {
	return `[Unit]
Description=Zelie SFTP server
Documentation=https://github.com/Caria-Core/zelie
After=network-online.target zelie-core.service zelie-panel.service
Wants=network-online.target

[Service]
User=` + SFTPUser + `
ExecStart=` + Binary + ` sftp
Restart=always
RestartSec=2
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
StateDirectory=zelie-sftp
StateDirectoryMode=0700
UMask=0077
` + hardening + `
[Install]
WantedBy=multi-user.target
`
}

// unitNames lists the unit files in start order.
func unitNames() []string {
	all := allServices()
	out := make([]string, len(all))
	for i, s := range all {
		out[i] = s + ".service"
	}
	return out
}
