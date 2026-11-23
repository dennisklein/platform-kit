#!/usr/bin/env bash
# Demo flow end to end (spec §8, docs/demo.html) through the bench API, then
# the UI smoke test.
#
#   scripts/e2e.sh                  docker compose up, API story, UI smoke test
#
# Environment:
#   PK_BENCH_URL      bench URL (default: the port compose publishes for the
#                     bench, so PK_BENCH_PORT in .env works; else 8080)
#   PK_ADMIN_TOKEN    bench admin token (default: read from the bench container)
#   E2E_SKIP_COMPOSE  1: the stack already runs (for example a local pk bench)
#   E2E_SKIP_UI       1: skip the Playwright smoke test
#   E2E_NO_LAB        1: no lab service; the demo change then touches a path
#                     without lab gate (docs/) instead of the Node files
#   E2E_RESET         1: rebuild the forge state first (pk forge-init --reset),
#                     so the story can run again; the bench drops the worktrees
#                     of the old state. E2E_RESET_CMD overrides the command
#                     (default: docker compose run --rm forge-init forge-init
#                     --reset; bench, ci and fluxsim read the new forge
#                     credentials without a restart)
#   E2E_STEPS         space separated subset of the steps below (default all);
#                     E2E_CHANGE and E2E_MR resume with an earlier change
#
# Each step is a function step_<name>; state between steps lives in shell
# variables (CHANGE, MERGE_SHA, ...). Soft checks (features of other areas
# that may still be missing) print "warn:" and go on; hard checks fail.
set -euo pipefail
cd "$(dirname "$0")/.."

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
# bench_url: PK_BENCH_URL, else the host port compose publishes for the bench
# (PK_BENCH_PORT from the shell or .env), else 8080.
bench_url() {
	if [ -n "${PK_BENCH_URL:-}" ]; then
		echo "$PK_BENCH_URL"
		return
	fi
	local hp
	hp=$(docker compose port bench 8080 2>/dev/null | tail -n1 || true)
	if [ -n "$hp" ]; then
		echo "http://${hp/0.0.0.0/127.0.0.1}"
		return
	fi
	local p=${PK_BENCH_PORT:-}
	if [ -z "$p" ] && [ -f .env ]; then
		p=$(sed -n 's/^[[:space:]]*PK_BENCH_PORT=["'\'']\{0,1\}\([0-9]*\).*/\1/p' .env | tail -n1)
	fi
	echo "http://127.0.0.1:${p:-8080}"
}
BENCH=$(bench_url)
API="$BENCH/api/v1"
NO_LAB=${E2E_NO_LAB:-0}
# Resume a partial run (E2E_STEPS) with the change of an earlier run.
CHANGE=${E2E_CHANGE:-}
MR=${E2E_MR:-}
MERGE_SHA=${E2E_MERGE_SHA:-}

say()  { printf '\n== %s\n' "$*"; }
ok()   { printf '   ok: %s\n' "$*"; }
warn() { printf '   warn: %s\n' "$*"; }
die()  { printf '   FAIL: %s\n' "$*" >&2; exit 1; }

# call METHOD PATH [JSON] [EXPECTED_STATUS] — body in $OUT, status in $CODE.
call() {
	local method=$1 path=$2 body=${3:-} want=${4:-2..} args
	args=(-sS -X "$method" -H "Authorization: Bearer $TOKEN" -H 'Accept: application/json' -o $TMP/out -w '%{http_code}')
	if [ -n "$body" ]; then
		args+=(-H 'Content-Type: application/json' --data-binary "$body")
	fi
	CODE=$(curl "${args[@]}" "$API$path")
	OUT=$(cat $TMP/out)
	if ! [[ $CODE =~ ^$want$ ]]; then
		die "$method $path: want $want, got $CODE: $OUT"
	fi
}
# j FILTER — jq over the last response.
j() { jq -r "$@" <<<"$OUT"; }
# urlenc STRING
urlenc() { jq -rn --arg s "$1" '$s|@uri'; }
# put_file PATH CONTENT — save a file in the change worktree.
put_file() {
	call PUT "/changes/$CHANGE/file?path=$(urlenc "$1")" "$(jq -n --arg c "$2" '{content: $c}')"
}

# ------------------------------------------------------------------ stack

