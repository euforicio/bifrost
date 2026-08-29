#!/usr/bin/env bash
set -Eeuo pipefail

readonly stage=${1:?staging directory required}
readonly image=${2:?immutable image required}
readonly actor=${3:?deployment actor required}
readonly deploy_dir=/opt/bifrost
readonly caddyfile=/etc/caddy/Caddyfile

if [[ $EUID -ne 0 ]]; then
	echo "Orion installation requires root" >&2
	exit 77
fi
if [[ ! $image =~ ^ghcr\.io/euforicio/bifrost@sha256:[0-9a-f]{64}$ ]]; then
	echo "refusing non-immutable Bifrost image" >&2
	exit 64
fi
if [[ ! $actor =~ ^[A-Za-z0-9-]+$ ]]; then
	echo "invalid deployment actor" >&2
	exit 64
fi
for file in compose.yaml config.json bifrost-deploy Caddyfile.bifrost render-caddyfile.sh; do
	[[ -f $stage/$file ]] || { echo "missing staged file: $file" >&2; exit 78; }
done
[[ -s $deploy_dir/.env ]] || { echo "missing $deploy_dir/.env" >&2; exit 78; }

candidate=$(mktemp)
previous_caddy=$(mktemp)
token_file=$(mktemp)
cleanup() {
	rm -f -- "$candidate" "$previous_caddy" "$token_file"
}
trap cleanup EXIT
chmod 0600 "$token_file"
cat >"$token_file"
[[ -s $token_file ]] || { echo "missing ephemeral registry token" >&2; exit 77; }

"$stage/render-caddyfile.sh" "$stage/Caddyfile.bifrost" "$caddyfile" >"$candidate"
caddy validate --config "$candidate" --adapter caddyfile
cp --preserve=mode,ownership,timestamps "$caddyfile" "$previous_caddy"

install -d -m 0700 "$deploy_dir"
install -d -m 0770 -o 1000 -g 0 "$deploy_dir/data" "$deploy_dir/backups"
install -m 0644 "$stage/compose.yaml" "$stage/config.json" "$deploy_dir/"
install -m 0755 "$stage/bifrost-deploy" /usr/local/sbin/bifrost-deploy

deployed=false
recover() {
	local status=$?
	trap - ERR HUP INT TERM
	if [[ $deployed == true ]]; then
		echo "Orion installation failed after deployment; restoring Caddy and rolling Bifrost back" >&2
		install -m 0644 "$previous_caddy" "$caddyfile.bifrost-restore" || true
		mv -f "$caddyfile.bifrost-restore" "$caddyfile" || true
		systemctl reload caddy || true
		SSH_ORIGINAL_COMMAND="rollback $actor" /usr/local/sbin/bifrost-deploy <"$token_file" || true
	fi
	exit "$status"
}
trap recover ERR HUP INT TERM

SSH_ORIGINAL_COMMAND="deploy $image $actor" /usr/local/sbin/bifrost-deploy <"$token_file"
deployed=true
install -m 0644 "$candidate" "$caddyfile.bifrost-new"
mv -f "$caddyfile.bifrost-new" "$caddyfile"
systemctl reload caddy

curl --fail --silent --show-error http://127.0.0.1:8180/health >/dev/null
[[ $(curl --silent --output /dev/null --write-out '%{http_code}' http://127.0.0.1:8180/api/config) == 401 ]]
auth_status=$(curl --fail --silent --show-error http://127.0.0.1:8180/api/session/is-auth-enabled)
[[ $auth_status == *'"is_auth_enabled":true'* ]]

trap - ERR HUP INT TERM
echo "Bifrost deployed and Orion Caddy reloaded"
