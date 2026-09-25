# Zelie

Zelie is a server panel you install with one command. Connect a GitHub repository and
it builds and runs your app with a domain and HTTPS. Pick a game and it sets up the
server. Both live side by side on the same machine, managed from one place.

It ships as a single file, runs on your own server, and stays out of the way.

**Status:** early development. Nothing here is ready to use yet.

## What it will do

- **Deploy from GitHub.** Push to your repository and the new version goes live once it
  builds, its tests pass and it answers a health check. If anything fails, the old
  version keeps running and you see why.
- **Run game servers.** Import existing Pterodactyl and Pelican eggs, use the live
  console, SFTP and a file manager.
- **Set up databases for you.** MariaDB, PostgreSQL and Redis with one click. They are
  never exposed to the internet, and connection details are passed to your app
  automatically.
- **Update without downtime.** Updating Zelie never restarts your apps or game servers.

## How it is built

Zelie is written in Go and runs every app and game server in its own container on
containerd, with user namespaces turned on. The web panel runs as an unprivileged user
and talks to a small privileged core over a local socket, so a bug in the web interface
does not hand out root. [docs/architecture.md](docs/architecture.md) explains the design.

It runs on Debian and Ubuntu, on amd64 and arm64.

## License

The core is free software under the [GNU AGPL v3](LICENSE). Running a single server is
free, with every feature included. Managing several servers from one panel will be part
of a paid edition.

Security issues: see [SECURITY.md](SECURITY.md). Contributing: see
[CONTRIBUTING.md](CONTRIBUTING.md).

Zelie is made by [Cariacore](https://cariacore.com).