start_stack() {
	if [ "${E2E_SKIP_COMPOSE:-0}" != 1 ]; then
		docker compose up -d --build --wait bench
		BENCH=$(bench_url)
		API="$BENCH/api/v1"
	fi
	echo "bench: $BENCH"
	for _ in $(seq 60); do
		curl -fsS "$BENCH/healthz" >/dev/null 2>&1 && break
		sleep 2
	done
	curl -fsS "$BENCH/healthz" >/dev/null || die "bench not healthy at $BENCH"
	TOKEN=${PK_ADMIN_TOKEN:-}
	if [ -z "$TOKEN" ]; then
		TOKEN=$(docker compose exec -T bench cat /data/state/admin-token)
	fi
	# The change service comes up when the forge credentials exist.
	for _ in $(seq 60); do
		code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$API/changes")
		[ "$code" = 200 ] && break
		sleep 2
	done
	if [ "${E2E_RESET:-0}" = 1 ]; then
		say "reset forge state"
		bash -c "${E2E_RESET_CMD:-docker compose run --rm forge-init forge-init --reset}"
		# The bench reads the new credentials every 2 s, then drops the
		# worktrees of the old state; wait until it sees the seeded changes.
		sleep 3
		for _ in $(seq 60); do
			if curl -fsS -H "Authorization: Bearer $TOKEN" "$API/changes" 2>/dev/null |
				jq -e '[.changes[]|select(.id=="c-31d0aa" and .state=="approved")]|length==1' >/dev/null; then
				break
			fi
			sleep 2
		done
		ok "forge reset"
	fi
}

# ------------------------------------------------------------------ steps

# 1. The rail shows the seeded open changes, grouped.
step_rail() {
	say "1. rail"
	call GET /me
	[ "$(j .id)" = dennis ] || die "me is $(j .id), want dennis"
	call GET /changes
	for id in c-31d0aa c-p4x8ma c-6m1pqd c-h3sy7n; do
		[ "$(j --arg id $id '.changes[]|select(.id==$id)|.id')" = "$id" ] || die "open change $id missing"
	done
	[ "$(j '.changes[]|select(.id=="c-31d0aa")|.state')" = approved ] || die "c-31d0aa not approved"
	[ "$(j '.changes[]|select(.id=="c-6m1pqd")|.group')" = review ] || warn "c-6m1pqd not in group review"
	[ "$(j '.changes[]|select(.id=="c-h3sy7n")|.group')" = bots ] || warn "c-h3sy7n not in group bots"
	[ "$(j '.changes[]|select(.id=="c-p4x8ma")|.state')" = lab ] || warn "c-p4x8ma not in state lab"
	ok "$(j '.changes|length') open changes, main $(j '.main.sha[0:7]')"
}

# 2. New change with intent and work item.
step_new_change() {
	say "2. new change"
	call POST /changes '{"title":"Add cn-035 and cn-036 to defiant","intent":"Two more compute nodes for org-b workloads in GC5A-6Z","workItems":["w-114"]}' 201
	CHANGE=$(j .id)
	[ "$(j .state)" = draft ] || die "new change state $(j .state)"
	[ "$(j .local)" = true ] || die "new change has no worktree"
	call GET "/changes/$CHANGE/file?path=$(urlenc ".pk/changes/$CHANGE.yaml")"
	[ "$(j .exists)" = true ] || die "metadata file missing"
	ok "$CHANGE on $(j .ref) with metadata file"
}

node_yaml() { # name unit mgmtPort dataBPort dataAPort cableN
	cat <<EOF
apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: $1
spec:
  cluster: defiant
  role: worker
  location:
    rack: GC5A-6Z
    unit: $2
    size: 2
  model: Supermicro SYS-220U-TNR
  serial: S4R7K2J0$2$2
  bmc:
    mac: 3c:ec:ef:a1:0d:$(printf %02x "$2")
  ports:
    - {name: bmc, cable: MGT-0$6, peer: {switch: mgmt, port: $3}, network: mgmt}
    - {name: p1, cable: DATA-0$(($6 + 60)), peer: {switch: data-b, port: $4}, network: data}
    - {name: p2, cable: DATA-0$(($6 + 61)), peer: {switch: data-a, port: $5}, network: data}
EOF
}

