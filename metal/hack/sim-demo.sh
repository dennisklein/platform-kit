#!/usr/bin/env bash
# sim-demo.sh — the generic Redfish personality, curl-visible: a fleet of two
# simulated BMCs (no machine behind them) and one walk through what an operator
# does to boot a node from virtual media: read the service root, pick the
# optical slot, insert an image, set a one-time boot override, power on, follow
# a firmware update through its task monitor, eject.
#
# Runs entirely on loopback; no hardware, no VM, no network. curl and jq
# required.
#
# usage: hack/sim-demo.sh        (or: mise run sim:demo)
#   SIM_DEMO_PORT=<p>  first of three consecutive ports to listen on (default 8441)
set -euo pipefail

die() {
  echo "error: $*" >&2
  exit 1
}

rung() {
  echo
  echo "== $*"
}

# rf <machine-url> <path> [curl args…] — one authenticated Redfish request.
rf() {
  local base=$1 path=$2
  shift 2
  curl --silent --insecure --user "${ACCOUNT}" "$@" "${base}${path}"
}

# status <machine-url> <path> [curl args…] — the same, reduced to its HTTP status.
status() {
  local base=$1 path=$2
  shift 2
  rf "${base}" "${path}" --output /dev/null --write-out 'HTTP %{http_code}\n' "$@"
}

# etag <machine-url> <path> — the resource's current ETag.
etag() {
  rf "$1" "$2" --output /dev/null --dump-header - | tr -d '\r' | awk 'tolower($1) == "etag:" { print $2 }'
}

# post <machine-url> <path> <json> — a Redfish action.
post() {
  status "$1" "$2" --request POST --header 'Content-Type: application/json' --data "$3"
}

command -v curl >/dev/null || die "curl is required"
command -v jq >/dev/null || die "jq is required"

