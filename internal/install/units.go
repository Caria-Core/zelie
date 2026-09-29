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

// SFTPService is the SFTP server, and SFTPSocket the socket systemd holds for
// it. Neither is in Services: the port may be taken by something else on the
// machine, which must not fail an update.
const (
	SFTPService = "zelie-sftp"
	SFTPSocket  = SFTPService + ".socket"
)

// allServices is Services and the SFTP service.
func allServices() []string { return append(slices.Clone(Services), SFTPService) }

// enabledUnits are the units that are enabled and started at install: the
// services, and the SFTP socket, which starts its service on demand.
func enabledUnits() []string { return append(slices.Clone(Services), SFTPSocket) }

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
		SFTPSocket:               SFTPSocketUnit(),
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

// SFTPUnit is the unit of the SFTP server. It has no [Install] section: the
// socket starts it when someone connects, and it exits when idle. It gets
// its port from the socket, which is above 1023, so it needs no capability
// at all. It only talks to the panel and the core over their sockets.
func SFTPUnit() string {
	return `[Unit]
Description=Zelie SFTP server
Documentation=https://github.com/Caria-Core/zelie
Requires=` + SFTPSocket + `
After=` + SFTPSocket + ` zelie-core.service zelie-panel.service

[Service]
User=` + SFTPUser + `
# An older binary, such as one an update went back to, does not know this
# command and exits with 2. systemd then skips the unit without calling it
# failed and without restarting it, instead of starting it over and over.
ExecCondition=` + Binary + ` sftp --check
ExecStart=` + Binary + ` sftp
# Not "always": the server exits cleanly when nobody is connected, and the
# socket starts it again with the next connection.
Restart=on-failure
RestartSec=2
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
StateDirectory=zelie-sftp
StateDirectoryMode=0700
UMask=0077
` + hardening
}

// SFTPSocketUnit is the socket systemd listens on for the SFTP server. The
// port is the default; the core changes it with a drop-in when the panel
// says so.
func SFTPSocketUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Zelie SFTP port
Documentation=https://github.com/Caria-Core/zelie

[Socket]
ListenStream=%d
Accept=no

[Install]
WantedBy=sockets.target
`, DefaultSFTPPort)
}

// unitNames lists the unit files in start order.
func unitNames() []string {
	all := allServices()
	out := make([]string, 0, len(all)+1)
	for _, s := range all {
		out = append(out, s+".service")
	}
	return append(out, SFTPSocket)
}
