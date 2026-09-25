# Contributing

Thanks for your interest in Zelie. The project is at an early stage, so the fastest way
to help right now is to open an issue with a problem you want solved or a design
question. Please talk to us in an issue before starting on a large change.

## Building

You need Go (the version is in `go.mod`).

```sh
go test ./...
go build ./cmd/zelie
```

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

## Contributor License Agreement

Before your first pull request can be merged, you will be asked to sign a Contributor
License Agreement. It lets us keep offering Zelie under more than one license. You keep
the copyright to your work.

## Security

Please report vulnerabilities privately. See [SECURITY.md](SECURITY.md).
