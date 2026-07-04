# Queue-Depth Autoscaling + Loadtest Charts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the spec's "HPA on queue depth" real via KEDA ScaledObjects, then capture a flood run as CSV + chart (queue depth and replica count over time) for the blog post.

**Architecture:** KEDA's redis scaler reads `LLEN queue:<stage>` directly and drives an HPA per worker Deployment — no Prometheus stack needed (the existing `/metrics` endpoint stays as the sampler's data source). A bash sampler polls `/metrics` and `kubectl get deploy` into a CSV while firing the existing `/api/demo/flood` (1000 synthetic jobs); a small matplotlib script renders two stacked panels sharing the x-axis (queue depth on top, ready replicas below — not a dual-axis chart).

**Tech Stack:** KEDA v2 (helm), kubeconform + datree CRD catalog, bash, curl, kubectl, python3 + matplotlib.

## Global Constraints

- Spec §3 lists fetch/render/upload worker pools as "1-5 (HPA on queue depth)". KEDA is the mechanism, not a deviation; the demoable scaler is `worker-test` (only synthetic load fills a queue on demand).
- No hardcoded URLs in scripts: `API_URL` required, `NAMESPACE` defaults to `disttaskqueue`.
- Redis facts: service `redis:6379`, db 0, queues named `queue:<stage>`.
- Worker Deployment names: `worker-test`, `worker-fetch`, `worker-render`, `worker-upload`.
- Chart rules (dataviz): never dual-axis; one series per panel; recessive grid; values readable without a legend.
- Cluster steps (Tasks 1 and 5) need kubeconfig + helm; everything else runs locally.
- Commit subjects: Conventional Commits (`feat(k8s)`, `build`, `docs(ops)`).

---

### Task 1: Install KEDA on the cluster + runbook (requires cluster access)

**Files:**
- Modify: `docs/OPERATIONS.md` (append as `## 13`)

- [ ] **Step 1: Install KEDA**

Run:

```bash
helm repo add kedacore https://kedacore.github.io/charts
helm repo update
helm install keda kedacore/keda --namespace keda --create-namespace
kubectl get pods -n keda
```

Expected: `keda-operator`, `keda-operator-metrics-apiserver`, and `keda-admission-webhooks` pods `Running` within a minute.

- [ ] **Step 2: Append the runbook section**

```markdown
## 13. Runbook — queue-depth autoscaling (KEDA)

Worker Deployments scale 1–5 replicas on Redis queue depth via KEDA
ScaledObjects (`deploy/k8s/29-keda-scaledobjects.yaml`). KEDA's redis
scaler runs `LLEN queue:<stage>` every 5 s and drives a standard HPA;
target is 25 queued jobs per replica.

One-time install:

    helm repo add kedacore https://kedacore.github.io/charts
    helm repo update
    helm install keda kedacore/keda --namespace keda --create-namespace

Then `kubectl apply -f deploy/k8s/29-keda-scaledobjects.yaml`.

Inspect scaling state:

    kubectl -n disttaskqueue get scaledobjects
    kubectl -n disttaskqueue get hpa
    kubectl -n disttaskqueue describe scaledobject worker-test

To load-test the scaler and produce the chart, see §14.
```

- [ ] **Step 3: Commit**

```bash
git add docs/OPERATIONS.md
git commit -m "docs(ops): KEDA install and autoscaling runbook"
```

---

### Task 2: ScaledObject manifests + validation

**Files:**
- Create: `deploy/k8s/29-keda-scaledobjects.yaml`
- Modify: `Makefile:38-39` (k8s-validate)

**Interfaces:**
- Produces: ScaledObjects named after their Deployments (`worker-test`, `worker-fetch`, `worker-render`, `worker-upload`), min 1 / max 5, redis-list trigger.

- [ ] **Step 1: Write the manifests**

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: worker-test
  namespace: disttaskqueue
spec:
  scaleTargetRef:
    name: worker-test
  minReplicaCount: 1
  maxReplicaCount: 5
  pollingInterval: 5
  cooldownPeriod: 60
  triggers:
    - type: redis
      metadata:
        address: redis:6379
        listName: queue:test
        listLength: "25"
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: worker-fetch
  namespace: disttaskqueue
