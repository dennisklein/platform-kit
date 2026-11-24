#!/usr/bin/env bash
# talos-docker-spike.sh — settle the questions the docker e2e harness design rests on:
#
#   1. does the Talos container image boot to MAINTENANCE MODE when no config is
#      injected? (talosctl 1.13 dropped `cluster create --skip-injecting-config`,
#      so the harness must docker-run the container itself)
#   2. does a stock `talosctl gen config` apply over the insecure maintenance API
#      on the container platform — and which docker-specific patches are needed?
#   3. do etcd bootstrap + node health complete, and how long does each rung take?
#   4. (--kind) is the container's :50000 reachable from the kind docker network?
#
# User-run (needs the docker socket). Touches nothing but docker + /tmp.
#
#   metal/hack/talos-docker-spike.sh          # rungs 1-3 on the default bridge
#   metal/hack/talos-docker-spike.sh --kind   # rungs 1-3, container on the kind network, + rung 4
#   metal/hack/talos-docker-spike.sh --clean  # remove container, volumes, /tmp state
set -euo pipefail

REPO=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
# The release the virtual lab installs; TALOS_VERSION overrides it.
TALOS_VERSION=${TALOS_VERSION:-$(sed -n 's/^ *talosVersion: *\(v[0-9.]*\).*/\1/p' "$REPO/lab/lab.yaml")}
IMAGE=ghcr.io/siderolabs/talos:$TALOS_VERSION
NAME=talos-spike
NET=bridge

case ${1:-} in
--clean)
    docker rm -f -v $NAME >/dev/null 2>&1 || true
    rm -rf /tmp/$NAME.*
    echo "clean"
    exit 0
    ;;
--kind)
    NET=kind
    docker network inspect kind >/dev/null 2>&1 || { echo "no kind docker network — make dev-up first" >&2; exit 1; }
    ;;
"") ;;
*) echo "usage: $0 [--kind|--clean]" >&2; exit 1 ;;
esac

tmp=$(mktemp -d /tmp/$NAME.XXXX)
echo "state: $tmp (talosconfig etc. — kept for poking; --clean removes)"
docker rm -f -v $NAME >/dev/null 2>&1 || true

# ── rung 1: boot with NO config, expect maintenance mode ─────────────────────────
SECONDS=0
docker run -d --name $NAME --hostname $NAME --network $NET \
    --read-only --privileged --security-opt seccomp=unconfined \
    --mount type=tmpfs,destination=/run \
    --mount type=tmpfs,destination=/system \
    --mount type=tmpfs,destination=/tmp \
    --mount type=volume,destination=/system/state \
    --mount type=volume,destination=/var \
    --mount type=volume,destination=/etc/cni \
    --mount type=volume,destination=/etc/kubernetes \
    --mount type=volume,destination=/usr/libexec/kubernetes \
    --mount type=volume,destination=/opt \
    -e PLATFORM=container \
    "$IMAGE" >/dev/null

ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" $NAME)
echo "container up: $ip ($IMAGE, net=$NET)"

deadline=$((SECONDS + 120))
until talosctl -n "$ip" version --insecure --short >/dev/null 2>&1; do
    ((SECONDS < deadline)) || {
        echo "VERDICT rung1: FAIL — no insecure API on :50000 after 120s; last log lines:" >&2
        docker logs --tail 15 $NAME >&2
        exit 1
    }
    sleep 2
done
echo "VERDICT rung1: maintenance API answering on :50000 after ${SECONDS}s"
docker logs $NAME 2>&1 | grep -i -m1 "maintenance" || echo "  (no 'maintenance' line in the log — check mode by hand)"

# ── rung 2: stock gen config + apply over the insecure API ───────────────────────
SECONDS=0
talosctl gen config spike "https://$ip:6443" \
    --output "$tmp" --with-docs=false --with-examples=false >/dev/null
talosctl apply-config --insecure -n "$ip" --file "$tmp/controlplane.yaml"
echo "VERDICT rung2: apply-config accepted after ${SECONDS}s (stock gen config, zero patches)"

# ── rung 3: bootstrap etcd, wait for health ──────────────────────────────────────
SECONDS=0
export TALOSCONFIG=$tmp/talosconfig
talosctl config endpoint "$ip"
talosctl config node "$ip"
for _ in $(seq 60); do
    talosctl bootstrap 2>/dev/null && break
    sleep 2
done
echo "  bootstrap accepted after ${SECONDS}s"
talosctl health --wait-timeout 5m
echo "VERDICT rung3: healthy after ${SECONDS}s total"
talosctl version --short | sed 's/^/  /'

# ── rung 4 (--kind): reach :50000 from a kind node ───────────────────────────────
if [[ $NET == kind ]]; then
    node=$(docker ps --format '{{.Names}}' | grep -m1 -- '-control-plane$') \
        || { echo "VERDICT rung4: SKIP — no kind node container running"; exit 0; }
    if docker exec "$node" bash -c "timeout 3 bash -c '</dev/tcp/$ip/50000'"; then
        echo "VERDICT rung4: $node reaches $ip:50000 (pods egress via the node, so pods do too)"
    else
        echo "VERDICT rung4: FAIL — $node cannot reach $ip:50000"
    fi
fi