# 3. Node files (port problem on data-a 37, fixed to 38), then commit 1 with
# the suggested message (advisor commit-message).
step_edit() {
	say "3. edit"
	if [ "$NO_LAB" = 1 ]; then
		put_file "docs/capacity/defiant.md" "# Defiant capacity

cn-035 and cn-036 go to GC5A-6Z U6 and U8 ($CHANGE).
"
		ok "saved docs/capacity/defiant.md (no lab gate)"
		return
	fi
	put_file control/inventory/cn-035.yaml "$(node_yaml cn-035 6 37 37 37 141)"
	if j '.file.problems[].message' | grep -Eiq 'data-a 37|in use'; then
		ok "problem: $(j '.file.problems[0].message')"
	else
		warn "no port problem for data-a 37"
	fi
	put_file control/inventory/cn-035.yaml "$(node_yaml cn-035 6 37 37 38 141)"
	ok "cn-035 fixed to data-a 38, impact: $(j '.impact.text // "-"')"
	put_file control/inventory/cn-036.yaml "$(node_yaml cn-036 8 39 39 40 143)"
	ok "impact: $(j '.impact.text // "-"')"
	call GET "/changes/$CHANGE/problems"
	[ "$(j .errors)" = 0 ] || die "problems: $(j -c '[.problems[].message]')"
	local msg='feat(inventory): add cn-035 and cn-036 to GC5A-6Z'
	call POST "/changes/$CHANGE/commit-message" '{}' '...'
	if [[ $CODE =~ ^2 ]] && [ -n "$(j '.message // ""')" ]; then
		msg=$(j .message)
		ok "suggested message ($(j .model)): $(head -n1 <<<"$msg")"
	else
		warn "commit-message advisor: $CODE"
	fi
	call POST "/changes/$CHANGE/commits" "$(jq -n --arg m "$msg" '{message: $m}')" 201
	ok "commit 1 $(j .short) $(j .subject)"
}

# 5. BGP policy: add cn-035 only; "Check this commit" finds cn-036 missing;
# accept the suggested fix (falls back to a manual edit); commit 2.
step_bgp() {
	[ "$NO_LAB" = 1 ] && return
	say "5. bgp policy and check this commit"
	local p=clusters/defiant/infra/cilium/bgp-rack-6z.yaml
	call GET "/changes/$CHANGE/file?path=$(urlenc $p)"
	j .content | sed 's/^          - cn-034$/          - cn-034\n          - cn-035/' >$TMP/bgp
	put_file $p "$(cat $TMP/bgp)"
	ok "added cn-035, impact: $(j '.impact.text // "-"')"
	local sid=""
	call POST "/changes/$CHANGE/advisors/check-commit" '{}' '...'
	if [[ $CODE =~ ^2 ]]; then
		ok "check-commit: $(j -r '[.points[].text]|first // "-"')"
		sid=$(j -r '[.suggestions[]?|select(.path=="'$p'" and .status=="pending")|.id]|first // ""')
	else
		warn "check-commit advisor: $CODE"
	fi
	if [ -n "$sid" ]; then
		call POST "/suggestions/$sid/accept" '{}'
		ok "accepted suggestion $sid"
	else
		warn "no suggestion for $p, adding cn-036 by hand"
		call GET "/changes/$CHANGE/file?path=$(urlenc $p)"
		j .content | sed 's/^          - cn-035$/          - cn-035\n          - cn-036/' >$TMP/bgp
		put_file $p "$(cat $TMP/bgp)"
	fi
	call GET "/changes/$CHANGE/file?path=$(urlenc $p)"
	j .content | grep -q -- '- cn-036' || die "cn-036 not in the BGP policy"
}

# 4. Commit (all saved files).
step_commit() {
	say "4. commit"
	local msg='feat(cilium): peer cn-035 and cn-036 in rack 6Z'
	[ "$NO_LAB" = 1 ] && msg='docs: capacity plan for cn-035 and cn-036'
	call GET "/changes/$CHANGE/status"
	[ "$(j .clean)" = false ] || die "nothing to commit"
	call POST "/changes/$CHANGE/commits" "$(jq -n --arg m "$msg" '{message: $m}')" 201
	ok "commit $(j .short) $(j .subject)"
	call GET "/changes/$CHANGE/status"
	[ "$(j .clean)" = true ] || die "worktree not clean after commit"
}

# 6. Generic editor: HelmRelease interval problem, masked SOPS token; saving
# the masked text keeps the stored secret (the file is unchanged after).
step_editor() {
	say "6. editor"
	local p=clusters/defiant/infra/cilium/helmrelease.yaml orig
	call GET "/changes/$CHANGE/file?path=$(urlenc $p)"
	j .content >$TMP/hr
	grep -q 'ENC\[' $TMP/hr && die "secret value not masked"
	grep -q '••••••••' $TMP/hr || die "no masked value in $p"
	ok "secrets masked: $(j -c '[.secrets[]?.field]')"
	orig=$(cat $TMP/hr)
	put_file $p "$(sed 's/^  interval: 5m$/  interval: 5 minutes/' $TMP/hr)"
	j '.file.problems[].message' | grep -q 'must be a duration' || die "no duration problem: $(j -c '[.file.problems[].message]')"
	ok "problem: $(j '.file.problems[0].message')"
	put_file $p "$orig"
	call GET "/changes/$CHANGE/status"
	if j -r '.files[]?.path' | grep -qx "$p"; then
		die "$p changed after saving the masked text back"
	fi
	ok "masked save kept the stored secret; $p unchanged"
}

