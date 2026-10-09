# Installing Zelie

## Before you start

- A server running Debian or Ubuntu with systemd and cgroup v2, on amd64 or arm64. Any
  current release of either has both.
- Root access, and `curl`, `openssl` and `sha256sum`, which most servers already have.
- For a domain: a DNS record pointing it at the server, and ports 80 and 443 free. For a
  Cloudflare Tunnel: a domain on Cloudflare.

Docker can keep running next to Zelie. Zelie brings its own containerd and never touches
Docker's containers or firewall rules. Another panel, such as Pterodactyl or Pelican, can
stay too, as long as you reach Zelie through a Cloudflare Tunnel or its web server moves
off ports 80 and 443.

## Install

As root:

```sh
curl -fsSL https://github.com/Caria-Core/zelie/releases/latest/download/install.sh | sh
```

`install.sh` downloads the release for the machine and checks its signature with the key
it carries. If the check fails, nothing is installed. Then it runs `zelie install`, which
checks the server and asks a few questions.

To install a given release instead of the latest, set `ZELIE_VERSION` on the `sh` side:

```sh
curl -fsSL https://github.com/Caria-Core/zelie/releases/latest/download/install.sh | ZELIE_VERSION=v0.7.11 sh
```

## The questions

The first question is how people will reach the panel. The answer decides how Zelie
serves every app with a domain later, too.

```
How will people reach the panel?

  1) A domain that points to this server.
  2) A Cloudflare Tunnel.
  3) This server's IP address, with a self-signed certificate.
```

**1. A domain.** Zelie listens on ports 80 and 443 and gets certificates from Let's
Encrypt, for the panel and for each app you give a domain. Choose this when the server
has a public address and nothing else serves websites on it. The install asks for:

- the panel's domain, such as `panel.example.com`, which must already point to the
  server;
- an email address for Let's Encrypt, which writes to it if a certificate has trouble.

If ports 80 or 443 are taken, most likely by another web server, the install stops
before it changes anything and says so.

**2. A Cloudflare Tunnel.** No port opens to the internet. Zelie listens on one port of
the server itself, and Cloudflare's connector brings the traffic there and handles
HTTPS. Choose this when your domain is on Cloudflare, when another web server already
has ports 80 and 443, or when the server is behind NAT. The install asks for:

- the panel's domain;
- the local port the tunnel sends traffic to. Press Enter to keep the current one, 8480
  on a first install;
- if no Cloudflare connector runs on the server yet, the install command Cloudflare
  shows for a new tunnel (Zero Trust, Networks, Tunnels, then Debian). Zelie reads the
  token from it and installs the connector. It never gets access to your Cloudflare
  account.

If a connector already runs, Zelie leaves it alone. Either way, add a public hostname in
Cloudflare for the panel, and later one for each app domain, all pointing to
`http://127.0.0.1:8480`. The install reminds you at the end.

**3. The server's IP address.** For trying Zelie out. The panel uses a self-signed
certificate, so the browser warns about it, and passkeys need a domain, so you log in
with an authenticator app instead.

### Without questions

Flags answer the questions, for example from automation:

| Flag | Meaning |
|---|---|
| `--mode` | `domain`, `tunnel` or `ip` |
| `--host` | the panel's domain, or the server's IP address with `--mode ip` |
| `--email` | the Let's Encrypt email, with `--mode domain` |
| `--port` | the local port for the tunnel; if left out, the one an earlier install used, or 8480 |

```sh
curl -fsSL https://github.com/Caria-Core/zelie/releases/latest/download/install.sh | sh -s -- --mode domain --host panel.example.com --email you@example.com
```

The Cloudflare connector's command has no flag, so `--mode tunnel` without a terminal
only works when a connector already runs on the server.

## Creating the administrator

The install ends with a link like `https://panel.example.com/setup#…`. It works once and
for 24 hours. If it runs out, `zelie setup-link` prints a new one, as long as no
administrator exists yet.

