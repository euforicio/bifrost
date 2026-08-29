# Orion production deployment

This deployment serves Bifrost from `https://bifrost.riftlabs.app` on the
Hostinger Orion server at `31.97.143.81`.

## Runtime contract

- GitHub Actions follows the Rift control-plane deployment shape: validate and
  build in a serialized production workflow, pack the host deployment surface,
  copy it into a private Orion staging directory, and run an idempotent host
  installer. Bifrost retains immutable GHCR promotion because its runtime is
  containerized.
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

- `ORION_SSH_KEY`: dedicated Orion SSH private key, matching the secret name and
  staged-copy convention used by the Rift control-plane deployment.

Pushes to `dev` run validation, immutable image build, boot smoke, private
staging copy, host installation, managed Caddy update, and public verification.
The feature branch is included during the initial rollout and should be removed
from the trigger after merge. Dispatch `orion-rollback.yml` to restore the
image and data snapshot from the immediately preceding deployment.

Restrict the `orion-production` environment to the protected production branch,
require an independent reviewer, and disable self-approval before adding its
secret. After the initial rollout, restrict it to the protected production
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
`BIFROST_ADMIN_PASSWORD`, and `BIFROST_SETUP_TOKEN`. The dedicated deployment
key must be installed for `root`, because the installer updates Docker, the
shared Caddy configuration, and host deployment files. The deploy command still
accepts only an immutable `ghcr.io/euforicio/bifrost@sha256:...` image or a
rollback operation. Registry credentials remain ephemeral and are removed with
the private staging directory.

## Caddy and Cloudflare DNS

The installer renders the marked `Caddyfile.bifrost` block into Orion's shared
Caddyfile, preserves unrelated sites, rejects ambiguous host blocks, validates
the complete candidate, and only then reloads Caddy:

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
