#!/usr/bin/env bash
set -euo pipefail
chart=charts/gocacheport
helm lint "$chart" --strict
helm template check "$chart" >/dev/null
helm template check "$chart" --set persistence.enabled=false,cache.memoryBytes=0,startupProbe.failureThreshold=120 >/dev/null
helm template check "$chart" --set persistence.existingClaim=cache,auth.existingSecret=auth,tracing.enabled=true,tracing.existingSecret=otel,networkPolicy.enabled=true,metrics.serviceMonitor.enabled=true,metrics.prometheusRule.enabled=true >/dev/null
if helm template check "$chart" --set replicaCount=2 >/dev/null 2>&1; then
  echo 'chart accepted multiple cache owners' >&2
  exit 1
fi
if helm template check "$chart" --set cache.diskBytes=1024 >/dev/null 2>&1; then
  echo 'chart accepted an artifact larger than its disk budget' >&2
  exit 1
fi
if helm template check "$chart" --set startupProbe.failureThreshold=0 >/dev/null 2>&1; then
  echo 'chart accepted an invalid Kubernetes startup probe' >&2
  exit 1
fi
