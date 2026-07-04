# Security Policy

## Supported Versions

Only the latest release is supported for security updates. To receive security fixes, keep your deployment on the most recent version.

## Reporting a Vulnerability

Do not open a public GitHub issue for security vulnerabilities. Instead, report privately via GitHub Security Advisories: [Report a vulnerability](https://github.com/smallchungus/DistTaskQueue/security/advisories/new).

Security reports will be acknowledged within 48 hours and are subject to a 90-day coordinated disclosure window before public announcement.

## Security Model

### Data at Rest

- **OAuth tokens**: Encrypted at rest using AES-256-GCM. The `TOKEN_ENCRYPTION_KEY` is a 32-byte secret stored in your deployment platform's secret store (Kubernetes Secret, Fly secret, environment variable, etc., never committed to git). Tokens are decrypted only in memory for API calls and immediately discarded.

- **Email and PDF files**: Downloaded MIME messages, generated PDFs, and extracted attachments are stored unencrypted on the local data volume. You are responsible for securing the underlying storage infrastructure (VM firewall, network segmentation, encrypted block storage).

### Threat Model

DistTaskQueue is designed for **single-operator self-hosting**: you host your own data and control the server. Threat assumptions:

- **Trusted operator**: The person or organization running the cluster has legitimate access to all data flowing through it.
- **Untrusted network**: The cluster may be exposed to the internet via HTTPS (TLS termination at the edge or via cert-manager). The API surface (dashboard, metrics, HTTP endpoints) accepts only authenticated requests via OAuth tokens stored in the database.
- **Shared infrastructure not in scope**: DistTaskQueue does not protect against a co-tenant or malicious cloud provider accessing the hypervisor or block storage. Use encrypted block storage (EBS encryption, GCP Persistent Disk encryption) if you require that assurance.

### What DistTaskQueue Does NOT Provide

- **Multi-tenancy**: There is no isolation between users within a single deployment. The sweeper, scheduler, and workers are cooperative and assume honest clients.
- **Audit logging**: Actions are logged to stderr/stdout; there is no centralized immutable audit trail. Add a sidecar (Stackdriver, DataDog, etc.) if your compliance regime requires it.
- **Encryption in transit for internal communication**: Pod-to-pod and pod-to-database traffic is unencrypted within the Kubernetes cluster. Enable mTLS (Linkerd, Istio) or run on a private network if you require that.
- **Rate limiting or DoS protection**: The API has no built-in rate limiting. Deploy a WAF or API gateway (Traefik, ngrok, Cloudflare) if exposed to untrusted networks.

### Dependency Security

This project uses vendored dependencies and expects you to:

1. Pin container images by digest (not `latest` tag) in your Kubernetes manifests.
2. Regularly rebuild and redeploy to pick up security fixes in transitive Go dependencies.
3. Monitor the project's GitHub releases for security advisories and apply them promptly.

## Incident Response

If you discover a security vulnerability in your running deployment:

1. **Stop the leak**: Disable the affected pod/credential.
2. **Assess scope**: Check logs for unauthorized access or data exfiltration.
3. **Fix the root cause**: Update to the patched version or apply a workaround.
4. **Report upstream**: If the vulnerability is in DistTaskQueue itself, report it via GitHub Security Advisories so other users can protect themselves.
