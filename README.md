# gocacheport

Share Go build and test results between trusted machines. One binary runs the
HTTP cache server and the `GOCACHEPROG` helper. The server stores artifacts on
disk and keeps small, frequently used artifacts in memory.

## Quick start

Build on Linux or macOS with Go 1.27.3 or later.

```sh
go build -o bin/gocacheport ./cmd/gocacheport
./bin/gocacheport serve --dir "$PWD/.cache/server" \
  --listen 127.0.0.1:8080 --admin-listen 127.0.0.1:9090
```

In another shell, start in this checkout and set the absolute path to the helper
before changing to the Go project you want to build.

```sh
export GOCACHEPROG="$(pwd)/bin/gocacheport prog --url http://127.0.0.1:8080"
cd /path/to/your-go-project
go build ./...
go test ./...
```

Go starts a helper for each command. If the server is unavailable, the helper
falls back to local builds by default. Use `--strict` to surface remote errors
while diagnosing the connection.

## Run a shared cache

Use one server per cache directory. Put the artifact listener behind HTTPS,
give the server and trusted clients the same bearer token through
`GOCACHEPORT_TOKEN_FILE`, and keep the separate admin listener private.
A client with write access can supply build artifacts to other clients.

The [Helm chart](charts/gocacheport) runs one server with persistent storage.
The [operations guide](docs/operations.md) covers image builds, Helm setup,
storage, and telemetry. See [architecture](docs/architecture.md) for the
cache protocol and local artifact lifetime.

Run `gocacheport serve --help` or `gocacheport prog --help` for flags and
defaults. Flags override their corresponding environment variables, such as
`GOCACHEPORT_MEMORY_BYTES` for `--memory-bytes`.

## Development

Run `mise install` and `mise run check` from the checkout. The checks run Go
vet, ShellCheck, Helm checks, race-enabled integration tests through Dagger,
and a binary build. See [testing](docs/verification.md) for prerequisites and
the disposable Kubernetes check.
