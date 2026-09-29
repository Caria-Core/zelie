# Installing Zelie

Zelie runs on Debian and Ubuntu with systemd, on amd64 or arm64. Install it as root:

```sh
curl -fsSL https://github.com/Caria-Core/zelie/releases/latest/download/install.sh | sh
```

`install.sh` downloads the release for the machine, checks its signature with the key it
carries, and only then runs `zelie install`, which asks a few questions. Set
`ZELIE_VERSION=v0.2.2` before `sh` to install a given release.

## How the panel is reached

**A domain.** Point the domain's DNS at the server. Zelie listens on ports 80 and 443 and
gets its certificates from Let's Encrypt. Nothing else may use those ports.

**A Cloudflare Tunnel.** No port opens to the internet. Zelie listens on `127.0.0.1:8480`
(or a port you choose), and the tunnel sends traffic there. If a tunnel connector already
runs on the server, Zelie leaves it alone; add a public hostname for the panel, and one
for each app domain later, pointing to `http://127.0.0.1:8480`. If none runs, paste the
install command Cloudflare shows for a new tunnel and Zelie installs the connector. Zelie
never gets access to your Cloudflare account.

**The server's IP address.** For trying Zelie out. The panel uses a self-signed
certificate, so the browser warns about it.

The questions can also be answered with flags, for example from automation:

```sh
curl -fsSL https://github.com/Caria-Core/zelie/releases/latest/download/install.sh | sh -s -- --mode tunnel --host panel.example.com
```

At the end, the install prints a one-time link, valid for 24 hours, to create the
administrator.

## What the install changes

- `/usr/local/bin/zelie`, and Zelie's own containerd and runc under `/usr/local/lib/zelie`, with
  their settings in `/etc/zelie`.
  They do not touch Docker, which can keep running next to them.
- Three system users, `zelie`, `zelie-proxy` and `zelie-sftp`, which cannot log in.
- Five systemd services: `zelie-containerd`, `zelie-core`, `zelie-proxy`, `zelie-panel` and
  `zelie-sftp`. The SFTP server listens on port 2222 by default; the Server page changes it. If
  ufw is on, the install opens port 2222 in it.
- An nftables table named `zelie`, which only concerns Zelie's own container networks.
  Other rules, such as ufw's or Docker's, are left as they are.
- Data under `/var/lib/zelie`, `/var/lib/zelie-panel`, `/var/lib/zelie-proxy` and
  `/var/lib/zelie-sftp`.

Running the install again is safe. It finishes an install that stopped halfway and
leaves alone what is already done.

## Updates

The Server page in the panel shows when a new release is out, with its notes. Updating
downloads the release, checks its signature, and restarts Zelie's own services. Apps,
databases and game servers keep running. If the new version does not answer within a
minute and a half, the old one is put back and the page says why. The previous binary
stays next to the new one as `/usr/local/bin/zelie.old`. A server installed before SFTP
existed gets the SFTP user and service the first time it starts on a version that has them.

Running the install command again also updates Zelie.
