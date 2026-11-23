#!/usr/bin/env bash
# Test the LLM endpoint of the bench from inside the bench container, so the
# network path is the same as the bench uses (host.docker.internal for a model
# on this machine). It lists the models and asks for one short answer, which
# also loads the model (the first answer of a cold Ollama model is slow).
#
#   scripts/llm-check.sh            (or: make llm-check)
#
# The bench reads PK_LLM_BASE_URL, PK_LLM_MODEL, PK_LLM_API_KEY and
# PK_LLM_EMBED_MODEL at start. After a change: docker compose up -d bench
set -euo pipefail
cd "$(dirname "$0")/.."

docker compose exec -T bench sh -s <<'EOF'
set -eu
if [ -z "${PK_LLM_BASE_URL:-}" ]; then
	echo "PK_LLM_BASE_URL is not set: the bench uses the mock model (deterministic answers)."
	exit 0
fi
url=${PK_LLM_BASE_URL%/}
auth=""
[ -n "${PK_LLM_API_KEY:-}" ] && auth="Authorization: Bearer $PK_LLM_API_KEY"
get() { wget -q -T 20 -O- ${auth:+--header "$auth"} "$1"; }

echo "endpoint: $url"
if ! models=$(get "$url/models" 2>&1); then
	echo "FAIL: GET $url/models: $models"
	echo "Is the model server running? On Linux it must listen on the docker bridge,"
	echo "for example OLLAMA_HOST=0.0.0.0:11434 (127.0.0.1 is not reachable from containers)."
	exit 1
fi
ids=$(printf '%s' "$models" | grep -o '"id" *: *"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/')
echo "models: $(printf '%s' "$ids" | tr '\n' ' ')"
model=${PK_LLM_MODEL:-$(printf '%s\n' "$ids" | head -n 1)}
if [ -n "${PK_LLM_MODEL:-}" ] && ! printf '%s\n' "$ids" | grep -qx "$PK_LLM_MODEL"; then
	echo "WARN: PK_LLM_MODEL=$PK_LLM_MODEL is not in the list above"
fi
echo "model: $model${PK_LLM_MODEL:+ (PK_LLM_MODEL)}"

body='{"model":"'"$model"'","messages":[{"role":"user","content":"Reply with the word OK."}],"max_tokens":16}'
start=$(date +%s)
if ! answer=$(wget -q -T 180 -O- --header "Content-Type: application/json" ${auth:+--header "$auth"} \
	--post-data "$body" "$url/chat/completions" 2>&1); then
	echo "FAIL: POST $url/chat/completions: $answer"
	exit 1
fi
took=$(( $(date +%s) - start ))
content=$(printf '%s' "$answer" | grep -o '"content" *: *"[^"]*"' | head -n 1 | sed 's/.*: *"\(.*\)"$/\1/')
echo "answer in ${took}s: ${content:-<no content field>}"
[ "$took" -gt 20 ] && echo "note: slow answer; run this check again to see the warm time"
if [ -n "${PK_LLM_EMBED_MODEL:-}" ]; then
	if wget -q -T 60 -O- --header "Content-Type: application/json" ${auth:+--header "$auth"} \
		--post-data '{"model":"'"$PK_LLM_EMBED_MODEL"'","input":["platform-kit"]}' "$url/embeddings" >/dev/null 2>&1; then
		echo "embeddings: $PK_LLM_EMBED_MODEL ok"
	else
		echo "WARN: embeddings with $PK_LLM_EMBED_MODEL failed; the bench falls back to keyword search"
	fi
fi
echo "ok"
EOF
echo "bench status:"
docker compose exec -T bench pk me get-status | sed -n '/^llm:/,/^[a-z]/p' | sed '$d'
