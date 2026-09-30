#!/usr/bin/env bash
set -euo pipefail

helper=$(cd "$(dirname "$0")" && pwd)/orion-push.sh
readonly helper
test_root=$(mktemp -d "${TMPDIR:-/tmp}/bifrost-orion-push-test.XXXXXX")
readonly test_root
trap 'rm -rf -- "$test_root"' EXIT
readonly remote="$test_root/euforicio/orion-infra.git"
readonly checkout="$test_root/checkout"
readonly original=sha256:93edfb5066db5138c599a6a5a7414b3e0007846106968baad210236b0337409b
readonly target=sha256:2974c023480a358d1e19dfe1a553700dcf19aeadc58eab504a309addc3161235
mkdir -p "$(dirname "$remote")"
git init --quiet --bare --initial-branch=main "$remote"
git clone --quiet "$remote" "$checkout" 2>/dev/null
git -C "$checkout" config user.name 'Deployment test'
git -C "$checkout" config user.email 'deployment-test@users.noreply.github.com'
mkdir -p "$checkout/apps/bifrost"

for format in name-first digest-first; do
  if [[ $format == name-first ]]; then
    cat > "$checkout/apps/bifrost/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
images:
- name: ghcr.io/euforicio/bifrost
  newName: ghcr.io/euforicio/bifrost
  digest: $original
EOF
  else
    cat > "$checkout/apps/bifrost/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
images:
- digest: $original
  name: ghcr.io/euforicio/bifrost
  newName: ghcr.io/euforicio/bifrost
EOF
  fi
  git -C "$checkout" add apps/bifrost/kustomization.yaml
  git -C "$checkout" commit --quiet -m "Seed $format manifest"
  git -C "$checkout" push --quiet origin main
  if ! "$helper" "ghcr.io/euforicio/bifrost@$target" "$checkout" deployment-test > "$test_root/helper.log" 2>&1; then
    cat "$test_root/helper.log"
    echo "promotion failed for $format" >&2
    exit 1
  fi
  git --git-dir="$remote" show main:apps/bifrost/kustomization.yaml | grep -Fq "digest: $target"
  before=$(git -C "$checkout" rev-parse HEAD)
  "$helper" "ghcr.io/euforicio/bifrost@$target" "$checkout" deployment-test > "$test_root/helper.log" 2>&1
  [[ $(git -C "$checkout" rev-parse HEAD) == "$before" ]]
  echo "$format promotion and repeat passed"
done

# An ambiguous manifest must fail before changing the published commit.
printf '\n- digest: %s\n' "$original" >> "$checkout/apps/bifrost/kustomization.yaml"
if "$helper" "ghcr.io/euforicio/bifrost@$target" "$checkout" deployment-test > "$test_root/helper.log" 2>&1; then
  echo 'ambiguous digest manifest was accepted' >&2
  exit 1
fi
[[ $(git --git-dir="$remote" rev-parse main) == "$before" ]]
echo 'ambiguous digest rejection passed'
