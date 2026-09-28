# Contributing

Thanks for your interest in Zelie. It is pre-alpha, and for now we are not accepting pull
requests. Issues are welcome: a bug, a problem you want solved or a design question.

## Building

You need Go (the version is in `go.mod`), and Node.js 22 or later with pnpm for the web
interface.

```sh
pnpm --dir web install
pnpm --dir web build      # writes the interface into internal/webui/build/ui
go test ./...
go build ./cmd/zelie      # embeds the interface built above
```

A binary built without the interface still runs, and says so when the panel is
opened.

Zelie runs on Linux. On macOS you can build and run the unit tests, but running
containers needs a Linux machine or virtual machine.

The integration tests start real containers, so they need root and are kept behind a
build tag. `hack/vm-test.sh` builds them and runs them inside a
[Lima](https://lima-vm.io) VM named `zelie` (Debian or Ubuntu).

## Code style

- Run `gofmt` and `go vet`. CI checks both.
- Keep dependencies to a minimum. Every new dependency needs a reason in the pull
  request.
- Comments explain why, not what. If the code needs a comment to say what it does,
  try making the code clearer first.
- Tests come with the change they cover.
- Commit messages say what the change does, in plain English.

## Text people read

Everything the interface shows can be translated, so no user-facing sentence is
written inline.

- In the web interface, text comes from `t('area.key')`, with the English in
  `web/src/lib/locales/en.ts`. Plurals use `{n, plural, one {…} other {# …}}`, and the
  browser picks the right form for each language.
- The server sends messages, not sentences. Define each one once with `msg.Define`,
  giving it a code and its English text; `writeError` accepts nothing else. After
  adding or changing one, run `go test ./internal/msg -run TestLocale -update` to
  regenerate `web/src/lib/locales/messages.en.ts`. CI fails when that file is out of
  date.
- Logs, deployment output and anything sent to other services stay in English.

## Contributor License Agreement

When pull requests open, contributors will be asked to sign a Contributor License
Agreement first. It lets us keep offering Zelie under more than one license. You keep the
copyright to your work.

## Security

Please report vulnerabilities privately. See [SECURITY.md](SECURITY.md).
