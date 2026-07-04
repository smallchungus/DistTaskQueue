#!/usr/bin/env bash
set -euo pipefail

: "${API_URL:?set API_URL, e.g. http://localhost:8080 after 'kubectl -n disttaskqueue port-forward svc/api 8080:80'}"
NS="${NAMESPACE:-disttaskqueue}"

psql_q() {
  kubectl -n "$NS" exec statefulset/postgres -- psql -U dtq -d dtq -tA -c "$1"
}

say() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }

say "enqueue slow synthetic job (5s sleep)"
curl -fsS -X POST "$API_URL/api/demo/slow" > /dev/null
job=$(psql_q "SELECT id FROM pipeline_jobs WHERE is_synthetic ORDER BY created_at DESC LIMIT 1")
say "job $job queued"

say "waiting for a worker to claim it"
until [ "$(psql_q "SELECT status FROM pipeline_jobs WHERE id = '$job'")" = "running" ]; do
  sleep 0.2
done
victim=$(psql_q "SELECT worker_id FROM pipeline_jobs WHERE id = '$job'")
say "job running on $victim — killing worker pod now"

kubectl -n "$NS" delete pod -l app=worker-test --grace-period=0 --force >/dev/null 2>&1
say "worker pod force-deleted"

last=""
deadline=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  row=$(psql_q "SELECT status || ' worker=' || coalesce(worker_id, '-') || ' attempts=' || attempts || ' err=' || coalesce(last_error, '-') FROM pipeline_jobs WHERE id = '$job'")
  if [ "$row" != "$last" ]; then
    say "$row"
    last="$row"
  fi
  case "$row" in done*) say "job survived the crash"; exit 0 ;; esac
  sleep 0.5
done

say "timed out after 120s — check sweeper logs: kubectl -n $NS logs deploy/sweeper"
exit 1
