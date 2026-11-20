# Testing

Install the pinned tools with `mise install`. Run `mise run check` for Go
vet, ShellCheck, Helm lint and rendering, the race-enabled integration suite,
and a binary build.

Dagger runs the integration suite through the Go module pinned in
`dagger.lock`. Use a container runtime for the local engine, or set
`DAGGER_ENGINE` to an engine you can access. The Go module can fetch
`otelgotest` from upstream `main` if the test image lacks it. Dagger can replay
a successful test step from its cache. Go's `-count=1` applies when that step
executes.

The suite uses the Go toolchain to check remote build and test cache reuse
and reads export archives after the helper exits. HTTP and filesystem tests
cover the wire protocol, digest verification, storage limits, restarts,
concurrency, and fallback during outages. The trace test checks OTLP/HTTP
payloads against an independent receiver.

Run `mise run test` for a native race-enabled run when investigating
host-specific behavior.

## Kubernetes smoke test

With Docker running, build the local image with `mise run image`, then run
`mise run kubernetes`. The script creates a disposable kind cluster with its
own kubeconfig. It checks authentication, artifact reads and writes, probes,
metrics, and whether an artifact survives a pod restart with its original PVC.

For a CI job that runs as a sibling container on the same Docker engine, set
`GOCACHEPORT_KIND_CONTAINER` to the job container's name. The check uses the
kind network and retains Kubernetes certificate verification.
