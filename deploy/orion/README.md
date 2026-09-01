# Orion production deployment

Bifrost is served at `https://bifrost.riftlabs.app` by the Orion K3s cluster.
The deployment is GitOps-managed by `euforicio/orion-infra`; this repository
builds and publishes the application image but does not SSH application files
to the server.

## Deployment flow

`.github/workflows/orion-image.yml` follows the Rift control-plane deployment
model:

1. Run the provider-account, transport, and UI validation suites.
2. Build and boot-smoke a Linux AMD64 image.
3. Push the image to `ghcr.io/euforicio/bifrost` and resolve its immutable
   digest.
4. Update `apps/bifrost/kustomization.yaml` in `euforicio/orion-infra` with
   that digest.
5. Let Flux reconcile the K3s Deployment on Orion.

The `orion-production` GitHub environment must contain `ORION_INFRA_TOKEN`, a
fine-grained token that can update the Orion infrastructure repository. The
runtime secrets, PostgreSQL credentials, Caddy ingress, network policy, and
persistent storage are owned by `orion-infra`, not this repository.

## Rollback

Run `.github/workflows/orion-rollback.yml` with a previously healthy
`sha256:...` image digest. The workflow pins that digest in the GitOps
repository; Flux performs the rollback. It does not mutate application data.

## Verification

After Flux reports the new revision ready, verify the deployed digest and
public health on Orion:

```bash
kubectl -n bifrost get deployment bifrost \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl -n bifrost rollout status deployment/bifrost --timeout=180s
curl --fail --silent --show-error https://bifrost.riftlabs.app/health
```

`provider-smoke.sh` remains the explicit inference acceptance suite. Supply
the virtual keys as environment variables; the script never reads runtime
provider credentials from the cluster.