# Lab run on the head (skipped with E2E_NO_LAB).
run_lab() {
	[ "$NO_LAB" = 1 ] && return
	say "lab"
	call POST "/changes/$CHANGE/labs" '{}' 202
	local run
	run=$(j .id)
	for _ in $(seq 120); do
		call GET "/labs/runs/$(urlenc "$run")"
		[ "$(j .result)" != running ] && break
		sleep 1
	done
	[ "$(j .result)" = pass ] || die "lab $run: $(j .result) $(j .summary)"
	ok "$run pass: $(j .summary)"
}

# 7. Change lab.
step_lab() { run_lab; }

# 8. Review: impact, gates, overlap.
step_review() {
	say "8. review"
	call GET "/changes/$CHANGE"
	ok "impact $(j '.impact.text // "-"'), risk $(j '.risk // "-"'), overlaps $(j '[.overlapsDetail[].change]|join(",")')"
	j '.lifecycle[]|"\(.state):\(.status):\(.label)"' | tr '\n' ' '
	echo
	if [ "$NO_LAB" != 1 ] && ! j '.overlapsDetail[].change' | grep -q c-p4x8ma; then
		warn "no overlap with c-p4x8ma"
	fi
}

# 9. Push (draft MR), mark ready (metadata to the MR description).
step_push_ready() {
	say "9. push and ready"
	call POST "/changes/$CHANGE/push"
	[ "$(j .created)" = true ] || die "push did not open a merge request"
	[ "$(j .mr.draft)" = true ] || die "merge request is not a draft"
	MR=$(j .mr.number)
	ok "draft !$MR $(j .mr.url)"
	call POST "/changes/$CHANGE/ready"
	[ "$(j .mr.draft)" = false ] || die "still draft after ready"
	j .description | grep -q "^Pk-Change: $CHANGE" || die "description lacks the trailers: $(j .description)"
	call GET "/changes/$CHANGE/file?path=$(urlenc ".pk/changes/$CHANGE.yaml")"
	[ "$(j .exists)" = false ] || die "metadata file still in the branch"
	ok "ready, state $(curl -sS -H "Authorization: Bearer $TOKEN" "$API/changes/$CHANGE" | jq -r .state)"
}

# 10. Main moves (merge c-31d0aa); our change is behind; rebase; lab again.
step_main_moved() {
	say "10. main moved"
	call POST /changes/c-31d0aa/merge
	ok "c-31d0aa merged as $(j '.sha[0:7]')"
	call GET "/changes/$CHANGE"
	[ "$(j .behindTarget)" = 1 ] || die "behind $(j .behindTarget), want 1"
	call POST "/changes/$CHANGE/merge" '' 409
	j .error | grep -q 'behind main by 1, rebase first' || die "merge error: $OUT"
	ok "merge refused: $(j .error)"
	call POST "/changes/$CHANGE/rebase"
	[ "$(j .ok)" = true ] || die "rebase failed: $OUT"
	[ "$(j .change.behindTarget)" = 0 ] || die "still behind after rebase"
	if [ "$(j .impactUnchanged)" = true ]; then ok "rebased $(j '.from[0:7]')..$(j '.to[0:7]'), impact unchanged"; else warn "impact changed (or unknown) after rebase"; fi
	run_lab
}

# 11. Approvals as anna and jonas (demo endpoint).
step_approvals() {
	say "11. approvals"
	for who in anna jonas; do
		call POST /demo/approve "$(jq -n --arg a $who --arg c "$CHANGE" '{as: $a, changeId: $c}')"
		ok "approved as $who"
	done
	for _ in $(seq 30); do
		call GET "/changes/$CHANGE"
		[ "$(j .state)" = approved ] && break
		sleep 1
	done
	[ "$(j .state)" = approved ] || die "state $(j .state), approvals $(j '.approvals|tojson')"
	ok "approved $(j .approvals.have) of $(j .approvals.need)"
}

