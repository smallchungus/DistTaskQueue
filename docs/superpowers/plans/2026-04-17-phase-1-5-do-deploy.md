# Phase 1.5 — Deploy to DigitalOcean droplet (today, minimal, temporary)

> **Scope:** prove the system works on a real public cluster, TODAY. Migrate to Hetzner tomorrow once verification clears — manifests are portable. No multi-env CI/CD, no named domain, no auto-deploy yet — that's Phase 1.6.

**What we ship:**

- Single $12/mo DigitalOcean droplet (Ubuntu 24.04, 1 vCPU / 2 GB / 50 GB)
- k3s single-node cluster
- All services in the `disttaskqueue` namespace (from Phase 1's manifests)
- Our `api` image pushed to GHCR and pulled by the Deployment
- `cloudflared` running on the droplet as an ephemeral tunnel → public `*.trycloudflare.com` URL
- Only `/healthz` and `/version` exposed (no dashboard yet — that's Phase 4)

**What we SKIP today (Phase 1.6 polish later):**

- Named domain + permanent Cloudflare Tunnel
- Multi-env (staging + prod namespaces)
- Auto-deploy via CI/CD
- HPA + Prometheus
- PV backups

---

## Section A — User creates droplet (you)

You already did or are about to. Settings:

- Ubuntu 24.04 LTS, $12/mo Basic Regular, NYC1
- SSH key added
- Name: `disttaskqueue-do-prod`

Report back with the public IP.

---

## Section B — Install k3s on the droplet (you, ~5 min)

SSH in:

```bash
ssh root@<DROPLET_IP>
```

Install k3s (curl script, official):

```bash
curl -sfL https://get.k3s.io | sh -
```

Wait 30 s for it to come up, then verify:

```bash
kubectl get nodes
```

Expected: one node in `Ready` state.

Grab the kubeconfig:

```bash
cat /etc/rancher/k3s/k3s.yaml
```

Copy the full YAML output. Paste it back in the chat so I can write it to your local `~/.kube/dtq-do-config`, then we swap the server address from `127.0.0.1` to the droplet IP so you can `kubectl` from your laptop.

---

## Section C — Apply manifests from your laptop (I'll drive, ~3 min)

Once I have the kubeconfig + droplet IP:

```bash
export KUBECONFIG=~/.kube/dtq-do-config
kubectl apply -f deploy/k8s/00-namespace.yaml
kubectl apply -f deploy/k8s/01-config.yaml
```

Create the secret (I'll generate a fresh 32-byte token encryption key and store it in the cluster only — no local file):

```bash
kubectl -n disttaskqueue create secret generic dtq-secrets \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -base64 24)" \
  --from-literal=TOKEN_ENCRYPTION_KEY="$(openssl rand -base64 32)"
```

Apply the rest:

```bash
kubectl apply -f deploy/k8s/10-postgres.yaml
kubectl apply -f deploy/k8s/11-redis.yaml
kubectl apply -f deploy/k8s/20-api.yaml
```

Wait for pods:

```bash
kubectl -n disttaskqueue wait --for=condition=ready pod --all --timeout=180s
kubectl -n disttaskqueue get pods
```

Expected: postgres, redis, api all `Running`.

Verify from the droplet (inside the cluster):

```bash
kubectl -n disttaskqueue port-forward svc/api 8080:80 &
curl -s http://localhost:8080/healthz
curl -s http://localhost:8080/version
```

Expected: `200`, then the JSON `{"version":"phase1.5-do","commit":"..."}`.

---

## Section D — Public URL via ephemeral Cloudflare Tunnel (you, ~3 min)

On the droplet:

```bash
# Install cloudflared
wget https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb
dpkg -i cloudflared-linux-amd64.deb
```

Port-forward the API service so cloudflared can see it locally:

```bash
kubectl -n disttaskqueue port-forward svc/api 8080:80 &
```

Start the ephemeral tunnel (backgrounded in its own terminal or with `nohup`):

```bash
cloudflared tunnel --url http://localhost:8080
```

Cloudflared prints a URL like `https://<random-words>.trycloudflare.com`. That's the public URL. From anywhere:

```bash
curl -s https://<that-url>/healthz
curl -s https://<that-url>/version
```

Expected: `200`, JSON version. Share the URL with me to confirm.

**Caveats:** `trycloudflare.com` URLs are ephemeral — if the cloudflared process dies, the URL changes next restart. That's fine for today's proof-of-ship. Phase 1.6 replaces this with a named tunnel + your domain.

---

## Section E — Workers + scheduler (optional for today)

The API is up and accessible. For Phase 1.5 we can stop here — "shipped" means "reachable on the internet." The workers + scheduler manifests don't exist in `deploy/k8s/` yet; we'll add them in Phase 1.6 alongside the CI/CD auto-deploy story.

If you want the full stack running today, that's a 15-min extension: add `deploy/k8s/30-worker.yaml` (3 deployments, one per stage), `deploy/k8s/31-sweeper.yaml`, `deploy/k8s/32-scheduler.yaml`, `deploy/k8s/12-gotenberg.yaml`. All straightforward. Just say the word.

---

## After — migrating to Hetzner tomorrow

Once Hetzner verification clears:

1. Provision Hetzner CX22 (same steps as Section A but on Hetzner).
2. Install k3s (identical).
3. Swap your local kubeconfig to the Hetzner cluster.
4. Run `kubectl apply -f deploy/k8s/` against the new cluster — all manifests work unchanged.
5. `kubectl -n disttaskqueue create secret` with your existing encryption key (copy it from the DO cluster first).
6. Update DNS / tunnel config.
7. Tear down the DO droplet.

Total migration time: ~15 min. The code and manifests are cloud-agnostic.
