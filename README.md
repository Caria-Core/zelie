# Zelie Panel

Zelie is a server panel for your own machine. Give it a Node.js, Python or Go app from
GitHub, a Docker image, or soon a game server, and it builds it, runs it and puts it
online with HTTPS. You skip the setup: no reverse proxy to configure, no certificates to
renew, no Dockerfile unless you want one.

It is meant to replace the older panels, such as Pterodactyl, Pelican and cPanel, with
something lighter and safer. Zelie is a single binary, with no web server, PHP or
database server of its own to look after. Every app runs in its own locked-down
container, and a setup wizard asks a few questions and gets you to your first app. When
one server is not enough, the same panel will run several.

> [!WARNING]
> **Pre-alpha.** Zelie is in early development. There is no release yet, and it is not
> ready for production or for any server you care about.

![Zelie's home page: three apps and two databases, all live](docs/images/overview.png)

## What it does

- Deploys apps from a GitHub repository or a container image, with a domain and HTTPS.
- Builds with your Dockerfile, or works out the build itself with
  [Railpack](https://github.com/railwayapp/railpack).
- Runs your tests before a new version goes live, and keeps the old one if anything fails.
- Runs PostgreSQL, MariaDB and Redis, reachable only by the apps you link them to.
- Backs up databases and app volumes on a schedule, on the server and in S3.
- Shows a database's tables and keys in the browser, read-only.
- Seals secrets as they are saved, so the web interface cannot read them back.
- Asks for a passkey or an authenticator app at every login.

![A PostgreSQL database's tables, browsed read-only in Zelie](docs/images/database.png)

Coming next: game servers from Pterodactyl and Pelican eggs, the one-command installer
with updates, and several servers from one panel.

[docs/features.md](docs/features.md) describes each feature, and
[docs/architecture.md](docs/architecture.md) explains how Zelie is built and why.

## Install

Not yet. The first release will install with one command on Debian or Ubuntu, amd64 or
arm64. Until then, [CONTRIBUTING.md](CONTRIBUTING.md) shows how to build and run it.

## License

The core is free software under the [GNU AGPL v3](LICENSE). Everything on a single server
is free, with no limit on apps, databases or game servers. Running across several servers
will be part of a paid edition.

Security issues: [SECURITY.md](SECURITY.md). Made by [Cariacore](https://cariacore.com).