# Agents never approve. The step approves dennis-agent (a new token) and
# revokes the grant at the end. If dennis-agent already has an active grant
# (the token in your agent harness), it skips, so that token keeps working.
step_agent_denied() {
	say "agent approve denied"
	call GET /agents '' '...'
	if [[ $CODE =~ ^2 ]] && [ "$(j '[.[]?|select(.id=="dennis-agent" and .active==true)]|length' 2>/dev/null)" = 1 ]; then
		warn "agent grant active: skipped, to keep your token (E2E_AGENT_FORCE=1 runs it and revokes the grant)"
		[ "${E2E_AGENT_FORCE:-0}" = 1 ] || return 0
	fi
	call POST /agents/dennis-agent/approve '' '...'
	if [ "$CODE" = 501 ] || [ "$CODE" = 503 ]; then
		warn "agent grants not available ($CODE), skipped"
		return
	fi
	[[ $CODE =~ ^2 ]] || die "approve agent: $CODE $OUT"
	local agent
	agent=$(j .token)
	code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $agent" -H 'Content-Type: application/json' -d '{}' "$API/changes/c-6m1pqd/approve")
	[ "$code" = 403 ] || die "agent approve: $code, want 403"
	ok "agent approve 403"
	call DELETE /agents/dennis-agent/grant '' '2..'
}

# 12. Merge with trailers.
step_merge() {
	say "12. merge"
	call POST "/changes/$CHANGE/merge"
	MERGE_SHA=$(j .sha)
	for t in "Pk-Change: $CHANGE" "Reviewed-in: !$MR" "Pk-Work-Item: w-114" "Pk-Author: dennis"; do
		j .message | grep -qF "$t" || die "merge message lacks '$t': $(j .message)"
	done
	for who in anna jonas; do
		j .message | grep -Eq "^Approved-by: .*\b$who\b" || die "merge message lacks Approved-by $who"
	done
	ok "merged ${MERGE_SHA:0:7}: $(j .message | head -n1)"
	[ "$NO_LAB" = 1 ] || j .message | grep -q "^Pk-Lab-Run: $CHANGE/run-" || die "merge message lacks Pk-Lab-Run"
}

# 12b. Flux: deployed and verified on defiant; the work item is done.
step_deploy() {
	say "12b. deploy"
	local st=""
	for _ in $(seq 60); do
		call GET "/changes/$CHANGE"
		st=$(j .state)
		[ "$st" = verified ] && break
		sleep 1
	done
	if [ "$st" = verified ]; then
		ok "verified: $(j -c '[.rollout[]?|"\(.cluster) \(.verified)"]')"
	else
		warn "state $st after 60 s (is fluxsim running?)"
	fi
	call GET /work
	local ws
	ws=$(j '.items[]|select(.id=="w-114")|.status')
	[ "$ws" = done ] && ok "w-114 done" || warn "w-114 is $ws"
}

# 13. History: our merge on top with trailers, filters, time travel.
step_history() {
	say "13. history"
	call GET '/history?limit=5'
	[ "$(j '.entries[0].change')" = "$CHANGE" ] || die "history top is $(j '.entries[0].change')"
	[ "$(j '.entries[0].sha')" = "$MERGE_SHA" ] || die "history top sha differs"
	j '.entries[0].trailers[]|"\(.key): \(.value)"' | grep -Eq '^Approved-by: .*anna' || die "history trailers"
	[ "$(j '.entries[1].change')" = c-31d0aa ] || die "c-31d0aa is not second"
	ok "top: $(j '.entries[0].short') $(j '.entries[0].title')"
	call GET '/history?entity=cn-003'
	local sha
	sha=$(j '.entries[]|select(.change=="c-2hd7vw")|.sha')
	[ -n "$sha" ] || die "entity filter cn-003 lacks c-2hd7vw"
	call GET "/history/$sha"
	ok "c-2hd7vw: $(j '.semantic|length') semantic lines, $(j '.files|length') files"
	call GET "/refs/$sha/file?path=$(urlenc control/inventory/cn-003.yaml)&convert=true"
	ok "cn-003 at c-2hd7vw stored $(j '.storedVersion // "-"'), converted from $(j '.convertedFrom // "-"')"
	call GET '/history?limit=50'
	[ "$(j '[.entries[]|select(.type=="break-glass")]|length')" -ge 1 ] || die "no break-glass entry"
}

# ------------------------------------------------------------------ main

ALL="rail new_change edit bgp commit editor lab review push_ready main_moved approvals agent_denied merge deploy history"
start_stack
for s in ${E2E_STEPS:-$ALL}; do
	"step_$s"
done
echo
echo "e2e api: ok ($CHANGE)"

if [ "${E2E_SKIP_UI:-0}" != 1 ]; then
	(cd web && PK_BENCH_URL="$BENCH" npx playwright test)
fi
echo "e2e: ok"
