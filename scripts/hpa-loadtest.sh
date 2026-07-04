#!/usr/bin/env bash
set -euo pipefail

: "${API_URL:?set API_URL, e.g. http://localhost:8080 after 'kubectl -n disttaskqueue port-forward svc/api 8080:80'}"
NS="${NAMESPACE:-disttaskqueue}"
DURATION="${DURATION_SEC:-300}"
OUT="${OUT:-loadtest.csv}"

echo "elapsed_sec,queue_depth,ready_replicas,running_jobs" > "$OUT"
start=$(date +%s)
fired=0

while :; do
  now=$(date +%s)
  elapsed=$(( now - start ))
  [ "$elapsed" -ge "$DURATION" ] && break

  metrics=$(curl -fsS "$API_URL/metrics")
  depth=$(grep -F 'dtq_queue_depth{stage="test"}' <<< "$metrics" | awk '{print $2}')
  running=$(grep -F 'dtq_job_count{status="running"}' <<< "$metrics" | awk '{print $2}')
  replicas=$(kubectl -n "$NS" get deploy worker-test -o jsonpath='{.status.readyReplicas}')
  echo "$elapsed,${depth:-0},${replicas:-0},${running:-0}" >> "$OUT"

  if [ "$fired" -eq 0 ] && [ "$elapsed" -ge 20 ]; then
    curl -fsS -X POST "$API_URL/api/demo/flood" > /dev/null
    echo "flood fired at ${elapsed}s" >&2
    fired=1
  fi
  sleep 2
done

echo "wrote $OUT ($(( $(wc -l < "$OUT") - 1 )) samples)" >&2
