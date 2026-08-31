#!/usr/bin/env bash
set -euo pipefail

readonly image=${1:?immutable image required}
readonly infra_root=${2:?orion-infra checkout required}
readonly actor=${3:?deployment actor required}

if [[ ! $image =~ ^ghcr\.io/euforicio/bifrost@sha256:[0-9a-f]{64}$ ]]; then
	echo "image must be an immutable euforicio/bifrost digest" >&2
	exit 1
fi

cd "$infra_root"
[[ -d .git ]] || { echo "orion-infra checkout is not a Git repository" >&2; exit 1; }
git remote get-url origin | grep -Eq '(^|[:/])euforicio/orion-infra(\.git)?$' || {
	echo "refusing to update an unexpected Git repository" >&2
	exit 1
}

git pull --rebase origin main

readonly manifest=apps/bifrost/kustomization.yaml
[[ -f $manifest ]] || { echo "missing $manifest" >&2; exit 1; }
[[ $(grep -Ec '^[[:space:]]*digest:' "$manifest") -eq 1 ]] || {
	echo "$manifest must contain exactly one image digest" >&2
	exit 1
}

readonly digest=${image##*@}
sed -E -i.bak "s|^([[:space:]]*digest:).*$|\1 $digest|" "$manifest"
rm -f -- "$manifest.bak"
grep -Fq "digest: $digest" "$manifest"

if git diff --quiet -- "$manifest"; then
	echo "Bifrost already points at $digest"
	exit 0
fi

git config user.name "$actor"
git config user.email "$actor@users.noreply.github.com"
git add "$manifest"
git commit -m "Deploy Bifrost ${digest:7:12}"

for attempt in 1 2 3; do
	if git push origin HEAD:main; then
		exit 0
	fi
	if [[ $attempt -eq 3 ]]; then
		break
	fi
	git pull --rebase origin main
done

echo "failed to push Orion GitOps update after three attempts" >&2
exit 1
