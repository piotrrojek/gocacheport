# Operations

## Kubernetes

Build a container image and push it to a registry your cluster can pull from.
Replace the example repository with your own.

```sh
image=registry.example.com/build/gocacheport
tag=0.1.1
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg VERSION="$tag" --build-arg COMMIT="$(git rev-parse HEAD)" \
  -t "$image:$tag" --push .
```

Create the namespace and provision a Secret named `gocacheport-auth` with a
`token` key. Install the chart with the same image and tag.

```sh
helm upgrade --install gocacheport ./charts/gocacheport \
  --namespace build-cache \
  --set fullnameOverride=gocacheport \
  --set image.repository="$image" --set-string image.tag="$tag" \
  --set auth.existingSecret=gocacheport-auth
```

Use `imagePullSecrets` if the registry needs credentials. The artifact service
is `gocacheport.build-cache.svc:8080`. Use HTTPS for traffic over untrusted
networks and pass the token to clients through `GOCACHEPORT_TOKEN_FILE`.
The admin service is `gocacheport-admin.build-cache.svc:9090` and has no
authentication. Keep it private, especially if you enable pprof.

The chart runs one replica with the `Recreate` strategy and a 20 GiB PVC that
survives uninstall. Use `persistence.existingClaim` to supply a volume or
`persistence.enabled=false` for a disposable cache. Use a filesystem with
working Unix locks and atomic rename. To run multiple independent caches, give
each server its own storage and route.

With `networkPolicy.enabled=true`, the chart admits artifact clients from the
same namespace and admin clients from the `monitoring` namespace by default.
Set `clientsFrom` and `monitoringFrom` to match your deployment. The CNI must
enforce NetworkPolicy.

## Sizing and recovery

Set `cache.diskBytes` and `cache.maxEntries` to bound committed storage and
startup inventory. Leave disk space for concurrent uploads, filesystem
overhead, and evicted files still held open by readers. With the defaults,
32 simultaneous 128 MiB uploads can use 4 GiB of temporary space.

`cache.memoryBytes` budgets the payload and 256 bytes per entry in the memory
tier. Allow extra memory for the disk inventory, active requests, and telemetry.
The chart sets `GOMEMLIMIT=384MiB` below a 512 MiB container limit.

The server scans its inventory before accepting connections. The chart allows
five minutes for startup. If a healthy large inventory takes longer, adjust
`startupProbe.periodSeconds` or `startupProbe.failureThreshold`. The admin
listener serves `/livez` for process health and `/readyz` for storage readiness.
Readiness returns 503 during shutdown. Leave enough termination grace time for
`cache.shutdownTimeout` and the five-second trace exporter flush.

A cold disk hit verifies SHA-256 before serving an artifact. The server removes
corrupt records when it detects them and discards abandoned uploads on restart.
A crash or power loss can lose recent entries because directory renames are
not synced. The next build can regenerate them.

## Helper storage

The helper stages raw files under `gocacheport/artifacts` in the user cache
directory. Use `--local-dir` to pick a different parent that you own, with
trusted ancestors and without symlinks or group/world write access. Helpers
sharing the same parent should use the same limits.

The default budget is 4 GiB of payload and 10,000 entries. Active sessions stay
protected. Closed sessions remain readable for at least 24 hours by default
so tools can read export archives after Go exits. Cleanup runs at startup and
before staging. Increase `--local-retention` if readers need more time, and
size the budget for concurrent builds and retained sessions. The helper can
fail a build if it cannot stage a file within these limits.

## Metrics and traces

Scrape `/metrics` on the admin listener. Use either scrape annotations or
enable `metrics.serviceMonitor.enabled` with the Prometheus Operator CRDs.
The optional rules under `metrics.prometheusRule` expect the `namespace` and
`service` labels. When cache errors rise, check request saturation, disk
usage, misses and evictions alongside the storage logs.

Set `tracing.enabled=true` and `tracing.endpoint` to the OTLP/HTTP base URL
of your collector. The SDK appends `/v1/traces` to a base URL.
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` accepts an exact traces URL. Use
`tracing.existingSecret` for exporter authentication settings. To correlate a
request with a span, enable debug logging and compare the trace and span IDs.
