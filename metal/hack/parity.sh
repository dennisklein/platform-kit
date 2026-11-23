#!/usr/bin/env bash
# parity.sh — the render gate and, eventually, the cutover gate: `pk-metal
# convert` + `render` must reproduce `talhelper genconfig` byte-for-byte for every
# live node in control/talos. Runs on a THROWAWAY secret generated per run — the
# sops-sealed talsecret.sops.yaml is never touched (parity-live, with the real
# bundle, is the separate cutover rung). talosconfig embeds a client cert minted per
# run, so presence is asserted and bytes are not.
#
#   make -C metal parity          # needs the mise-pinned talhelper CLI on PATH
#   TALHELPER=/path/to/talhelper make -C metal parity
set -euo pipefail

REPO=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
TALOS_DIR=$REPO/control/talos
TALHELPER=${TALHELPER:-talhelper}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cp "$TALOS_DIR"/talconfig.yaml "$TALOS_DIR"/talenv.yaml "$TALOS_DIR"/talenv.inventory.yaml "$tmp"/
cp -r "$TALOS_DIR"/patches "$tmp"/patches

"$TALHELPER" gensecret > "$tmp/talsecret.yaml"

(cd "$tmp" && "$TALHELPER" genconfig --offline-mode --no-gitignore \
    -e talenv.yaml -e talenv.inventory.yaml -o ref >/dev/null)

cd "$REPO/metal"
go run ./cmd/pk-metal convert --talos-dir "$tmp" --out "$tmp/crs" \
    --env-file "$tmp/talenv.yaml" --env-file "$tmp/talenv.inventory.yaml" >/dev/null
go run ./cmd/pk-metal render --dir "$tmp/crs" --secret "$tmp/talsecret.yaml" \
    --patch-root "$tmp" --out "$tmp/got" >/dev/null

[[ -f $tmp/ref/talosconfig && -f $tmp/got/talosconfig ]] \
    || { echo "parity: talosconfig missing from an output" >&2; exit 1; }
rm "$tmp"/ref/talosconfig "$tmp"/got/talosconfig

nodes=$(ls "$tmp/ref" | wc -l)
[[ $nodes -ge 5 ]] || { echo "parity: only $nodes machine configs in the reference — expected all live nodes" >&2; exit 1; }

diff -ru "$tmp/ref" "$tmp/got"
echo "parity: $nodes machine configs byte-identical to talhelper genconfig"
