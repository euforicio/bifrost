# Orion production deployment

This deployment serves Bifrost from `https://bifrost.riftlabs.app` on the
Hostinger Orion server at `31.97.143.81`.

## Runtime contract

- GitHub Actions builds the checked-out source with `transports/Dockerfile.local`
  and publishes an immutable commit tag to `ghcr.io/euforicio/bifrost`.
- Production deploys an immutable `image@sha256:digest`, never a mutable tag.
- Bifrost binds only to `127.0.0.1:8180`; the existing Orion Caddy service is
  the public TLS and reverse-proxy boundary.
- `/opt/bifrost/data` persists the single-instance SQLite configuration, logs,
  provider accounts, and encrypted refresh tokens.
- `/opt/bifrost/.env` is created once on the host with mode `0600`. Its stable
  encryption key must be backed up with the data directory.
- Dashboard and inference authentication are enabled before public exposure.

## GitHub configuration

Create the `orion-production` environment and add one environment secret:

- `ORION_SSH_PRIVATE_KEY`: dedicated private deployment key whose public key is
  authorized for `root` with the forced command shown below.

Pushes to `feature/codex-xai-account-auth` run `orion-image.yml`, which builds
and smoke-tests without production access. After merging these workflows to
the default branch, dispatch `orion-deploy.yml` with the validated digest to
promote exactly that image. Dispatch `operation=rollback` to restore the image
and data snapshot from the immediately preceding deployment.

Restrict the `orion-production` environment to the protected production branch,
require an independent reviewer, and disable self-approval before adding its
secret. The deployment workflow is intentionally unavailable until it exists
on the default branch.

Provision the checked-in contract once as root:

```bash
install -d -m 0700 /opt/bifrost
install -d -m 0770 -o 1000 -g 0 /opt/bifrost/data /opt/bifrost/backups
install -m 0644 compose.yaml config.json /opt/bifrost/
install -m 0755 bifrost-deploy /usr/local/sbin/bifrost-deploy
```

Create `/opt/bifrost/.env` with mode `0600` and stable values for
`BIFROST_ENCRYPTION_KEY`, `BIFROST_ADMIN_USERNAME`,
`BIFROST_ADMIN_PASSWORD`, and `BIFROST_SETUP_TOKEN`. The deployment key entry
in `/root/.ssh/authorized_keys` must be restricted:

```text
restrict,command="/usr/local/sbin/bifrost-deploy" ssh-ed25519 <deployment-public-key>
```

The forced command accepts only `deploy` with an immutable image from
`ghcr.io/euforicio/bifrost`, or `rollback` with the workflow actor. It reads a short-lived GitHub
registry token from standard input into a temporary Docker configuration and
cannot open an interactive root shell.

## Caddy and Cloudflare DNS

Install `Caddyfile.bifrost` as a site fragment in Orion's existing Caddy
configuration. Validate the complete configuration before reloading Caddy:

```bash
caddy validate --config /etc/caddy/Caddyfile
systemctl reload caddy
```

Create a Cloudflare DNS `A` record for `bifrost.riftlabs.app` pointing to
`31.97.143.81`. Keep the record DNS-only so Caddy terminates public TLS
directly. Ports 80 and 443 must reach Caddy; port 8180 remains loopback-only.

## Operations

```bash
cd /opt/bifrost
BIFROST_IMAGE="$(docker inspect --format '{{.Config.Image}}' bifrost)" \
  docker compose -f compose.yaml ps
docker inspect --format '{{.Config.Image}} {{.State.Health.Status}}' bifrost
curl --fail --silent --show-error https://bifrost.riftlabs.app/health
```

The last five consistent pre-deploy archives are retained under
`/opt/bifrost/backups`.
The previous image and matching backup are stored together in
`/opt/bifrost/.previous-deployment`. Restore requires that local image, the
matching data backup, and the unchanged `BIFROST_ENCRYPTION_KEY`.
