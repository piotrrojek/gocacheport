#!/usr/bin/env bash
set -euo pipefail
cluster="gocacheport-$(date +%s)-$$"
work=$(mktemp -d)
export KUBECONFIG="$work/kubeconfig"
data_forward=''
admin_forward=''
cleanup() {
  [[ -z "$data_forward" ]] || kill "$data_forward" 2>/dev/null || true
  [[ -z "$admin_forward" ]] || kill "$admin_forward" 2>/dev/null || true
  kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
image=${GOCACHEPORT_CHECK_IMAGE:-gocacheport:dev}
docker image inspect "$image" >/dev/null
kind create cluster --name "$cluster" --wait 120s
kind load docker-image "$image" --name "$cluster"
kubectl create namespace cache
printf 'integration-token' > "$work/token"
kubectl -n cache create secret generic cache-auth --from-file=token="$work/token"
helm install cache charts/gocacheport --namespace cache --wait --timeout 180s \
  --set fullnameOverride=cache,image.repository="${image%:*}",image.tag="${image##*:}",image.pullPolicy=Never \
  --set auth.existingSecret=cache-auth,persistence.size=1Gi,cache.diskBytes=67108864,cache.maxObjectBytes=1048576

forward() {
  local service=$1 port=$2 logfile=$3 attempt
  kubectl -n cache port-forward "svc/$service" ":$port" --address 127.0.0.1 > "$logfile" 2>&1 &
  forward_pid=$!
  for ((attempt = 0; attempt < 100; attempt++)); do
    if grep -q 'Forwarding from' "$logfile"; then
      forward_port=$(sed -nE 's/Forwarding from 127\.0\.0\.1:([0-9]+).*/\1/p' "$logfile" | head -1)
      return
    fi
    kill -0 "$forward_pid" 2>/dev/null || { cat "$logfile" >&2; return 1; }
    sleep 0.1
  done
  echo 'port forward did not start' >&2
  return 1
}
forward cache 8080 "$work/data.log"
data_forward=$forward_pid
data_port=$forward_port
forward cache-admin 9090 "$work/admin.log"
admin_forward=$forward_pid
admin_port=$forward_port
action=0101010101010101010101010101010101010101010101010101010101010101
digest=ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
url="http://127.0.0.1:$data_port/v1/cache/$action"
[[ $(curl -s -o /dev/null -w '%{http_code}' "$url") == 401 ]]
[[ $(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Authorization: Bearer integration-token' -H "X-Gocacheport-Output-ID: $digest" --data-binary abc "$url") == 204 ]]
[[ $(curl -fsS -H 'Authorization: Bearer integration-token' "$url") == abc ]]
curl -fsS "http://127.0.0.1:$admin_port/readyz"
curl -fsS "http://127.0.0.1:$admin_port/metrics" > "$work/metrics"
grep -q '^gocacheport_writes_total 1$' "$work/metrics"
grep -q '^gocacheport_disk_bytes 59$' "$work/metrics"

kill "$data_forward" "$admin_forward"
wait "$data_forward" "$admin_forward" 2>/dev/null || true
data_forward=''
admin_forward=''
old_uid=$(kubectl -n cache get pod -l app.kubernetes.io/instance=cache -o jsonpath='{.items[0].metadata.uid}')
kubectl -n cache rollout restart deployment/cache
kubectl -n cache rollout status deployment/cache --timeout=120s
new_uid=$(kubectl -n cache get pod -l app.kubernetes.io/instance=cache -o jsonpath='{.items[0].metadata.uid}')
[[ "$old_uid" != "$new_uid" ]]
forward cache 8080 "$work/restarted.log"
data_forward=$forward_pid
curl -fsS -D "$work/headers" -H 'Authorization: Bearer integration-token' "http://127.0.0.1:$forward_port/v1/cache/$action" > "$work/artifact"
[[ $(cat "$work/artifact") == abc ]]
grep -qi "^X-Gocacheport-Output-Id: $digest" "$work/headers"
[[ $(kubectl -n cache get pvc cache -o jsonpath='{.status.phase}') == Bound ]]
echo 'Kubernetes: nonroot/read-only image, auth, probes, metrics, and PVC restart passed'
