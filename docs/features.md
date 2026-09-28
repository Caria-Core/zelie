# Features

What Zelie does today, one area at a time. It is pre-alpha: all of this works, but little
of it has run on real servers yet. Where something has only been tried against a
stand-in, it says so.

## Apps

- **From GitHub.** Connecting GitHub creates a GitHub App in your own account, with
  access only to the repositories you pick. Private repositories work. Deploying from a
  real private repository has been tested; deploying on every push needs the panel to be
  reachable from the internet, and waits for the first real install.
- **From an image.** Any public image, such as `nginx:alpine`.
- **Builds.** A repository with a Dockerfile is built with it. Without one, Railpack works
  out the build for Node, Python, Go, static sites and more. Zelie shows the build and
  start commands Railpack chose, and either can be replaced.
- **Variables.** Plain and secret ones, or a whole `.env` file pasted in. A secret is
  sealed as it is saved and only the privileged core can open it. Builds get variables as
  BuildKit secrets, which stay out of the image.
- **Storage.** Volumes that outlive deployments, each with a size limit you can raise.
- **Limits.** Memory and CPU, set against what the server has and what the other apps
  were given.
- **Logs** of every app, live in the browser.

## Deployments

- A new version starts next to the old one. An app with a domain must answer on its
  health check path before traffic moves over. If the build or the start fails, the old
  version keeps serving and the log shows why.
- When a Node project has a test script, it runs on each new build first, with the app's
  variables. A failing test keeps the old version.
- The last five versions that went live keep their image, so a rollback takes a second or
  two. Older images are deleted.
- An app that stops by itself, or a server that restarts, brings the live version back,
  waiting longer after each crash. After five crashes in ten minutes the app stays down
  and the panel says why.

## Domains and HTTPS

Give an app a domain and the proxy routes it and gets a certificate from Let's Encrypt.
So far this has run with self-signed certificates only, not against Let's Encrypt.

## Databases

- PostgreSQL, MariaDB and Redis, each in its own container with its own volume.
- No port opens to the internet. Link a database to an app and the app reaches it by
  name and gets `DATABASE_URL` or `REDIS_URL`, along with the engine's usual variables.
  The password is sealed; nobody reads it in the panel.
- **Data tab.** Tables, rows, filters by column, search, sorting and CSV export, and for
  Redis, keys and their values. Everything is read-only: the core builds each query
  itself and runs it in a read-only transaction with a time limit.
- **From your computer.** Tools like TablePlus or DBeaver can connect through an SSH
  tunnel to a port only the server itself can reach, as a user of their own whose
  password is shown once.

## Backups

- Databases are backed up on a schedule and kept for as many days as you choose. Volumes
  can be backed up too, with the app running or stopped for a consistent copy.
- Backups are encrypted on the server. The key stays with the core, and a recovery file
  lets another server open them.
- Copies can go to any S3-compatible storage, and a new server can find and restore them.
- A dump from elsewhere (`.sql`, `.sql.gz`, a Redis `.rdb`) can be uploaded to restore a
  database. Statements that only fit the old server are left out and counted.
- A restore first backs up what is there. If the restore fails, that backup goes back.

## Logins

- The administrator needs a passkey or an authenticator app as well as a password, with
  recovery codes as a fallback.
- Changing any of these, restoring a backup or exporting data asks for a fresh second
  step.
- The account page lists every browser that is logged in and can log any of them out.

## Housekeeping

- The build cache is kept to a tenth of the disk, at most 20 GB.
- Images nothing has used for a week are deleted.

## Install and updates

- One command installs Zelie and asks how the panel is reached. The release's signature
  is checked first. Running the command again finishes an install that stopped halfway,
  and updates an older one.
- Behind a Cloudflare Tunnel, Zelie opens no port to the internet: it listens on one
  port of the server itself and the tunnel brings the traffic.
- The Server page shows when a new release is out and what changed. Updating restarts
  only Zelie's own services; apps and databases keep running. If the new version does not
  come up within a minute and a half, the old one is put back.
- Getting certificates from Let's Encrypt has not yet run against Let's Encrypt itself.

## Interface

Light and dark themes, usable on a phone, and built into the binary: no separate web
server, no CDN, no tracking. Every sentence it shows can be translated.

## Coming

- Game servers from Pterodactyl and Pelican eggs, with a console, SFTP, a file manager,
  startup settings and schedules.
- Several servers managed from one panel.
