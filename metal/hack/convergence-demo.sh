#!/usr/bin/env bash
# convergence-demo.sh — the convergence model, curl-visible: BIOS settings staged
# over @Redfish.Settings apply across a system reboot, and a multipart firmware
# push flashes the BMC (version + Manager LastResetTime advance — the same
# observables a firmware update on real hardware is judged by).
#
# Runs entirely on loopback against `pk-metal sim serve` (X12 fixture); no
# hardware, no docker. jq required.
#
# usage: hack/convergence-demo.sh [--port <p>]
set -euo pipefail

PORT=${2:-8801}
BASE="http://127.0.0.1:${PORT}/redfish/v1"
cd "$(dirname "$0")/.."

command -v jq >/dev/null || { echo "error: jq is required" >&2; exit 1; }

echo "== build: pk-metal"
go build -o /tmp/pk-metal-demo ./cmd/pk-metal

echo "== boot: pk-metal sim serve (supermicro-x12) on :${PORT}"
/tmp/pk-metal-demo sim serve --fixture supermicro-x12 --listen "127.0.0.1:${PORT}" &
SIM=$!
trap 'kill $SIM 2>/dev/null || true; rm -f /tmp/pk-metal-demo' EXIT
for _ in $(seq 1 50); do
  curl -sf "$BASE" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -sf "$BASE" >/dev/null || { echo "error: sim never came up" >&2; exit 1; }

rung() { echo; echo "== $*"; }

rung "BIOS current: CSMSupport"
curl -s "$BASE/Systems/1/Bios" | jq -r '.Attributes.CSMSupport'

rung "stage CSMSupport=Enabled into the pending object (PATCH Bios/SD)"
curl -s -X PATCH -H 'Content-Type: application/json' \
  -d '{"Attributes":{"CSMSupport":"Enabled"}}' "$BASE/Systems/1/Bios/SD" -o /dev/null -w 'HTTP %{http_code}\n'

rung "pending visible, current untouched"
echo "pending: $(curl -s "$BASE/Systems/1/Bios/SD" | jq -r '.Attributes.CSMSupport')"
echo "current: $(curl -s "$BASE/Systems/1/Bios" | jq -r '.Attributes.CSMSupport')"

rung "reboot the system (ComputerSystem.Reset ForceRestart)"
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"ResetType":"ForceRestart"}' "$BASE/Systems/1/Actions/ComputerSystem.Reset" -o /dev/null -w 'HTTP %{http_code}\n'

rung "applied across the reboot, pending cleared"
echo "current: $(curl -s "$BASE/Systems/1/Bios" | jq -r '.Attributes.CSMSupport')"
echo "pending: $(curl -s "$BASE/Systems/1/Bios/SD" | jq -r '.Attributes.CSMSupport // "(none)"')"

rung "firmware baseline"
curl -s "$BASE/Managers/1" | jq -r '"version \(.FirmwareVersion)  lastReset \(.LastResetTime)"'

rung "multipart push (ApplyTime Immediate + preserve block) — the proven recipe"
IMG=$(mktemp); printf 'SIMFW 02.00.00' > "$IMG"
curl -s -F 'UpdateParameters={"Targets":["/redfish/v1/Managers/1"],"@Redfish.OperationApplyTime":"Immediate","Oem":{"Supermicro":{"BMC":{"PreserveCfg":true,"PreserveSdr":true,"PreserveSsl":true,"BackupBMC":true}}}};type=application/json' \
  -F "UpdateFile=@${IMG};type=application/octet-stream" \
  "$BASE/UpdateService/upload" -o /dev/null -w 'HTTP %{http_code}\n'
rm -f "$IMG"

rung "flashed: version + LastResetTime advanced, inventory follows"
curl -s "$BASE/Managers/1" | jq -r '"version \(.FirmwareVersion)  lastReset \(.LastResetTime)"'
curl -s "$BASE/UpdateService/FirmwareInventory/BMC" | jq -r '"inventory BMC \(.Version)"'

echo
echo "done — pending→applied across a reboot, flash proven by observables."
