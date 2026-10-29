#!/usr/bin/env bash
# The agent part of the demo without an agent harness: the MCP calls that an
# agent makes for "add cn-037", sent with the agent token. The web UI shows them
# live (presence chip, Settings → Agents). Runs inside the bench container, so
# it needs only Docker on the host.
#
#   scripts/mcp-demo.sh pk_agt_...     (token from Settings → Agents)
#
# Steps: pk_change_new, pk_file_write (cn-037), pk_commit, pk_lab_run,
# pk_lab_status (the lab fails: no BGP policy selects cn-037),
# pk_change_approve (denied: agents never approve or merge).
set -euo pipefail
cd "$(dirname "$0")/.."
token=${1:?usage: scripts/mcp-demo.sh <agent token>}

docker compose exec -T -e TOKEN="$token" bench sh -s <<'EOF'
set -eu
url=http://127.0.0.1:8080/mcp
n=0
# call TOOL ARGS_JSON: print the text of the tool result.
call() {
	n=$((n + 1))
	echo "> $1" >&2
	out=$(wget -q -T 60 -O- --header "Authorization: Bearer $TOKEN" --header 'Content-Type: application/json' \
		--header 'Accept: application/json, text/event-stream' \
		--post-data '{"jsonrpc":"2.0","id":'$n',"method":"tools/call","params":{"name":"'"$1"'","arguments":'"$2"'}}' "$url") || {
		echo "  MCP call failed (token expired? approve the agent again)" >&2
		exit 1
	}
	case $out in *'"isError":true'*) echo "  $(printf '%s' "$out" | sed 's/.*"text":"\([^"]*\)".*/\1/')" >&2 ;; esac
	printf '%s' "$out"
	sleep 1
}

out=$(call pk_change_new '{"title":"Add cn-037 to defiant","intent":"1 compute node for tenants, in GC5A-6Z U30"}')
id=$(printf '%s' "$out" | grep -o 'c-[a-z0-9]\{6\}' | head -n 1)
[ -n "$id" ] || { echo "no change id: $out" >&2; exit 1; }
echo "  $id" >&2

node='apiVersion: inventory.pk.io/v1beta1\nkind: Node\nmetadata:\n  name: cn-037\nspec:\n  cluster: defiant\n  role: worker\n  location:\n    rack: GC5A-6Z\n    unit: 30\n    size: 2\n  model: Supermicro SYS-220U-TNR\n  serial: S4R7K2J0037\n  bmc:\n    mac: 3c:ec:ef:a1:0c:37\n  ports:\n    - {name: bmc, cable: MGT-0143, peer: {switch: mgmt, port: 41}, network: mgmt}\n    - {name: p1, cable: DATA-0205, peer: {switch: data-b, port: 41}, network: data}\n    - {name: p2, cable: DATA-0206, peer: {switch: data-a, port: 42}, network: data}\n'
out=$(call pk_file_write '{"id":"'"$id"'","path":"control/inventory/cn-037.yaml","content":"'"$node"'"}')
echo "  impact: $(printf '%s' "$out" | grep -o '\\"text\\":\\"[^\\]*' | tail -n 1 | sed 's/.*"//')" >&2

call pk_commit '{"id":"'"$id"'","message":"feat(inventory): add cn-037 in GC5A-6Z"}' >/dev/null
out=$(call pk_lab_run '{"id":"'"$id"'"}')
run=$(printf '%s' "$out" | grep -o "$id/run-[0-9]*" | head -n 1)
echo "  $run running" >&2
result=running
for _ in 1 2 3 4; do
	out=$(call pk_lab_status '{"run":"'"$run"'"}')
	result=$(printf '%s' "$out" | grep -o '\\"result\\":\\"[a-z]*' | head -n 1 | sed 's/.*"//')
	[ "$result" != running ] && break
done
echo "  $run: $result · $(printf '%s' "$out" | grep -o '\\"summary\\":\\"[^\\]*' | sed 's/.*"//')" >&2
call pk_change_approve '{"id":"'"$id"'"}' >/dev/null
echo "done: $id (open it in the web UI; the chat can add cn-037 to the BGP policy)" >&2
EOF
