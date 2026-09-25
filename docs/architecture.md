# Architecture

This document describes how Zelie is put together and why. It is written ahead of the
code and will change as the code catches up.

## One binary, three processes

Zelie ships as a single `zelie` binary. On a server it runs as three processes:

- **The core** runs as root. It talks to containerd, sets up networks and firewall
  rules, and manages disks. It does not listen on the network. It accepts a small set of
  typed requests, such as "start this container", over a Unix socket. The kernel tells it
  which user is on the other end, and only root and the panel get an answer.
- **The panel** runs as an unprivileged user. It serves the web interface and the API,
  handles logins, and receives webhooks from GitHub. It does not listen on the network
  either: the proxy hands it web traffic over a Unix socket.
- **The proxy** runs as its own unprivileged user and may only bind ports 80 and 443. It
  terminates HTTPS and forwards each domain to the container that serves it.

Keeping these apart means a bug in the web interface gives an attacker an unprivileged
account and a short list of allowed requests, not root on the server. It also means the
panel can restart or update while every site keeps serving: the proxy rarely restarts,
and when its routes change it swaps them in without dropping connections.

## Logging in

Every account needs a second step besides its password: a passkey or an authenticator
app. Until an account has one, logging in only lets it set one up. The first
administrator is created from a one-time link that `zelie setup-link` prints on the
server, so nobody who merely finds a fresh install can claim it.

Passwords are hashed with argon2id. Sessions live on the server and the browser holds
only a random token in a `__Host-` cookie; the database keeps a hash of it. Requests that
change something are refused when a browser says they come from another site, and the
interface runs under a Content-Security-Policy that allows no inline code except its
own, by hash.

## Containers

Every app and game server runs in its own container. Zelie talks to containerd directly
instead of going through Docker. That keeps it lighter, lets Zelie control its own
upgrades, and means restarting or updating the container runtime does not stop running
containers.

By default every container gets:

- a user namespace, so root inside the container is an unprivileged user on the host
- the default seccomp profile and a reduced set of capabilities
- limits on CPU, memory, disk and number of processes
- a place on its project's network and nowhere else: it can reach the internet and the
  other containers of the same project, but not other projects or services running on
  the host

Nothing runs privileged.

## Apps from GitHub

Zelie connects to GitHub through a GitHub App that it creates in your own account, with
access only to the repositories you choose. On a push:

1. The code is built in a temporary container. If the repository has a Dockerfile, it
   is used. Otherwise [Railpack](https://github.com/railwayapp/railpack) works out how
   to build it.
2. The project's tests run, if it has any.
3. The new version starts next to the old one and must pass a health check.
4. Traffic moves to the new version. The old one is kept for a quick rollback.

If any step fails, the old version keeps serving traffic and the panel shows what went
wrong.

## Web traffic

The proxy gets certificates from Let's Encrypt with
[CertMagic](https://github.com/caddyserver/certmagic), the library behind Caddy's
automatic HTTPS, and forwards requests with Go's standard reverse proxy. It only
requests certificates for domains that are routed, and it only forwards to container
addresses, never to the host. A panel can be reached through a domain, through a bare IP
address, or through a Cloudflare Tunnel with no open web ports at all.

## Databases

MariaDB, PostgreSQL and Redis run in containers on the private network of the project
that uses them. They never get a public port. When a database is attached to an app, the
connection details are passed to it as environment variables.

## Game servers

Zelie reads Pterodactyl and Pelican eggs, so existing game definitions work. The console
streams over a WebSocket authorised with a short-lived signed token. Files are available
over SFTP and in the panel, locked to the server's own volume.

## State and secrets

The panel keeps its state in SQLite. Secrets such as environment variables and database
passwords are encrypted at rest. The encryption key is shown once at install time so it
can be backed up, because losing it would mean losing the secrets.

## Updates

Releases are signed. Zelie downloads a new version, checks its signature, starts it and
checks its health. If the new version does not come up, it switches back to the old one.
Apps and game servers keep running throughout, since they live in containerd and not
inside the Zelie process.

## Several servers

A panel can manage more than one server in the paid edition. The extra servers run the
same binary in agent mode and connect out to the panel, so they need no open port for
it. That code is not part of this repository.
