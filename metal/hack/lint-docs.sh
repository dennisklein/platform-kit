#!/usr/bin/env bash
# Every condition reason and phase value the API declares must be documented in
# the behaviour chapter. The vocabulary is the operator's ABI: a reason that
# reaches a human with nothing written about it is a dead end, and the way that
# happens is somebody adding a const and nobody adding a row.
#
# Generated field prose is not checked here — that page cannot drift, it is
# regenerated from the same types. This gate is only for the hand-written half.
set -euo pipefail

cd "$(dirname "$0")/.."
BEHAVIOUR=../docs/metal/behaviour.md

if [[ ! -f $BEHAVIOUR ]]; then
  echo "lint-docs: $BEHAVIOUR is missing" >&2
  exit 1
fi

missing=0

# Condition reasons and types: `ReasonFoo = "Foo"` / `ConditionFoo = "Foo"`.
while read -r value; do
  if ! grep -qF "\`$value\`" "$BEHAVIOUR"; then
    echo "lint-docs: reason/condition $value is not in the behaviour tables" >&2
    missing=$((missing + 1))
  fi
done < <(grep -rhoE '(Reason|Condition)[A-Za-z]+ += +"[A-Za-z]+"' api/ \
  | sed -E 's/.*"([A-Za-z]+)"/\1/' | sort -u)

# Phase values: `HostAvailable HostPhase = "Available"` and friends.
while read -r value; do
  if ! grep -qF "\`$value\`" "$BEHAVIOUR"; then
    echo "lint-docs: phase $value is not in the behaviour tables" >&2
    missing=$((missing + 1))
  fi
done < <(grep -rhoE '[A-Za-z]+Phase += +"[A-Za-z]+"' api/ \
  | sed -E 's/.*"([A-Za-z]+)"/\1/' | sort -u)

if ((missing > 0)); then
  echo "lint-docs: $missing undocumented value(s) — add them to $BEHAVIOUR" >&2
  exit 1
fi
echo "lint-docs: clean"
