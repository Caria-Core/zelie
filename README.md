# Zelie

Zelie is a server panel you install with one command. Point it at a GitHub repository
and it builds and runs your app with a domain and HTTPS. Pick a game and it sets up the
server. Both live side by side on the same machine, managed from one place.

It ships as a single file, runs on your own server, and stays out of the way.

**Status:** early development. The pieces below marked as working run on a test
machine; there is no release yet and nothing here is ready for production.

Website, documentation and support: [zelie.cariacore.com](https://zelie.cariacore.com)
(being built).

## What works today

- **Apps from GitHub.** Give it a repository and a branch. Zelie fetches the code,
  builds it with the repository's Dockerfile or, when there is none, works out the build
  on its own with [Railpack](https://github.com/railwayapp/railpack), which knows Node,
  Python, Go, static sites and more.
- **Push to deploy.** Connect GitHub and Zelie creates a GitHub App in your own account,
  with access only to the repositories you choose. Private repositories work, every push
  to the branch is deployed, and the commit on GitHub shows whether it went live. If
  several pushes arrive during a build, only the newest is deployed next. So far this has
  been tested against a stand-in for GitHub's API, not yet against GitHub itself.
- **Apps from an image.** Run any public image, such as `nginx:alpine`.
- **Deployments that fail safely.** A new version starts next to the old one and must
  stay up before the domain moves over. If the build fails or the app crashes on start,
  the old version keeps serving and the deployment's log shows why, including the app's
  last output. Build logs stream to the browser while they run.
- **Domains and HTTPS.** Give an app a domain and the proxy routes it and gets its
  certificate from Let's Encrypt. So far this has been tried with self-signed
  certificates only, not yet against Let's Encrypt itself.
- **Environment variables, with secrets.** Mark a variable secret and it is sealed as
  it is saved. Not even the panel can read it back: only the privileged core opens it,
  when it starts the container. Paste a whole `.env` file to fill in the list.
- **Live logs** of every app, in the browser.
- **Careful logins.** The administrator needs a passkey or an authenticator app
  as well as a password, with recovery codes as a fallback. Changing any of these, or
  the password, asks for a fresh second step. The account page lists every browser
  that is logged in, including ones that got the password but not the second step, and
  logs them out.
- **A calm interface.** Light and dark themes, works on a phone, and ships inside the
  binary: no separate web server, no CDN, no tracking.

## What is coming

- **Safer deployments.** Run the project's tests before a version goes live, check its
  health over HTTP, and roll back to an earlier version with one click.
- **Game servers.** Import existing Pterodactyl and Pelican eggs, with a live console,
  SFTP, a file manager, startup settings, schedules and port management.
- **Databases.** MariaDB, PostgreSQL and Redis with one click, never exposed to the
  internet, with their connection details passed to your app. Backups on a schedule, to
  the server's disk and to S3.
- **One-command install and updates** that never restart your apps or game servers,
  and reaching the panel through a Cloudflare Tunnel with no open web ports.

## How it is built

Zelie is written in Go. Three processes run from the same binary, each with only the
rights it needs:

- The **core** runs as root and is the only part that can start containers. It listens
  on a local socket, never on the network, and answers only root and the panel.
- The **panel** serves the web interface as an unprivileged user. It has no network
  port either: the proxy hands it web traffic over a socket. A bug in the web interface
  gives an attacker that user, not root.
- The **proxy** terminates HTTPS and may do nothing else. Updating or restarting the
  panel does not drop a single connection.

Every app runs in its own container on containerd, with a user namespace so that root
inside is an ordinary user outside, a seccomp profile, dropped capabilities and limits
on memory, CPU and processes. Each app gets its own network. Builds run inside such a
container too, never directly on the host, and each build step gets a sandbox of its own.

[docs/architecture.md](docs/architecture.md) explains each part and why it is that way.

It runs on Debian and Ubuntu, on amd64 and arm64.

## License

The core is free software under the [GNU AGPL v3](LICENSE). Everything on a single node
is free, with no limit on how many apps, game servers or databases you run on it. Running
across several nodes, for hosting companies and for scaling out, will be part of the paid
Pro edition.

Security issues: see [SECURITY.md](SECURITY.md). Contributing: see
[CONTRIBUTING.md](CONTRIBUTING.md).

Zelie is made by [Cariacore](https://cariacore.com).
