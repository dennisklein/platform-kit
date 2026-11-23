#!/usr/bin/env bash
# Port preflight for the demo stack: before `docker compose up`, make sure that
# the host ports of bench, Gitea and fluxsim are free. Without it, a port
# conflict on the bench shows up only after about 40 s of first-start seeding.
#
#   scripts/preflight-ports.sh      (make up and make fresh run it)
#
# Ports: PK_BENCH_PORT (8080), PK_FORGE_PORT (3000), PK_FLUX_PORT (8081), from
# the shell, else from .env next to compose.yaml, else the default. A port that
# this compose project already publishes (the stack runs) is not a conflict.
# Works with bash 3.2 (macOS) and on Linux.
set -u
cd "$(dirname "$0")/.."

# env_port VAR DEFAULT: the value from the shell, else from .env, else DEFAULT.
env_port() {
	local var=$1 def=$2 val
	eval "val=\${$var:-}"
	if [ -z "$val" ] && [ -f .env ]; then
		val=$(sed -n "s/^[[:space:]]*$var=[\"']\{0,1\}\([0-9]*\).*/\1/p" .env | tail -n1)
	fi
	echo "${val:-$def}"
}

# in_use PORT: true when something accepts connections on 127.0.0.1:PORT.
in_use() {
	if command -v nc >/dev/null 2>&1; then
		nc -z -w 1 127.0.0.1 "$1" >/dev/null 2>&1
	else
		(exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1
	fi
}

# ours SERVICE CONTAINER_PORT HOST_PORT: true when this compose project
# already publishes HOST_PORT for SERVICE.
ours() {
	docker compose port "$1" "$2" 2>/dev/null | grep -q ":$3\$"
}

# free_after PORT: the next free port in steps of 10, for the hint.
free_after() {
	local p=$(($1 + 10)) i=0
	while [ $i -lt 20 ] && in_use $p; do
		p=$((p + 10))
		i=$((i + 1))
	done
	echo $p
}

fail=0
for row in "PK_BENCH_PORT 8080 bench 8080" "PK_FORGE_PORT 3000 gitea 3000" "PK_FLUX_PORT 8081 fluxsim 8081"; do
	set -- $row
	var=$1 def=$2 svc=$3 cport=$4
	port=$(env_port "$var" "$def")
	if in_use "$port" && ! ours "$svc" "$cport" "$port"; then
		echo "port $port ($svc) is in use on 127.0.0.1: stop that program, or set $var=$(free_after "$port") in .env next to compose.yaml" >&2
		fail=1
	fi
done
if [ $fail = 1 ]; then
	echo "Then run the command again. The bench URL follows PK_BENCH_PORT." >&2
	exit 1
fi
echo "ports free: bench $(env_port PK_BENCH_PORT 8080), Gitea $(env_port PK_FORGE_PORT 3000), fluxsim $(env_port PK_FLUX_PORT 8081)"
