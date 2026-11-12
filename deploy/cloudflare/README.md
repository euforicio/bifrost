# Cloudflare runtime

Bifrost runs as one Cloudflare Container. The existing configuration and log
schemas, GORM migrations and encryption remain intact. Config-store and
log-store paths `cloudflare://config` and `cloudflare://logs` select private
Durable Object SQL storage. Container files are disposable caches; R2 stores
log payloads and backups rather than active database or journal files.

Build with `make build-cloudflare` after initializing the local Go workspace.
The `cloudflare` build tag enables the adapter. The linker flag in that target
gives the embedded SQLite driver a separate registration name while existing
non-Cloudflare SQLite builds retain their original driver.

Deployment secrets provide `BIFROST_CLOUDFLARE_CONFIG_SQL_URL`,
`BIFROST_CLOUDFLARE_LOGS_SQL_URL`, `SQLITE_STORAGE_TOKEN` and a per-process
`SQLITE_STORAGE_GENERATION_CAPABILITY`. The controller activates a monotonic
generation before each new process. Stale writers cannot commit. Uncertain
storage acknowledgements terminate the process; its replacement reloads durable
state. SQLite uses one connection and atomic page commits. Persistent ATTACH,
WAL and weakened synchronization are rejected.

Preserve the exact existing `BIFROST_ENCRYPTION_KEY`, admin credentials and
setup token. `framework/cmd/migratepostgressqlite` converts one read-only
PostgreSQL snapshot into two new SQLite files, preserving provider keys without
re-encryption. It checks all business rows, numeric precision, identity
high-water marks, foreign keys and integrity. Completed exports use standalone
DELETE journaling. Existing output files are never overwritten. Preserve the
original encrypted PostgreSQL backup for recovery and derived PostgreSQL data.

Set log `object_storage.type` to `r2`, with a bucket, prefix and optional gzip.
`BIFROST_CLOUDFLARE_R2_URL=http://bifrost.objects` and a private
`BIFROST_CLOUDFLARE_R2_TOKEN` connect through the Container outbound binding.
No public S3 credential is needed. Each compressed object is limited to 100 MiB;
use bounded log batches and archive parts (8 MiB is the deployment default).
Existing inline log payloads remain readable through the hybrid log store.

Production requires verified database imports and authenticated acceptance
before replacing the existing service. Keep deployment secrets out of source
control and disable unrelated legacy telemetry exporters.

Run `npm ci && npm test` in `deploy/cloudflare` after setting up the Go workspace.
This runs the actual embedded engine against workerd SQL and R2, including every
canonical migration, nested migration savepoints, nanosecond timestamps, commit,
rollback and a full workerd replacement. It does not require a Cloudflare account.
The source merge pipeline requires these checks before image promotion.

The `Qualify native Cloudflare image` workflow also builds the production
Linux/amd64 CGO variant on pull requests, independently of the production
cutover switch. It runs the immutable local image against real workerd SQL and
R2, verifies the compiled revision and admin login, and replaces both runtimes
to check database retention. It requires no production credentials and does
not publish or deploy the image. The production deployment repeats qualification
against the exact published image digest before promotion.

The SQL authority limits each database file to 1 GiB. Monitor capacity before
that boundary and export cold log payloads to R2. Retention changes are separate
from migration: do not prune existing source logs merely to complete cutover.
Freeze all source writers before the final read-only export; run the current
canonical PostgreSQL migrations first. The converter rejects an incomplete source
migration ledger instead of skipping a data migration. Keep the encrypted source
backup and verify byte-level native imports before enabling production writers.

## Stable upstream release

This fork includes the stable open-source `transports/v2.5.1` release
(`7cb75fb2f8276ca48d0792c857fecb67b8879e2f`, published 2026-11-10).
The production transport uses core 1.15.2, framework 1.12.2 and the matching
plugin releases, built from local workspace sources to retain Cloudflare storage,
provider account login, subscription pricing and Jev System One routing.
The fork's Jev classifier remains available alongside upstream's decision classifier.
Production builds use Go 1.27.4 and patched Brotli, compress and x/net releases
to satisfy the production vulnerability gate.

The upgrade applies upstream's incremental configuration and logging migrations
through the existing migration ledger. Keep the existing encryption key, setup
token and admin credentials. Native SQL qualification covers the full migration
set, transactions, restart retention, fencing, R2 and SQL metering.

When MCP OAuth discovery is enabled, its configuration must specify an explicit
`oauth2_server_config.issuer_url`; header-only MCP deployments need no issuer.
Production keeps dashboard authentication and inference authentication enabled.
Automatic main-merge deployment qualifies the immutable native image and promotes
the backend before its frontend assets. A queued backend release resumes frontend
publication through `cloudflare-frontend.yml` using the original checked artifact.
