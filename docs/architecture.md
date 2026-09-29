# Architecture

This document describes how Zelie is put together and why. Parts of it, such as game
servers, updates and several servers, describe plans the code has not reached yet.

## One binary, four processes

Zelie ships as a single `zelie` binary. On a server it runs as four processes:

- **The core** runs as root. It talks to containerd, sets up networks and firewall
  rules, and manages disks. It does not listen on the network. It accepts a small set of
  typed requests, such as "start this container", over a Unix socket. The kernel tells it
  which user is on the other end, and only root and the panel get an answer, apart from the
  SFTP server, which may use the file routes.
- **The panel** runs as an unprivileged user. It serves the web interface and the API,
  handles logins, and receives webhooks from GitHub. It does not listen on the network
  either: the proxy hands it web traffic over a Unix socket.
- **The proxy** runs as its own unprivileged user and may only bind ports 80 and 443. It
  terminates HTTPS and forwards each domain to the container that serves it.
- **The SFTP server** runs as its own unprivileged user and listens on port 2222 by
  default, or on another port chosen on the Server page. It has no access to the volumes.
  It asks the panel who may log in to which game server, and passes every file operation to
  the core. The core lets its user call the file routes and no others, and only on the volumes
  of game servers: the panel gives the core that list, and the core keeps it across restarts.

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

Changing how an account logs in (its password, passkeys, authenticator app or recovery
codes) needs a second step from the last 15 minutes. Logging in counts. After that the
panel asks again, so a browser left open or a stolen cookie is not enough to take the
account over. A password alone does not count, and the last second step on an account
cannot be removed. Changing the password logs out every other browser.

Wrong passwords and codes are counted in the database, per address and per account, so
restarting the panel does not reset them. After a few failures from one address, each
further try must come with a small proof of work the browser solves, about a second on a
phone, which grows as the failures do; nothing is sent to a third party. An owner locked
out for good runs `zelie reset-login` on the server: root can read everything anyway, so
the link it prints may set a new password. It works once, for an hour, and removes the
account's second steps, which it must set up again before doing anything else.

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
2. The project's tests run, if it has any: in the new image, in a container of their
   own, for at most ten minutes. For Node projects built with Railpack, Zelie finds the
   test script itself; any other command can be set in the app's settings.
3. The new version starts next to the old one and must pass a health check: an app
   with a domain must answer HTTP with a status below 500, an app without one must stay
   up.
4. Traffic moves to the new version. The images of the last five live versions are
   kept, so restarting or rolling back needs no build.

If any step fails, the old version keeps serving traffic and the panel shows what went
wrong. The commit on GitHub is marked as building, live or failed.

The App is created with GitHub's manifest flow: the panel describes the App, you confirm
it on GitHub, and GitHub hands the panel its private key and webhook secret. Both are
encrypted with the panel's key before they are stored. The App may read code and set
commit statuses, nothing else. It signs its requests to GitHub with short-lived tokens
that reach only the repositories it was given.

Webhooks need no login, so each one is checked against the webhook secret before
anything else, and a delivery that was already handled is ignored if it arrives again.
For pushes to work, GitHub has to reach the panel at its domain; the GitHub page in the
panel shows whether its last delivery arrived. Public repositories deploy without the
App, when you start the deployment yourself.