spec:
  scaleTargetRef:
    name: worker-fetch
  minReplicaCount: 1
  maxReplicaCount: 5
  pollingInterval: 5
  cooldownPeriod: 60
  triggers:
    - type: redis
      metadata:
        address: redis:6379
        listName: queue:fetch
        listLength: "25"
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: worker-render
  namespace: disttaskqueue
spec:
  scaleTargetRef:
    name: worker-render
  minReplicaCount: 1
  maxReplicaCount: 5
  pollingInterval: 5
  cooldownPeriod: 60
  triggers:
    - type: redis
      metadata:
        address: redis:6379
        listName: queue:render
        listLength: "25"
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: worker-upload
  namespace: disttaskqueue
spec:
  scaleTargetRef:
    name: worker-upload
  minReplicaCount: 1
  maxReplicaCount: 5
  pollingInterval: 5
  cooldownPeriod: 60
  triggers:
    - type: redis
      metadata:
        address: redis:6379
        listName: queue:upload
        listLength: "25"
```

- [ ] **Step 2: Teach kubeconform the KEDA CRD**

Replace the `k8s-validate` target in `Makefile`:

```makefile
k8s-validate: ## Validate k8s manifests (requires kubeconform; fetches CRD schemas)
	kubeconform -summary -strict \
	  -schema-location default \
	  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
	  deploy/k8s/
```

- [ ] **Step 3: Validate**

Run: `make k8s-validate`
Expected: `Valid: N, Invalid: 0, Errors: 0` with the four ScaledObjects counted.

- [ ] **Step 4: Commit**

```bash
git add deploy/k8s/29-keda-scaledobjects.yaml Makefile
git commit -m "feat(k8s): KEDA ScaledObjects scale workers on queue depth"
```

---

### Task 3: `scripts/hpa-loadtest.sh` sampler

**Files:**
- Create: `scripts/hpa-loadtest.sh` (mode 755)

**Interfaces:**
- Consumes: `GET $API_URL/metrics` (`dtq_queue_depth{stage="test"}`, `dtq_job_count{status="running"}`), `POST $API_URL/api/demo/flood` (1000 jobs, one call per 60 s allowed), `kubectl get deploy worker-test`.
- Produces: CSV `elapsed_sec,queue_depth,ready_replicas,running_jobs` at `$OUT` (default `loadtest.csv`), one row per ~2 s; flood fired once at t≈20 s.

- [ ] **Step 1: Write the script**

```bash
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
```

- [ ] **Step 2: Syntax check**

Run: `chmod +x scripts/hpa-loadtest.sh && bash -n scripts/hpa-loadtest.sh && { command -v shellcheck >/dev/null && shellcheck scripts/hpa-loadtest.sh || echo "shellcheck not installed, skipped"; }`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add scripts/hpa-loadtest.sh
git commit -m "feat(k8s): loadtest sampler records queue depth vs replicas"
```

---

### Task 4: `scripts/plot-loadtest.py`

**Files:**
- Create: `scripts/plot-loadtest.py`

**Interfaces:**
- Consumes: the Task 3 CSV (`elapsed_sec,queue_depth,ready_replicas,running_jobs`).
- Produces: PNG with two stacked panels sharing the x-axis: queue depth (line, top) and ready replicas (step, bottom). Usage: `python3 scripts/plot-loadtest.py loadtest.csv loadtest.png`.

- [ ] **Step 1: Write the script**

