# Delivery plan

1. Establish the protocol, filesystem format, resource budgets, and toolchain.
2. Implement atomic verified filesystem storage, LRU tiers, and retention.
3. Implement authenticated HTTP, structured logs, metrics, and graceful shutdown.
4. Implement a streaming, concurrent Go cache helper with fail-open semantics.
5. Package the image, Helm chart, CI, development tasks, and operational docs.
6. Verify real HTTP and filesystem invariants, Go build/test reuse, restart
   persistence, outage behavior, and a real Kubernetes deployment.

## Follow-up backlog

- Benchmark representative CI repositories before changing cache policy.
- Add additional storage backends only when a concrete deployment needs them.
- Consider persistent client caching and deduplication after measuring disk cost.
- Tune tracing sampling and dashboards against the deployed observability stack.
