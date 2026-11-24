#!/usr/bin/env bash
# Runs every Go fuzz target in the module for a short, fixed time each.
#
# go test accepts one -fuzz target per package and run, so the targets are
# listed first (go test -list) and then run one by one. Plain `go test ./...`
# already runs every target over its seed corpus; this script is the slower
# search for new inputs, meant for a person before a release or after a
# change to a parser. It is not part of CI on purpose: a fuzz run is not
# repeatable, and a red CI run must point at a change, not at luck.
#
# A failing input is written by go test to testdata/fuzz/<Target>/ next to
# the target. Commit it with the fix: it then runs as a seed on every test.
set -euo pipefail

SCRIPT=${0##*/}
HELP="usage: $SCRIPT [package pattern]   (run from metal/; default pattern: ./...)

environment variables:
  FUZZTIME   how long each target runs, as go test -fuzztime takes it,
             e.g. 30s or 5000x (default: 10s)"

FUZZTIME=${FUZZTIME:-10s}

die() {
  echo "Error: $*" >&2
  exit 1
}

# list_targets prints one 'package target' line per fuzz target in pattern.
list_targets() {
  local pattern=$1 line names=()
  while IFS= read -r line; do
    case $line in
      Fuzz*) names+=("$line") ;;
      "ok "*)
        local pkg
        pkg=$(awk '{print $2}' <<<"$line")
        for name in "${names[@]}"; do
          echo "$pkg $name"
        done
        names=()
        ;;
    esac
  done < <(go test -list '^Fuzz' "$pattern")
}

main() {
  case ${1:-} in
    -h | --help) echo "$HELP" >&2; exit 0 ;;
  esac
  local pattern=${1:-./...}
  [[ -f go.mod ]] || die "run this from the metal/ directory, e.g.: cd metal && hack/$SCRIPT"

  local targets
  targets=$(list_targets "$pattern") || die "go test -list failed; run 'go test $pattern' to see why"
  # An empty list means the listing broke, not that there is nothing to do:
  # the module has fuzz targets, and a silent pass would hide that.
  [[ -n $targets ]] || die "no fuzz targets found in $pattern; check the pattern, e.g.: $SCRIPT ./internal/..."

  local failed=() pkg name
  while read -r pkg name; do
    echo "  fuzz $pkg $name for $FUZZTIME" >&2
    if ! go test -run '^$' -fuzz "^${name}\$" -fuzztime "$FUZZTIME" "$pkg" >&2; then
      failed+=("$pkg $name")
    fi
  done <<<"$targets"

  if ((${#failed[@]} > 0)); then
    printf '  failed: %s\n' "${failed[@]}" >&2
    die "${#failed[@]} fuzz target(s) failed; the failing inputs are in testdata/fuzz/ next to each target"
  fi
  echo "  all $(wc -l <<<"$targets") fuzz targets passed" >&2
}

main "$@"
