# Chaos Demo Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A one-command demo that kills a worker pod mid-job on the k3s cluster and prints the timestamped recovery timeline (running → revived by sweeper → re-run → done), producing the evidence artifact for the blog post.

**Architecture:** Pure script + docs — no Go changes. The script fires the existing `/api/demo/slow` trigger (5 s synthetic job on stage `test`), waits for a worker to claim it, force-deletes the `worker-test` pod, then polls the job row via `kubectl exec` psql until it completes, printing every state change with a timestamp. Works with the current BRPOP code and with the BLMOVE plan applied — recovery routes through the sweeper either way.

**Tech Stack:** bash, kubectl, curl, psql (inside the postgres pod).

## Global Constraints

- No hardcoded URLs: `API_URL` is required, `NAMESPACE` defaults to `disttaskqueue` (overridable).
- Cluster facts used: postgres is StatefulSet `postgres` (user `dtq`, db `dtq`), worker pods carry label `app=worker-test`, demo endpoints live on the `api` Service (port 80).
- Timing expectations: worker heartbeat TTL 15 s (`WORKER_HEARTBEAT_TTL_SEC`), sweeper interval 5 s — revival lands ~15–25 s after the kill.
- Don't claim the demo works without running it: the captured output in Task 3 is the completion evidence.
- Commit subjects: Conventional Commits (`feat(k8s)` / `docs(ops)`).

---

### Task 1: `scripts/chaos-demo.sh`

**Files:**
- Create: `scripts/chaos-demo.sh` (mode 755)

**Interfaces:**
- Consumes: `POST $API_URL/api/demo/slow` (enqueues one synthetic 5 s job), `pipeline_jobs` columns `id, status, worker_id, attempts, last_error, is_synthetic`.
- Produces: timestamped timeline on stdout; exit 0 once the job reaches `done`, exit 1 on the 120 s safety timeout.

- [ ] **Step 1: Write the script**

```bash
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
```

- [ ] **Step 2: Syntax check**

Run: `chmod +x scripts/chaos-demo.sh && bash -n scripts/chaos-demo.sh && { command -v shellcheck >/dev/null && shellcheck scripts/chaos-demo.sh || echo "shellcheck not installed, skipped"; }`
Expected: no output from `bash -n`; shellcheck clean or skipped.

- [ ] **Step 3: Commit**

```bash
git add scripts/chaos-demo.sh
git commit -m "feat(k8s): add chaos demo script that kills a worker mid-job"
```

---

### Task 2: OPERATIONS.md runbook section

**Files:**
- Modify: `docs/OPERATIONS.md` (append after `## 9. Runbook — metrics and dashboards`, renumbering is NOT needed — add as `## 12` at the end to keep existing anchors stable)

- [ ] **Step 1: Append the section**

```markdown
## 12. Runbook — chaos demo (kill a worker mid-job)

Proves the crash-recovery story end to end: a worker dies while processing,
the sweeper notices the expired heartbeat, the job retries on a fresh pod.

Prerequisites: kubeconfig pointing at the cluster, the stack deployed, and
the API reachable:

    kubectl -n disttaskqueue port-forward svc/api 8080:80 &
    export API_URL=http://localhost:8080

Run:

    ./scripts/chaos-demo.sh

What you should see (timings depend on WORKER_HEARTBEAT_TTL_SEC=15 and the
5 s sweep interval):

1. Job enqueued and claimed within a second or two.
2. Pod force-deleted while the job sleeps.
3. ~15–25 s later the sweeper marks it `queued` with `err=worker died`,
   `attempts=1`.
4. The replacement pod (Deployment recreates it in seconds) claims and
   finishes it: `done`.

A captured run lives in `docs/chaos-demo-sample.txt`. If the demo stalls,
check `kubectl -n disttaskqueue logs deploy/sweeper`.
```

- [ ] **Step 2: Commit**

```bash
git add docs/OPERATIONS.md
git commit -m "docs(ops): chaos demo runbook"
```

---

### Task 3: Capture a real run (requires cluster access)

**Files:**
- Create: `docs/chaos-demo-sample.txt`

- [ ] **Step 1: Run against the cluster**

Run:

```bash
kubectl -n disttaskqueue port-forward svc/api 8080:80 &
API_URL=http://localhost:8080 ./scripts/chaos-demo.sh | tee docs/chaos-demo-sample.txt
```

Expected: timeline ending in `done … job survived the crash`, exit 0. If the job finishes before the kill lands (5 s window missed), just re-run — the script is idempotent per invocation.

- [ ] **Step 2: Sanity-check the artifact**

The sample must show all four phases (queued → running on a worker → queued with `err=worker died` → done). If a phase is missing, the demo didn't demonstrate recovery — re-run rather than committing a weak artifact.

- [ ] **Step 3: Commit**

```bash
git add docs/chaos-demo-sample.txt
git commit -m "docs(ops): captured chaos demo run"
```