Builds run [BuildKit](https://github.com/moby/buildkit) inside an ordinary Zelie
container, with its own user namespace and resource limits, not as a service on the
host. Each build step then runs in a sandbox of its own inside that container. The
source archive is unpacked in a separate container that has nothing else mounted, so an
archive with crafted symlinks has nothing to reach. Package manager caches are kept per
app, and builds run one at a time.

## Web traffic

The proxy gets certificates from Let's Encrypt with
[CertMagic](https://github.com/caddyserver/certmagic), the library behind Caddy's
automatic HTTPS, and forwards requests with Go's standard reverse proxy. It only
requests certificates for domains that are routed, and it only forwards to container
addresses, never to the host. A panel can be reached through a domain, through a bare IP
address, or through a Cloudflare Tunnel with no open web ports at all.

## Databases

MariaDB, PostgreSQL and Redis each run in a container of their own, with a volume for
their files. A database never gets a public port. Each app is on a network of its own and the
firewall drops traffic between them; linking a database to an app opens one way through,
to the database's one port, so the app reaches it by name. The app also gets variables to
connect with. The
password is sealed for the core: the panel stores it but cannot read it, and only the
core opens it, when it starts the database or a linked app.

Anything that reads a database, such as a backup or the Data tab, runs the engine's own
client inside the database's container, started by the core. The panel asks for what it
wants in typed requests; the core builds the query, checks table and column names
against the database's catalog, and runs it read-only with a time limit.

Desktop tools reach a database through an SSH tunnel. The core listens on a port on
127.0.0.1 and passes each connection to the database's container, and the tool signs in
as a user made for it alone, so its password can change or go without touching the apps.

Backups are encrypted with [age](https://age-encryption.org) before they touch the disk.
The key is the core's; a recovery file, which the panel asks you to save, lets another
server open them.

## Game servers

Zelie reads Pterodactyl and Pelican eggs, so existing game definitions work. The console
streams over a WebSocket authorised with a short-lived signed token. Files are available
over SFTP and in the panel, locked to the server's own volume.

The panel's file manager does its work in the core, which opens the server's volume as an
`os.Root`. Every path goes through it, so `..`, an absolute path or a symbolic link that a
player or an installer left in the volume cannot lead out of it, even if the link is swapped
while a request runs. The panel is unprivileged and sends only typed requests: list, read or
save a text file, make a folder, rename, delete, upload, download, compress and extract.
Files are replaced whole through a temporary file, so a running game never reads half a
file, and every file written belongs to the user the game runs as. Archives are unpacked
into one folder and stopped at the first entry that would leave it, a link that points out
of it, a device file, or a size or entry count over the limit. Unlike preparing the files
for a start, this works while the server runs.

SFTP uses the same file operations. A login is either an SSH key that an administrator added
to their account, or the server's own SFTP password, which is shown once and kept as an
argon2id hash. The password of an account is never accepted, so SFTP is no way around its
second step. The user name is the server's name, for keys and passwords alike, since a key
belongs to one account only and says who is logging in. The server speaks only the `sftp`
subsystem: no shell, no commands, no port forwarding. Wrong passwords are counted per
address and per server, as on the login page, and one address may hold only a few
connections open. Files are written whole, through a temporary file in the core, so an
upload that breaks halfway leaves the old file as it was; for the same reason a file can
only be written from start to end, which every common SFTP program does. The host key is
made on first start and its fingerprint is shown on the Server page.

Players connect straight to the game server, not through the proxy. The administrator
gives Zelie a pool of ports, as in Pterodactyl and Pelican, and each server takes some of
them. Zelie suggests a range that leaves out ports other software on the machine already
uses. The core opens a port with an nftables rule that sends traffic for it to the
container, and one that lets exactly that traffic through; where ufw or another iptables
firewall is present, the same opening is made there. The rules follow the container when
it restarts with a new address, and go when the server is deleted.

The address the panel shows players is not the one the panel was opened with, which may
be a tunnel that game traffic never crosses. The core reads the machine's network cards
and picks the first public IPv4 address, leaving out its own bridges and the container
runtimes' interfaces. A machine behind NAT has none, so the panel shows the private
address of the card the default route uses and says players outside the network may not
reach it. Nothing is asked of an outside service. The administrator can replace the
address with a name or IP of their own on the Server page.

A game server runs as an ordinary user, 988 inside its container as in Wings. Before
each start the core gives that user every file of the server's volume and edits the
config files the egg lists, staying inside the volume however a link in it points. The
image's own entrypoint starts the game with its console open. To stop it Zelie sends the
egg's stop command or signal, and kills the game only if it is still running a minute
later. A server that exits by itself is started again like a crashing app, with a
growing delay.

A new server is set up by the egg's install script. Zelie runs it once, in a container
of its own that is built from the egg's install image, with the server's files mounted
at `/mnt/server` and the same isolation and limits as any other container. Its output is
kept as a log. The script runs again only when you ask for a reinstall, and Zelie backs up
the server's files first; it does not delete them.

## State and secrets

The panel keeps its state in SQLite. An app's variables can be marked secret. The panel
seals those with a public key whose private half only the core holds, and stores just
the sealed value; the core opens it when it starts the app's container. A copy of the
panel's database, or a bug that lets someone read it, therefore reveals no secret, and
the interface never shows a secret again once it is saved. Someone in full control of
the panel could still deploy a version of the app that prints its environment, so this
protects the stored data, not against a taken-over panel.

Builds can read the variables too, since frameworks such as Next.js need some of them
at build time. The core opens the sealed ones and hands every variable to BuildKit as a
secret: a file mounted for the build, not a build argument, so its value stays out of the
image and its history. Only a build step that prints a value would show it in the build
log.

The sealed text names its app, so a value cannot be moved to another app. The core's key
will be part of what the installer asks you to back up, because losing it would mean
losing the secrets.

## Updates

Releases are signed. Zelie downloads a new version, checks its signature, starts it and
checks its health. If the new version does not come up, it switches back to the old one.
Apps and game servers keep running throughout, since they live in containerd and not
inside the Zelie process.

## Several servers

A panel can manage more than one server in the paid edition. The extra servers run the
same binary in agent mode and connect out to the panel, so they need no open port for
it. That code is not part of this repository.
