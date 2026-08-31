# Orion production deployment

This deployment serves Bifrost from `https://bifrost.riftlabs.app` on the
Hostinger Orion server at `31.97.143.81`.

## Runtime contract

- GitHub Actions validates and builds Bifrost, publishes an immutable GHCR
  image, and commits its digest to `euforicio/orion-infra`.
- Flux reconciles `orion-infra` onto K3s. Routine deployments do not SSH to
  Orion or mutate Caddy directly.
- Production deploys an immutable `image@sha256:digest`, never a mutable tag.
- The Bifrost Service is cluster-internal; the K3s Caddy workload is the public
  TLS and reverse-proxy boundary.
- `/opt/bifrost/data` persists the single-instance SQLite configuration, logs,
  provider accounts, and encrypted refresh tokens.
- `/opt/bifrost/.env` is created once on the host with mode `0600`. Its stable
  encryption key must be backed up with the data directory.
- Dashboard and inference authentication are enabled before public exposure.

## GitHub configuration

Create the `orion-production` environment and add one environment secret:

- `ORION_INFRA_TOKEN`: fine-grained token with Contents read/write access only
  to `euforicio/orion-infra`.

Pushes to `main` run validation, immutable image build, boot smoke, and GitOps
promotion. Dispatch `orion-rollback.yml` with a previously deployed immutable
image reference to roll back through the same audited Git path.

Restrict the `orion-production` environment to the protected production branch,
require an independent reviewer, and disable self-approval before adding its
secret. After the initial rollout, restrict it to the protected production
branch.

The remaining Compose and host-install files are retained only as break-glass
rollback material during the K3s migration. They are not the deployment path.

The former host deployment contract was provisioned as root with:

```bash
install -d -m 0700 /opt/bifrost
install -d -m 0770 -o 1000 -g 0 /opt/bifrost/data /opt/bifrost/backups
install -m 0644 compose.yaml config.json /opt/bifrost/
install -m 0755 bifrost-deploy /usr/local/sbin/bifrost-deploy
```

The K3s secret must retain the stable values formerly stored in
`/opt/bifrost/.env`, including `BIFROST_ENCRYPTION_KEY`,
`BIFROST_ADMIN_USERNAME`, `BIFROST_ADMIN_PASSWORD`, and
`BIFROST_SETUP_TOKEN`. Never commit those values.

## Caddy and Cloudflare DNS

The K3s Caddy workload owns the complete GitOps-managed Caddyfile. Validate it
before reconciliation and keep Cloudflare DNS pointed at `31.97.143.81`.

Create a Cloudflare DNS `A` record for `bifrost.riftlabs.app` pointing to
`31.97.143.81`. Keep the record DNS-only so Caddy terminates public TLS
directly. Ports 80 and 443 must reach Caddy; port 8180 remains loopback-only.

## Operations

Use `kubectl -n bifrost get pods,svc`, Flux reconciliation status, and the
public health endpoint for operational checks. The exact production commands,
backup schedule, and data-restore procedure live in `euforicio/orion-infra`.
