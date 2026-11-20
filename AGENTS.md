# gocacheport

- Read `docs/architecture.md` and `docs/plan.md` before changing behavior.
- Go 1.27.3 and project tools are pinned in `mise.toml`. Use `mise run` tasks.
- Use the standard library, `net/http.ServeMux`, and `log/slog` for application code.
- Prometheus and OpenTelemetry official libraries are the observability exception.
- Preserve the Go cache program wire protocol. Stdout is exclusively protocol JSON.
- Remote cache failures must not fail builds unless strict mode is selected.
- Local staging failures may fail the affected build: Go requires a usable DiskPath.
- Keep server writes atomic, verify output digests, and bound memory and disk usage.
- Prefer integration tests with independent fixtures and the real Go toolchain.
- Do not add unit tests without a substantive invariant that integration cannot cover.
- Do not introduce external services solely for tests. Use testcontainers if a real
  service dependency becomes necessary.
- Run `mise run check` before delivery. Never bypass hooks or push protections.
