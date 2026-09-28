package install

import "fmt"

// Where an installed Zelie lives.
const (
	Binary    = "/usr/local/bin/zelie"
	UnitDir   = "/etc/systemd/system"
	PanelUser = "zelie"
	ProxyUser = "zelie-proxy"
)

// Services are Zelie's own systemd services, in the order they start.
var Services = []string{"zelie-core", "zelie-proxy", "zelie-panel"}

// Units returns the systemd units for opts, by file name.
func Units(opts Options) map[string]string {
	httpAddr, httpsAddr := ":80", ":443"
	bind := "AmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE\n"
	if opts.Mode == ModeTunnel {
		// Only the tunnel's connector, on this machine, talks to the proxy.
		httpAddr, httpsAddr = fmt.Sprintf("127.0.0.1:%d", opts.Port), ""
		bind = "CapabilityBoundingSet=\n"
	}
	// What the two unprivileged processes do not need, systemd takes away.
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
	return map[string]string{
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

// unitNames lists the unit files in start order.
func unitNames() []string {
	out := make([]string, len(Services))
	for i, s := range Services {
		out[i] = s + ".service"
	}
	return out
}