PORT=${SIM_DEMO_PORT:-8441}
ACCOUNT=admin:admin-pass
NODE1="https://127.0.0.1:${PORT}"
NODE2="https://127.0.0.1:$((PORT + 1))"
MEDIA="https://127.0.0.1:$((PORT + 2))"
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
FLEET_PID=
cleanup() {
  [ -z "${FLEET_PID}" ] || kill "${FLEET_PID}" 2>/dev/null || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

rung "build: pk-metal"
go build -o "${WORK}/pk-metal" ./cmd/pk-metal

# The image to mount: content does not matter to a BMC without a machine, the
# URL has to answer.
mkdir "${WORK}/media"
head -c 1048576 /dev/zero >"${WORK}/media/installer.iso"

cat >"${WORK}/fleet.yaml" <<EOF
media: { listen: "127.0.0.1:$((PORT + 2))", dir: "${WORK}/media" }
machines:
  - name: node-1
    listen: 127.0.0.1:${PORT}
    personality: generic
    credentials: { username: admin, password: admin-pass }
    identity: { serial: PKDEMO0001, uuid: 9b1c0001-0000-4000-8000-000000000001, bmcMAC: "02:77:00:00:01:01", nicMACs: ["52:54:00:77:00:11"] }
    backend: none
  - name: node-2
    listen: 127.0.0.1:$((PORT + 1))
    personality: generic
    credentials: { username: admin, password: admin-pass }
    identity: { serial: PKDEMO0002, uuid: 9b1c0002-0000-4000-8000-000000000002, bmcMAC: "02:77:00:00:02:01", nicMACs: ["52:54:00:77:00:21"] }
    backend: none
EOF

rung "start: pk-metal sim fleet (2 generic machines, backend none)"
"${WORK}/pk-metal" sim fleet --config "${WORK}/fleet.yaml" &
FLEET_PID=$!
for _ in $(seq 1 50); do
  curl --silent --insecure --fail "${NODE2}/redfish/v1" >/dev/null 2>&1 && break
  sleep 0.2
done
curl --silent --insecure --fail "${NODE2}/redfish/v1" >/dev/null || die "the fleet never came up"

rung "service root (public, no credentials)"
curl --silent --insecure "${NODE1}/redfish/v1" |
  jq '{RedfishVersion, Vendor, Product, Systems, Managers, Chassis, UpdateService, AccountService, SessionService, CertificateService, Tasks, EventService}'

rung "two machines, two identities"
for node in "${NODE1}" "${NODE2}"; do
  rf "${node}" /redfish/v1/Systems/1 |
    jq -r '"\(.SerialNumber)  uuid \(.UUID)  power \(.PowerState)  boot progress \(.BootProgress.LastState)"'
done

rung "a path nothing lives at answers 404 in the protocol's error format"
rf "${NODE1}" /redfish/v1/LicenseService | jq -r '.error | "\(.code): \(.message)"'

rung "virtual media hangs off the system; the optical slot is found by MediaTypes"
rf "${NODE1}" /redfish/v1/Systems/1/VirtualMedia | jq -r '.Members[]."@odata.id"' | while read -r slot; do
  rf "${NODE1}" "${slot}" | jq -r '"\(."@odata.id")  \(.MediaTypes | join(","))  inserted=\(.Inserted)"'
done
SLOT=$(rf "${NODE1}" /redfish/v1/Systems/1/VirtualMedia | jq -r '.Members[]."@odata.id"' | while read -r slot; do
  rf "${NODE1}" "${slot}" | jq -r 'select(.MediaTypes | index("CD")) | ."@odata.id"'
done | head -n 1)
[ -n "${SLOT}" ] || die "no optical virtual media slot"
echo "chosen: ${SLOT}"

rung "insert media (VirtualMedia.InsertMedia)"
post "${NODE1}" "${SLOT}/Actions/VirtualMedia.InsertMedia" \
  "{\"Image\":\"${MEDIA}/installer.iso\",\"Inserted\":true,\"WriteProtected\":true,\"TransferProtocolType\":\"HTTPS\"}"
rf "${NODE1}" "${SLOT}" | jq -r '"inserted=\(.Inserted)  image=\(.Image)"'

rung "boot override: a PATCH has to present the resource's ETag"
OVERRIDE='{"Boot":{"BootSourceOverrideTarget":"Cd","BootSourceOverrideEnabled":"Once","BootSourceOverrideMode":"UEFI"}}'
echo "without If-Match: $(status "${NODE1}" /redfish/v1/Systems/1 --request PATCH \
  --header 'Content-Type: application/json' --data "${OVERRIDE}")"
echo "with If-Match:    $(status "${NODE1}" /redfish/v1/Systems/1 --request PATCH \
  --header 'Content-Type: application/json' --header "If-Match: $(etag "${NODE1}" /redfish/v1/Systems/1)" --data "${OVERRIDE}")"
rf "${NODE1}" /redfish/v1/Systems/1 |
  jq -c '.Boot | {BootSourceOverrideEnabled, BootSourceOverrideTarget, BootSourceOverrideMode}'

rung "reset (ComputerSystem.Reset On)"
post "${NODE1}" /redfish/v1/Systems/1/Actions/ComputerSystem.Reset '{"ResetType":"On"}'
rf "${NODE1}" /redfish/v1/Systems/1 |
  jq -r '"power \(.PowerState)  boot progress \(.BootProgress.LastState)  override \(.Boot.BootSourceOverrideEnabled) (the one-time override is used up)"'
echo "node-2 is untouched: $(rf "${NODE2}" /redfish/v1/Systems/1 | jq -r '"power \(.PowerState)"')"

rung "firmware update: multipart push, then the task monitor"
rf "${NODE1}" /redfish/v1/Managers/1 | jq -r '"before: \(.FirmwareVersion)"'
PUSH=$(rf "${NODE1}" /redfish/v1/UpdateService | jq -r '.MultipartHttpPushUri')
TARGET=$(rf "${NODE1}" /redfish/v1/Managers/1 | jq -r '.Links.ActiveSoftwareImage."@odata.id"')
printf 'SIMFW 3.0.0' >"${WORK}/firmware.bin"
MONITOR=$(rf "${NODE1}" "${PUSH}" --output /dev/null --dump-header - \
  --form "UpdateParameters={\"Targets\":[\"${TARGET}\"],\"@Redfish.OperationApplyTime\":\"Immediate\"};type=application/json" \
  --form "UpdateFile=@${WORK}/firmware.bin;type=application/octet-stream" |
  tr -d '\r' | awk 'tolower($1) == "location:" { print $2 }')
[ -n "${MONITOR}" ] || die "the upload answered without a task monitor"
echo "task monitor: ${MONITOR}"
STATE=
for _ in $(seq 1 100); do
  NEXT=$(rf "${NODE1}" "${MONITOR}" | jq -r '.TaskState')
  [ "${NEXT}" = "${STATE}" ] || echo "task: ${NEXT}"
  STATE=${NEXT}
  [ "${STATE}" = Completed ] && break
  sleep 0.05
done
[ "${STATE}" = Completed ] || die "the task never completed (last state: ${STATE})"
rf "${NODE1}" /redfish/v1/Managers/1 | jq -r '"after:  \(.FirmwareVersion)"'

rung "eject media (VirtualMedia.EjectMedia)"
post "${NODE1}" "${SLOT}/Actions/VirtualMedia.EjectMedia" '{}'
rf "${NODE1}" "${SLOT}" | jq -r '"inserted=\(.Inserted)"'

rung "done"
