#!/usr/bin/env bash
# Fresh-clone test: do what the README Quick start says, in a temporary
# directory, on other ports, and check that the demo comes up. Run it the day
# before the talk, on the laptop of the talk, after the release push.
#
#   scripts/clone-test.sh [REPO] [BRANCH]
#
# REPO     default: the origin URL of this checkout
# BRANCH   default: the default branch of REPO (what a plain git clone gets)
#
# It uses its own compose project (pkclone) and ports (PK_BENCH_PORT 18080,
# PK_FORGE_PORT 13000, PK_FLUX_PORT 18081 unless set), so a running demo
# stack is not touched. At the end it deletes the stack, its data and the
# clone (CLONE_TEST_KEEP=1 keeps them). CLONE_TEST_BUILD=0 does not build and
# uses the image PK_IMAGE as it is.
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
repo=${1:-$(git -C "$here" remote get-url origin)}
branch=${2:-}

export COMPOSE_PROJECT_NAME=${COMPOSE_PROJECT_NAME:-pkclone}
export PK_BENCH_PORT=${PK_BENCH_PORT:-18080} PK_FORGE_PORT=${PK_FORGE_PORT:-13000} PK_FLUX_PORT=${PK_FLUX_PORT:-18081}
export PK_IMAGE=${PK_IMAGE:-platform-kit:clone-test}
unset COMPOSE_FILE
bench=http://127.0.0.1:$PK_BENCH_PORT

dir=$(mktemp -d "${TMPDIR:-/tmp}/pk-clone-test.XXXXXX")
cleanup() {
	if [ "${CLONE_TEST_KEEP:-0}" = 1 ]; then
		echo "kept: $dir/platform-kit (project $COMPOSE_PROJECT_NAME, bench $bench)"
		return
	fi
	if [ -f "$dir/platform-kit/compose.yaml" ]; then
		(cd "$dir/platform-kit" && docker compose down -v --remove-orphans >/dev/null 2>&1) || true
	fi
	rm -rf "$dir"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
t0=$(date +%s)
step() { printf '[%3ss] %s\n' $(($(date +%s) - t0)) "$*"; }

step "git clone $repo ${branch:+-b $branch}"
git clone -q ${branch:+-b "$branch"} "$repo" "$dir/platform-kit"
cd "$dir/platform-kit"
step "checked out $(git rev-parse --abbrev-ref HEAD) at $(git rev-parse --short HEAD)"

missing=""
for f in compose.yaml Dockerfile Makefile README.md docs/demo.html scripts/e2e.sh scripts/mcp-demo.sh scripts/llm-check.sh scripts/preflight-ports.sh; do
	[ -e "$f" ] || missing="$missing $f"
done
[ -z "$missing" ] || fail "the clone has no$missing. Commit, push and merge the demo first (or pass the branch)."
grep -q 'pull_policy: never' compose.yaml || fail "compose.yaml of the clone is old (no pull_policy: never): the release push is missing"
if git ls-files --error-unmatch pk >/dev/null 2>&1; then
	echo "warn: the clone has a committed binary 'pk' (git rm --cached pk)"
fi

./scripts/preflight-ports.sh
if [ "${CLONE_TEST_BUILD:-1}" = 0 ]; then
	step "docker compose up -d --wait (no build, image $PK_IMAGE)"
	docker compose up -d --wait --quiet-pull
else
	step "docker compose up -d --build --wait (first build: 2 to 4 min)"
	docker compose up -d --build --wait --quiet-pull
fi
step "stack up"

curl -fsS "$bench/healthz" >/dev/null || fail "bench not healthy at $bench"
token=$(docker compose exec -T bench cat /data/state/admin-token)
open=""
for _ in $(seq 60); do
	open=$(curl -fsS -H "Authorization: Bearer $token" "$bench/api/v1/changes" 2>/dev/null |
		sed -n 's/.*"id":"\(c-[a-z0-9]*\)".*/\1/p' | head -n1) || true
	[ -n "$open" ] && break
	sleep 2
done
[ -n "$open" ] || fail "the bench lists no changes after 2 min"
for id in c-31d0aa c-p4x8ma c-6m1pqd c-h3sy7n; do
	curl -fsS -H "Authorization: Bearer $token" "$bench/api/v1/changes/$id" >/dev/null || fail "seeded change $id missing"
done
step "seeded changes c-31d0aa c-p4x8ma c-6m1pqd c-h3sy7n present"
curl -fsS "$bench/" | grep -qi '<html' || fail "the web UI is not served at $bench/"
curl -fsS "http://127.0.0.1:$PK_FORGE_PORT/api/healthz" >/dev/null || fail "Gitea not healthy"
curl -fsS "http://127.0.0.1:$PK_FLUX_PORT/status" >/dev/null || fail "fluxsim not answering"
docker compose exec -T bench pk me get-status >/dev/null || fail "pk CLI in the image does not work"
step "web UI, Gitea, fluxsim and the pk CLI answer"
echo "clone test: ok ($(git rev-parse --short HEAD), $(($(date +%s) - t0)) s)"
