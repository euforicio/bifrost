# Orion production deployment

This deployment serves Bifrost from `https://bifrost.riftlabs.cc` on the
Hostinger Orion server at `31.97.143.81`.

## Runtime contract

- GitHub Actions builds the checked-out source with `transports/Dockerfile.local`
  and publishes it to `ghcr.io/<repository-owner>/bifrost`.
- Production deploys an immutable `image@sha256:digest`, never a mutable tag.
- Bifrost binds only to `127.0.0.1:8080`; Cloudflare Tunnel is the public TLS
  boundary.
- `/opt/bifrost/data` persists the single-instance SQLite configuration, logs,
  provider accounts, and encrypted refresh tokens.
- `/opt/bifrost/.env` is created once on the host with mode `0600`. Its stable
  encryption key must be backed up with the data directory.
- Dashboard and inference authentication are enabled before public exposure.

## GitHub configuration

Create the `orion-production` environment and add one environment secret:

- `ORION_SSH_PRIVATE_KEY`: dedicated private deployment key whose public key is
  authorized for `root` with the forced command shown below.

Pushes to `feature/codex-xai-account-auth` build and smoke-test the image without
production access. A manual dispatch with `deploy=true` additionally backs up,
deploys, rolls back on failed health, and verifies the public endpoint.
After the feature is merged, change the workflow push branch to the production
branch.

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

The forced command accepts only an immutable image from
`ghcr.io/euforicio/bifrost` and reads a short-lived GitHub registry token from
standard input. It cannot open an interactive root shell.

## Cloudflare Tunnel

Use a dedicated tunnel named `bifrost-orion`. Its only ingress rule is:

```yaml
ingress:
  - hostname: bifrost.riftlabs.cc
    service: http://127.0.0.1:8080
  - service: http_status:404
```

Run `cloudflared` as a system service on Orion. The DNS record should be the
proxied CNAME created by `cloudflared tunnel route dns bifrost-orion
bifrost.riftlabs.cc`; do not create a public port-forward for Bifrost.

## Operations

```bash
cd /opt/bifrost
BIFROST_IMAGE="$(docker inspect --format '{{.Config.Image}}' bifrost)" \
  docker compose -f compose.yaml ps
docker inspect --format '{{.Config.Image}} {{.State.Health.Status}}' bifrost
curl --fail --silent --show-error https://bifrost.riftlabs.cc/health
```

The last five consistent pre-deploy archives are retained under
`/opt/bifrost/backups`.
The previous image reference is stored in `/opt/bifrost/.previous-image`.
Restore requires both the matching data backup and the unchanged
`BIFROST_ENCRYPTION_KEY`.
