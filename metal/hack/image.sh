#!/usr/bin/env bash
# Builds the operator image and names it after the tree it was built from.
#
# The tag is the short commit sha when the tree is clean. A tree with local
# changes gets the sha plus a digest of the working tree, so every edit is a new
# tag: the Deployment's image reference changes and the cluster rolls. One tag
# for two different builds is how a dev loop ends up debugging yesterday's
# binary. There is no :latest for the same reason.
set -euo pipefail

SCRIPT=${0##*/}
HELP="usage: $SCRIPT tag | ref | build

  tag     print the tag for the current tree
  ref     print the full image reference (name:tag)
  build   docker build the image and print its reference

environment variables:
  PK_IMAGE_NAME          image name without tag (default: pk-metal)
  PK_IMAGE_CA_BUNDLE     PEM bundle the build trusts while it downloads modules, for a
                         network that re-signs TLS (default: none)
  PK_IMAGE_BUILD_FLAGS   extra flags for docker build, split on spaces,
                         e.g. --network=host (default: none)"

IMAGE_NAME=${PK_IMAGE_NAME:-pk-metal}
CA_BUNDLE=${PK_IMAGE_CA_BUNDLE:-}
BUILD_FLAGS=${PK_IMAGE_BUILD_FLAGS:-}

# State the teardown owns: the scratch index tree_tag() hashes the working tree
# with. A trap handler takes no arguments, hence the global.
scratch_index=""

usage() { echo "$HELP" >&2; }

die() {
  echo "Error: $*" >&2
  exit 1
}

cleanup() {
  if [[ -n $scratch_index ]]; then
    rm -f "$scratch_index"
  fi
}
trap cleanup EXIT INT TERM

# Prints the tag for the tree at $1.
tree_tag() {
  local repo=$1 sha tree
  sha=$(git -C "$repo" rev-parse --short=12 HEAD) \
    || die "$repo is not a git checkout with a commit. The image tag comes from the commit:
  build from a clone, not from an exported tarball."
  if [[ -z $(git -C "$repo" status --porcelain) ]]; then
    echo "$sha"
    return
  fi
  # A tree object for the working tree as it stands, untracked files included,
  # ignored ones not. It is staged into a scratch copy of the index, so the real
  # index and whatever is staged in it are not touched.
  scratch_index=$(mktemp)
  cp "$(git -C "$repo" rev-parse --absolute-git-dir)/index" "$scratch_index"
  GIT_INDEX_FILE=$scratch_index git -C "$repo" add --all
  tree=$(GIT_INDEX_FILE=$scratch_index git -C "$repo" write-tree)
  echo "$sha-dirty.${tree:0:12}"
}

build() {
  local context=$1 ref=$2 version=$3 ca_bundle=$4 flags=$5
  local args=(--tag "$ref" --build-arg "VERSION=$version")
  local proxy_var
  command -v docker >/dev/null 2>&1 \
    || die "docker is not installed. See https://docs.docker.com/engine/install/"
  docker info >/dev/null 2>&1 \
    || die "the docker daemon does not answer. Start it (sudo systemctl start docker)
  and make sure this user may talk to it (sudo usermod -aG docker \$USER, then log in again)."
  # The proxy settings of this shell, if it has any, reach the build as the
  # predefined build arguments docker never writes into the image.
  for proxy_var in HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy; do
    if [[ -n ${!proxy_var:-} ]]; then
      args+=(--build-arg "$proxy_var")
    fi
  done
  if [[ -n $ca_bundle ]]; then
    [[ -r $ca_bundle ]] \
      || die "PK_IMAGE_CA_BUNDLE=$ca_bundle is not a readable file. Point it at a PEM bundle or unset it."
    args+=(--secret "id=ca-bundle,src=$ca_bundle")
  fi
  # PK_IMAGE_BUILD_FLAGS is a list of flags the caller wrote for docker; it is
  # split on spaces on purpose.
  # shellcheck disable=SC2206
  args+=($flags)
  echo "  docker build $ref" >&2
  DOCKER_BUILDKIT=1 docker build "${args[@]}" "$context" >&2
  echo "$ref"
}

main() {
  local verb=${1:-} module repo tag
  module=$(cd "$(dirname "$0")/.." && pwd)
  repo=$(git -C "$module" rev-parse --show-toplevel) \
    || die "$module is not inside a git checkout."
  case "$verb" in
    tag)
      tree_tag "$repo"
      ;;
    ref)
      tag=$(tree_tag "$repo")
      echo "$IMAGE_NAME:$tag"
      ;;
    build)
      tag=$(tree_tag "$repo")
      build "$module" "$IMAGE_NAME:$tag" "$tag" "$CA_BUNDLE" "$BUILD_FLAGS"
      ;;
    -h | --help | help)
      usage
      ;;
    *)
      usage
      exit 1
      ;;
  esac
}

main "$@"