The link asks for an email address and a password of at least ten characters. Then it
asks for a second step, which every administrator needs: a passkey (Face ID, Touch ID,
Windows Hello or a security key) or an authenticator app. After the first one, Zelie
shows ten recovery codes, once. Keep them somewhere safe. Nothing else in the panel opens
until the second step is set up.

If you lose both your second step and your recovery codes, run `zelie reset-login` on
the server. It prints a link, valid once for an hour, that sets a new password. It also
removes the account's passkeys, authenticator, recovery codes and SSH keys and logs it
out everywhere, so the next login sets up a second step again. With more than one
administrator, name the account: `zelie reset-login you@example.com`.

## After the install

The panel is empty at first. A few things are worth doing early:

- **GitHub.** The GitHub page creates a GitHub App in your own account or organization,
  with access only to the repositories you choose. Deploying on every push needs the
  panel to be reachable from the internet, which it is with a domain or a tunnel.
- **Game server ports.** The first game server asks for a block of ports for players.
  Zelie suggests one it found free on the server, starting at 25565, and leaves out ports
  other programs use. Add more later on the Server page.
- **Game address.** The Server page shows the address players connect to, found from the
  server's network cards. If it shows a private address, give it the public IP or a
  domain.
- **SFTP.** The SFTP server listens on port 2222. If another program has that port, such
  as Pterodactyl's Wings, the install finishes anyway and says so; choose another on the
  Server page. If the port will not open, SFTP stays where it was and the page says why.
- **Off-site backups.** The Backups page takes any S3-compatible storage. Download the
  recovery file there too: without it, no other server can open your backups.

## What the install changes

- `/usr/local/bin/zelie`, and Zelie's own containerd and runc under
  `/usr/local/lib/zelie`, with their settings in `/etc/zelie`. They do not touch Docker.
- Three system users that cannot log in: `zelie` for the panel, `zelie-proxy` and
  `zelie-sftp`.
- Five systemd services, `zelie-containerd`, `zelie-core`, `zelie-proxy`, `zelie-panel`
  and `zelie-sftp`, and a socket, `zelie-sftp.socket`. The socket holds the SFTP port.
  The SFTP server starts when someone connects and stops after five idle minutes, so it
  uses no memory while nobody needs it. A port chosen on the Server page goes in
  `/etc/systemd/system/zelie-sftp.socket.d/port.conf`.
- If ufw is on, a rule that opens port 2222. Nothing else in ufw changes. With a domain,
  make sure ports 80 and 443 are open in any firewall in front of the server.
- nftables tables named `zelie` and `zelie-nat`, which only concern Zelie's containers
  and the game ports it forwards. Other rules, such as ufw's or Docker's, are left as
  they are.
- Data under `/var/lib/zelie`, `/var/lib/zelie-panel`, `/var/lib/zelie-proxy` and
  `/var/lib/zelie-sftp`, and the SFTP host key in `/etc/zelie-sftp`.

Running the install again is safe. It finishes an install that stopped halfway, leaves
alone what is already done, and updates an older version. Behind a tunnel it keeps the
port the proxy already uses; if you give another one with `--port`, the install says so,
and the Cloudflare routes need to follow.

## Updates

The Server page shows when a new release is out, with its notes. Updating downloads the
release, checks its signature and restarts Zelie's own services. Apps, databases and
game servers keep running. If the new version does not answer within a minute and a
half, the old one is put back, together with the panel's database as it was before the
update, and the page says why.

The previous binary stays next to the new one as `/usr/local/bin/zelie.old`, for that
rollback. The copy of the database is dropped once an update has succeeded, and a release
can change how the panel's database is laid out, which an older version refuses to open.
So going back by hand with `zelie.old` after a good update may not start the panel unless
you also put back a copy of `/var/lib/zelie-panel/panel.db` from before the update.

## Commands

| Command | What it does |
|---|---|
| `zelie install` | installs or updates Zelie; `install.sh` runs it |
| `zelie setup-link` | prints a new link to create the administrator, until one exists |
| `zelie reset-login [email]` | prints a link to set a new password and second step |
| `zelie version` | prints the version |

The other commands, such as `zelie core` and `zelie panel`, are what the services run.