```python
#!/usr/bin/env python3
import csv
import sys

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt

src = sys.argv[1] if len(sys.argv) > 1 else "loadtest.csv"
dst = sys.argv[2] if len(sys.argv) > 2 else "loadtest.png"

t, depth, replicas = [], [], []
with open(src) as f:
    for row in csv.DictReader(f):
        t.append(int(row["elapsed_sec"]))
        depth.append(float(row["queue_depth"]))
        replicas.append(int(row["ready_replicas"]))

fig, (ax1, ax2) = plt.subplots(
    2, 1, sharex=True, figsize=(9, 5.5), height_ratios=[3, 1]
)
ax1.plot(t, depth, color="#2A7DE1", linewidth=1.8)
ax1.set_ylabel("queue:test depth")
ax1.grid(True, alpha=0.25, linewidth=0.5)
ax2.step(t, replicas, where="post", color="#C4532E", linewidth=1.8)
ax2.set_ylabel("ready replicas")
ax2.set_xlabel("seconds since start")
ax2.set_yticks(range(0, 6))
ax2.grid(True, alpha=0.25, linewidth=0.5)
fig.suptitle("Flood: 1000 synthetic jobs vs KEDA scale-out", fontsize=11)
fig.tight_layout()
fig.savefig(dst, dpi=150)
print(f"wrote {dst}")
```

- [ ] **Step 2: Verify with synthetic data**

Run:

```bash
python3 -c "import matplotlib" 2>/dev/null || pip3 install matplotlib
printf 'elapsed_sec,queue_depth,ready_replicas,running_jobs\n0,0,1,0\n20,1000,1,1\n40,700,5,5\n60,300,5,5\n80,0,5,2\n150,0,1,0\n' > /tmp/fake.csv
python3 scripts/plot-loadtest.py /tmp/fake.csv /tmp/fake.png && test -s /tmp/fake.png && echo OK
```

Expected: `wrote /tmp/fake.png` then `OK`. Open the PNG and eyeball it: depth spike at 20 s, replica step 1→5, no label collisions.

- [ ] **Step 3: Commit**

```bash
git add scripts/plot-loadtest.py
git commit -m "feat(k8s): plot loadtest CSV as depth and replica panels"
```

---

### Task 5: Capture a real run (requires cluster access)

**Files:**
- Create: `docs/loadtest/flood-run.csv`, `docs/loadtest/flood-run.png`
- Modify: `docs/OPERATIONS.md` (append as `## 14`)

- [ ] **Step 1: Apply and run**

```bash
kubectl apply -f deploy/k8s/29-keda-scaledobjects.yaml
kubectl -n disttaskqueue port-forward svc/api 8080:80 &
API_URL=http://localhost:8080 OUT=docs/loadtest/flood-run.csv ./scripts/hpa-loadtest.sh
python3 scripts/plot-loadtest.py docs/loadtest/flood-run.csv docs/loadtest/flood-run.png
```

Expected: `kubectl -n disttaskqueue get hpa` shows KEDA-managed HPAs; during the run `worker-test` climbs toward 5 replicas after the flood and settles back to 1 after the cooldown. Paste `kubectl get pods` output showing scaled replicas as evidence — per project rules, don't claim k3s behavior without it.

- [ ] **Step 2: Sanity-check the chart**

The PNG must show: flat baseline (~20 s), depth spike to ~1000, replicas stepping 1→5 within ~10–15 s of the spike, depth draining faster post-scale, replicas returning to 1 after the 60 s cooldown. If replicas never move, `kubectl -n disttaskqueue describe scaledobject worker-test` and fix before committing artifacts.

- [ ] **Step 3: Append the runbook section**

```markdown
## 14. Runbook — flood loadtest with scaling chart

Fires /api/demo/flood (1000 synthetic jobs) while sampling queue depth and
worker-test replicas every 2 s, then renders the chart.

    kubectl -n disttaskqueue port-forward svc/api 8080:80 &
    export API_URL=http://localhost:8080
    OUT=docs/loadtest/flood-run.csv ./scripts/hpa-loadtest.sh
    python3 scripts/plot-loadtest.py docs/loadtest/flood-run.csv docs/loadtest/flood-run.png

Knobs: DURATION_SEC (default 300), NAMESPACE (default disttaskqueue).
A captured run lives in docs/loadtest/. Requires the KEDA ScaledObjects
from §13 to be applied; without them the depth drains on one replica and
the replica panel stays flat.
```

- [ ] **Step 4: Commit**

```bash
git add docs/loadtest docs/OPERATIONS.md
git commit -m "docs(ops): captured flood loadtest run with scaling chart"
```
