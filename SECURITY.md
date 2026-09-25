# Security

Zelie runs with root privileges and hosts other people's code, so we take security
reports seriously and treat them before anything else.

## Reporting a vulnerability

Please don't open a public issue. Report it privately through GitHub instead: go to the
**Security** tab of this repository and choose **Report a vulnerability**. If you cannot
use GitHub, write to support@cariacore.com with "Security" in the subject.

Include what you found, how to reproduce it, and which version you tested. We will
confirm that we received your report within three working days and keep you updated
until it is fixed. Once a fix is released, we are happy to credit you.

## Supported versions

Zelie has not had a release yet. Once it does, security fixes will go to the latest
release.

## How Zelie is designed to stay safe

These are commitments of the design. Zelie is still being built, and each one will be
covered by tests before the first release.

- The web panel runs as an unprivileged user. Only a small core process runs as root,
  and it accepts a narrow set of commands over a local socket.
- Every app and game server runs in its own container with user namespaces, a seccomp
  profile, dropped capabilities and resource limits. Nothing runs privileged.
- Databases are never exposed to the internet.
- Secret variables are sealed with a key only the core holds, so the panel's database
  and its backups do not reveal them, and they are never written to logs.
- Builds of your code run inside a confined container, never directly on the host.
- The administrator account requires a passkey or a one-time code.
- Releases are signed, and the installer and updater refuse anything with a bad
  signature.

[docs/architecture.md](docs/architecture.md) has more detail.
