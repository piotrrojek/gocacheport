# Architecture

## Scope and boundaries

One Go binary has two modes: `serve` exposes a remote cache over
HTTP; `prog` speaks the Go command's GOCACHEPROG JSON protocol on stdin/stdout.
There is no object store, database, or Redis. Application code uses the standard
library; official Prometheus and OpenTelemetry libraries provide interoperable
metrics, Go/process collectors, W3C trace propagation, and optional OTLP/HTTP
trace export. No running telemetry backend is required to use the cache.
The initial implementation targets Linux and macOS, using Go 1.27.3.

## Filesystem authority

The server owns one explicitly configured directory, accessed through `os.Root`.
A process-held filesystem lock prevents two servers from owning that directory.
Each action has one sharded file containing a versioned fixed-size header and
the artifact bytes. The header carries the output SHA-256, size, and creation
time. Uploads stream to a same-filesystem temporary file, verify their size and
SHA-256, sync, then atomically rename. Metadata cannot point at an incomplete
artifact. Different actions may contain the same output; deduplication is not
part of version one.

Disk byte and entry limits are enforced before publication. The server tracks
least-recently-used entries in memory, reconstructing the inventory on startup.
TTL is optional and measured from creation, not last access. Evicted open files
remain readable by in-flight requests on the supported Unix platforms. Disk
accounting covers committed record bytes, not filesystem allocation overhead or
in-flight temporary uploads. Operators must budget temporary space for bounded
concurrent uploads and size PVCs accordingly.

## Memory and concurrency

A byte- and entry-bounded LRU stores small immutable artifacts. Larger artifacts
stream from disk. The memory budget charges payload plus a conservative fixed
per-entry allowance; it is not an exact Go heap limit. The disk inventory also
uses memory proportional to the configured entry limit. Set `GOMEMLIMIT` below
the container memory limit with headroom for active requests and the inventory.
HTTP concurrency and client workers are independently bounded. Network bodies
and base64 protocol bodies stream instead of being decoded into giant strings.

## HTTP and trust

`PUT /v1/cache/{action}` accepts artifact bytes and an output SHA-256 header.
`GET /v1/cache/{action}` returns bytes and output metadata. IDs are exactly 32
bytes represented as lowercase hexadecimal. The client verifies every download
before giving its raw local path to Go. Both sides verify writes against the
Go-computed output digest. A digest establishes integrity, not the trust of a
writer: only trusted builds should share a cache.

Bearer authentication is optional, loaded from a mounted secret file or an
environment variable. Health, Prometheus metrics, and optional pprof are exposed
on a separate administrative listener, never on the artifact listener. Helm
defaults to ClusterIP and can restrict traffic with a NetworkPolicy. TLS belongs
at a trusted proxy or ingress; the client uses normal platform certificate
verification and never disables it.

## Cache program behavior

The helper announces `get`, `put`, and `close`, echoes request IDs, and can reply
out of order. Put bodies are base64 JSON string values following the request.
Raw output files live in private retained session directories. Local in-session
action hits avoid HTTP. Close drains all workers and acknowledges the request;
it does not remove returned files. This lets external export-data consumers read
the archives after their `go list` subprocess and cache helper have exited.

Network failures default to misses for reads and local-only success for writes,
with structured diagnostics on stderr. Short connect/TLS and response-header
deadlines precede the full operation timeout. A network or server failure cancels
peer requests and opens an exponential circuit breaker with a single recovery
probe. Admission overload, client errors, and invalid artifacts fail only their
own request. Admission responses carry `X-Gocacheport-Overloaded: true` with
HTTP 503 and `Retry-After: 1`; unmarked 503 responses still open the circuit.
Strict mode bypasses the circuit breaker for operators and checks.
The default local cache is `gocacheport/artifacts` under `os.UserCacheDir`.
`--local-dir` replaces the parent of `artifacts`. Custom parents must be owned,
non-symlink directories without group/world write permissions and have trusted
ancestors. The cache/session directories are 0700 and files are 0600. Lock files
must be private regular files, opened without following symlinks.

A shared filesystem lock serializes inventory, cleanup, staging and atomic
publication. Admission charges the entire incoming payload before copying;
partial writes also fit the budget. SHA-256 and exact length are verified before
rename. Each active session holds its own filesystem lock until helper exit, so
another helper cannot expire it. Clean close/EOF refreshes the session lease;
inactive sessions expire 24 hours later by default. Killed helpers release their
locks automatically and expire based on their last successful staging time.
Cleanup runs at startup and before staging, including after an earlier crash.
Repeated outputs reuse the existing verified digest file within a session rather
than replacing its inode: open consumers must not retain unaccounted copies.

The default shared limits are 4 GiB of payload and 10,000 artifact files plus
session directories. Temporary files count toward both; fixed-size lock files
and filesystem allocation overhead are excluded. Helpers sharing a parent must
use consistent limits. Protected sessions are never evicted to admit a write:
exhaustion fails local staging. Retention is a minimum post-exit grace period,
not an unlimited lifetime promise. Increase it for delayed consumers and budget
disk accordingly. Local staging is serialized across helpers, including a remote
download while its body is copied; remote HTTP requests remain concurrent.
Local staging errors drain the protocol body and fail only that request rather
than desynchronizing the stream; they cannot promise a successful build because
Go requires a usable raw artifact path for puts.
The retained files serve the DiskPath lifetime contract, not cross-session action
lookup or deduplication. The remote disk remains the reusable build cache.

## Kubernetes and observability

One Deployment replica, Recreate strategy, one PVC, non-root UID, read-only root
filesystem, and a writable temporary volume support the single-writer model.
Multiple replicas against the same directory are intentionally unsupported.
Prometheus/OpenMetrics exposition includes request rates, statuses, durations,
bytes, cache tier results, eviction, storage usage, and Go runtime/process
information. The chart supports scrape annotations, ServiceMonitor, and optional
PrometheusRule alerts. OTLP/HTTP traces can go to an OpenTelemetry Collector,
Tempo, or another compatible endpoint. Pod identity is supplied through the
Downward API. Structured access logs use route patterns rather than action IDs
and include trace/span IDs when tracing is active.

## Deliberate non-goals

S3 or other object stores, cross-replica coordination, multi-tenant isolation,
content deduplication, and persistent local action lookup are follow-up decisions,
not hidden dependencies of version one.
