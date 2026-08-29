#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly tarball=${1:?deployment tarball required}
readonly image=${2:?immutable image required}
readonly actor=${3:?deployment actor required}
readonly known_hosts=${4:?known_hosts path required}
: "${ORION_SSH_KEY:?ORION_SSH_KEY is required}"
: "${GHCR_TOKEN:?GHCR_TOKEN is required}"
: "${ORION_HOST:?ORION_HOST is required}"
: "${ORION_USER:?ORION_USER is required}"

[[ -f $tarball && -f $known_hosts ]] || { echo "missing deployment input" >&2; exit 1; }
key=$(mktemp)
stage=
cleanup() {
	if [[ -n $stage ]]; then
		ssh "${ssh_opts[@]}" "$ORION_USER@$ORION_HOST" "rm -rf -- $(printf '%q' "$stage")" >/dev/null 2>&1 || true
	fi
	rm -f -- "$key"
}
trap cleanup EXIT
printf '%s\n' "$ORION_SSH_KEY" >"$key"
chmod 0600 "$key"
ssh_opts=(-i "$key" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$known_hosts")

stage=$(ssh "${ssh_opts[@]}" "$ORION_USER@$ORION_HOST" 'umask 077; d=$(mktemp -d); chmod 0700 "$d"; printf %s "$d"')
case $stage in /tmp/tmp.*|/var/tmp/tmp.*) ;; *) echo "refusing unexpected staging path" >&2; exit 1 ;; esac
scp "${ssh_opts[@]}" -- "$tarball" "$ORION_USER@$ORION_HOST:$stage/bifrost-orion-deploy.tar.gz"
printf '%s' "$GHCR_TOKEN" | ssh "${ssh_opts[@]}" "$ORION_USER@$ORION_HOST" \
	"umask 077; cat > $(printf '%q' "$stage/registry-token"); chmod 0600 $(printf '%q' "$stage/registry-token")"

remote_command="STAGE=$(printf '%q' "$stage") IMAGE=$(printf '%q' "$image") ACTOR=$(printf '%q' "$actor") bash -s"
ssh "${ssh_opts[@]}" "$ORION_USER@$ORION_HOST" "$remote_command" <<'REMOTE'
set -euo pipefail
umask 077
case "$STAGE" in /tmp/tmp.*|/var/tmp/tmp.*) ;; *) echo "refusing unexpected staging path" >&2; exit 1 ;; esac
[[ ! -L $STAGE && -d $STAGE && $(stat -c '%u:%a' "$STAGE") == 0:700 ]] || { echo "invalid staging directory" >&2; exit 1; }
tar -C "$STAGE" -xzf "$STAGE/bifrost-orion-deploy.tar.gz"
/bin/bash "$STAGE/install.sh" "$STAGE" "$IMAGE" "$ACTOR" <"$STAGE/registry-token"
REMOTE
