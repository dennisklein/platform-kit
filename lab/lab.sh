#!/usr/bin/env bash
# The virtual lab: a kind cluster running the operator, a fleet of simulated
# BMCs each driving a QEMU machine, and the bridge between them. This script is
# the flow around those parts; `mise run lab:<verb>` is how it is meant to be
# called, because mise puts kind, kubectl, talosctl, yq and jq on the PATH at
# their pinned versions.
#
# Nothing here is part of the provisioning path. Once `up` has applied the
# manifests, the operator does the work and the only human step left is the
# approval (`approve`).
set -euo pipefail

SCRIPT=${0##*/}
HELP="usage: $SCRIPT <verb>

  doctor       check the workstation for what the lab needs; every miss names its fix
  up           network, kind cluster, operator, simulated fleet, lab manifests
  status       the rack as the operator sees it, then every kind
  approve      approve every operation that waits for a person
  kubeconfig   write the workload cluster's kubeconfig and talosconfig into the lab directory
  wait         approve and wait until every Host is Provisioned and every node Ready
  collect      write logs and object dumps to <lab directory>/diagnostics
  down         stop the fleet, delete the kind cluster, remove the network; keeps disks and caches
  reset        wipe the machines' disks and firmware state (the lab must be down)

  parts of up, for a partial rerun:
  network | cluster | deploy | fleet | manifests

environment variables:
  PK_LAB_DIR            where the lab keeps its state: kubeconfig, logs, generated
                        deployment, secrets bundle (default: <repo>/.lab)
  PK_LAB_FLEET          the fleet definition (default: <repo>/lab/fleet.yaml)
  PK_LAB_WAIT_TIMEOUT   seconds wait gives the lab to converge (default: 2400)
  PK_LAB_MIN_FREE_GIB   free disk doctor asks for under PK_LAB_DIR (default: 15)
  PK_LAB_KIND_CONFIG    the kind cluster definition (default: <repo>/lab/kind.yaml);
                        a copy with another node image is how a host still on
                        cgroup v1 runs the lab, see lab/README.md"

ROOT=$(cd "$(dirname "$0")/.." && pwd)
LAB_DIR=${PK_LAB_DIR:-$ROOT/.lab}
FLEET=${PK_LAB_FLEET:-$ROOT/lab/fleet.yaml}
WAIT_TIMEOUT=${PK_LAB_WAIT_TIMEOUT:-2400}
MIN_FREE_GIB=${PK_LAB_MIN_FREE_GIB:-15}
KIND_CONFIG=${PK_LAB_KIND_CONFIG:-$ROOT/lab/kind.yaml}

# Fixed names, shared with files that cannot read a variable: lab/kind.yaml names
# the kind cluster, the manifests under metal/config name the namespace and the
# Deployment, lab/manifests names the TalosCluster and the Secret its secretsRef
# points at.
CLUSTER=pk-metal
NAMESPACE=pk-metal
DEPLOYMENT=pk-metal-manager
TALOS_CLUSTER=lab
BUNDLE_SECRET=lab-talos-secrets

# Every kubectl, kind and pk-metal call below talks to the lab's own kubeconfig.
# The person's ~/.kube/config is never read and never written.
export KUBECONFIG=$LAB_DIR/kubeconfig

usage() { echo "$HELP" >&2; }

die() {
  echo "Error: $*" >&2
  exit 1
}

step() { echo "== $*" >&2; }

# ---------------------------------------------------------------- fleet file

fleet_query() {
  local fleet=$1 expression=$2
  [[ -r $fleet ]] || die "$fleet is not readable. Point PK_LAB_FLEET at a fleet file, e.g.: PK_LAB_FLEET=lab/fleet.yaml"
  yq -r "$expression" "$fleet"
}

# The expressions stay inside what jq and yq agree on, so the answers do not
# depend on which of the two yq implementations is on the PATH.
fleet_taps() { fleet_query "$1" '.machines[] | select(.backend == "qemu") | .qemu.tap'; }
fleet_listens() { fleet_query "$1" '.machines[].listen'; }
fleet_count() { fleet_query "$1" '.machines | length'; }

fleet_memory_mib() {
  fleet_query "$1" '.machines[] | select(.backend == "qemu") | .qemu.memoryMiB' \
    | awk '{ sum += $1 } END { print sum + 0 }'
}

# The directory the fleet keeps disks and firmware state in. Relative paths in
# the fleet file are relative to the repository root, which is where `fleet`
# starts the process.
fleet_state_dir() {
  local root=$1 fleet=$2 dir
  dir=$(fleet_query "$fleet" '.stateDir')
  [[ -n $dir && $dir != null ]] || die "$fleet has no stateDir. Add one, e.g.: stateDir: .lab/state"
  case "$dir" in
    /*) echo "$dir" ;;
    *) echo "$root/$dir" ;;
  esac
}

# The ledger the operator joins Assets against: BMC MAC -> the URL its simulator
# listens on. Derived, never written by hand, so it cannot disagree with the fleet.
fleet_ledger() {
  fleet_query "$1" '.machines[] | .identity.bmcMAC + " " + .listen' \
    | awk '{ printf "\"%s\": https://%s\n", tolower($1), $2 }'
}

# ---------------------------------------------------------------- doctor

# Prints a line per check and returns the number of failed ones. A check is a
# description, a fix and a command; the command's exit status is the verdict.
check() {
  local what=$1 fix=$2
  shift 2
  if "$@" >/dev/null 2>&1; then
    echo "ok    $what" >&2
    return 0
  fi
  echo "MISS  $what" >&2
  echo "      fix: $fix" >&2
  return 1
}

have() { command -v "$1" >/dev/null 2>&1; }

kvm_usable() { [[ -r /dev/kvm && -w /dev/kvm ]]; }

qemu_present() { have qemu-system-x86_64 && have qemu-img; }

net_tools_present() { have ip && have iptables; }

ovmf_present() {
  local pair
  if [[ -n ${PK_METAL_OVMF_CODE:-} ]]; then
    [[ -r $PK_METAL_OVMF_CODE && -r ${PK_METAL_OVMF_VARS:-} ]]
    return
  fi
  # The list the QEMU driver probes (metal/internal/machine/qemu).
  for pair in /usr/share/edk2/ovmf /usr/share/OVMF /usr/share/edk2-ovmf/x64; do
    [[ -r $pair/OVMF_CODE.fd || -r $pair/OVMF_CODE_4M.fd ]] && return 0
  done
  return 1
}

sudo_usable() { (( EUID == 0 )) || sudo -n true || sudo -v; }

cgroup_v2() { [[ -f /sys/fs/cgroup/cgroup.controllers ]]; }

enough_memory() {
  local need_mib=$1 available_kib
  available_kib=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
  (( available_kib / 1024 >= need_mib ))
}

enough_disk() {
  local dir=$1 need_gib=$2 free_kib
  mkdir -p "$dir"
  free_kib=$(df -Pk "$dir" | awk 'NR == 2 {print $4}')
  (( free_kib / 1024 / 1024 >= need_gib ))
}

doctor() {
  local lab_dir=$1 fleet=$2 min_free_gib=$3 kind_config=$4
  local missing=0 need_mib tool

  for tool in kind kubectl talosctl yq jq; do
    check "$tool" "mise install (the version is pinned in mise.toml)" have "$tool" || missing=$((missing + 1))
  done
  check "curl" "sudo apt-get install -y curl" have curl || missing=$((missing + 1))
  check "docker daemon answers" \
    "install docker (https://docs.docker.com/engine/install/), start it, and add yourself to its group: sudo usermod -aG docker \$USER" \
    docker info || missing=$((missing + 1))
  check "kind cluster definition $kind_config" \
    "point PK_LAB_KIND_CONFIG at a kind config file, or unset it to use lab/kind.yaml" \
    test -r "$kind_config" || missing=$((missing + 1))
  if [[ $kind_config == "$ROOT/lab/kind.yaml" ]]; then
    # The kubelet of Kubernetes 1.35 and later refuses to start on cgroup v1,
    # and kind then fails after a minute with a kubeadm stack trace that does
    # not mention cgroups at all. Somebody who brings their own kind config
    # has chosen their node image and is not second-guessed.
    check "cgroup v2 (the Kubernetes 1.36 in the kind node needs it)" \
      "boot with systemd.unified_cgroup_hierarchy=1, or run an older Kubernetes: copy lab/kind.yaml, set image: kindest/node:v1.34.0, and point PK_LAB_KIND_CONFIG at the copy" \
      cgroup_v2 || missing=$((missing + 1))
  fi
  check "/dev/kvm is usable by $(id -un)" \
    "enable virtualization in the firmware setup, load the module (sudo modprobe kvm_intel || sudo modprobe kvm_amd) and join the group: sudo usermod -aG kvm \$USER (then log in again)" \
    kvm_usable || missing=$((missing + 1))
  check "qemu-system-x86_64 and qemu-img" "sudo apt-get install -y qemu-system-x86 qemu-utils" \
    qemu_present || missing=$((missing + 1))
  check "OVMF (UEFI firmware for the machines)" \
    "sudo apt-get install -y ovmf, or point PK_METAL_OVMF_CODE and PK_METAL_OVMF_VARS at a firmware pair" \
    ovmf_present || missing=$((missing + 1))
  check "ip and iptables" "sudo apt-get install -y iproute2 iptables" \
    net_tools_present || missing=$((missing + 1))
  check "sudo (the lab network needs root)" \
    "run from a terminal where sudo can ask for the password, or configure passwordless sudo for ip and iptables" \
    sudo_usable || missing=$((missing + 1))

  if have yq; then
    # The machines' memory plus room for the kind node and the fleet process.
    need_mib=$(($(fleet_memory_mib "$fleet") + 3072))
    check "$need_mib MiB of available memory" \
      "close something, or give the machines less in $fleet (qemu.memoryMiB; a Talos control plane wants 2048, a worker 1024)" \
      enough_memory "$need_mib" || missing=$((missing + 1))
  fi
  check "$min_free_gib GiB free under $lab_dir" \
    "free some space, move the lab (PK_LAB_DIR=/path/with/space), or lower the bar: PK_LAB_MIN_FREE_GIB=10" \
    enough_disk "$lab_dir" "$min_free_gib" || missing=$((missing + 1))

  (( missing == 0 )) || die "$missing prerequisite(s) missing, see the fix under each MISS above."
  echo "the workstation has what the lab needs" >&2
}

# ---------------------------------------------------------------- up, in parts

network_up() {
  local root=$1 fleet=$2 taps
  step "lab network"
  mapfile -t taps < <(fleet_taps "$fleet")
  "$root/lab/net.sh" up "${taps[@]}"
}

cluster_up() {
  local lab_dir=$1 cluster=$2 kind_config=$3
  step "kind cluster $cluster"
  mkdir -p "$lab_dir"
  if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
    echo "  exists, reusing it" >&2
    kind export kubeconfig --name "$cluster" --kubeconfig "$lab_dir/kubeconfig" >&2
  else
    kind create cluster --name "$cluster" --config "$kind_config" --kubeconfig "$lab_dir/kubeconfig" --wait 180s >&2 \
      || die "kind could not create the cluster. Its own message above says why; a leftover
  from an earlier run is removed with: mise run lab:down"
  fi
  chmod 600 "$lab_dir/kubeconfig"
}

# Generates <lab_dir>/deploy: a kustomization on top of metal/config/lab that
# adds what is only known now. One `kubectl apply -k` then carries everything,
# and because the ledger ConfigMap's name follows its content, a changed fleet
# rolls the manager without anybody restarting it.
write_deploy_dir() {
  local root=$1 lab_dir=$2 fleet=$3 image=$4
  local dir=$lab_dir/deploy base
  mkdir -p "$dir"
  chmod 700 "$dir"

  # The cluster secrets bundle: generated here, on the person's workstation, by
  # the person's own talosctl, and handed to the operator as the Secret the
  # TalosCluster's secretsRef names. The operator has no code path that makes
  # one. It is generated once and kept, so that machines installed before a
  # `down` still belong to the cluster after the next `up`.
  if [[ ! -s $dir/secrets.yaml ]]; then
    echo "  generating a Talos secrets bundle (kept in $dir)" >&2
    (umask 077 && talosctl gen secrets --output-file "$dir/secrets.yaml" >&2) \
      || die "talosctl could not generate a secrets bundle. Check the tool: talosctl version --client"
  fi

  {
    echo "# Derived from the fleet definition (${fleet##*/}) by $SCRIPT. Do not edit."
    fleet_ledger "$fleet"
  } > "$dir/ledger.yaml"

  base=$(realpath --relative-to="$dir" "$root/metal/config/lab")
  cat > "$dir/kustomization.yaml" <<EOF
# Generated by $SCRIPT. Do not edit; rerun: mise run lab:load
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - $base
images:
  - name: pk-metal
    newName: ${image%:*}
    newTag: ${image##*:}
configMapGenerator:
  - name: pk-metal-ledger
    namespace: $NAMESPACE
    behavior: replace
    files:
      - ledger.yaml
secretGenerator:
  - name: $BUNDLE_SECRET
    namespace: $NAMESPACE
    files:
      - secrets.yaml
    options:
      # The TalosCluster names this Secret in its secretsRef, which is not a
      # reference kustomize knows how to rewrite.
      disableNameSuffixHash: true
EOF
}

deploy() {
  local root=$1 lab_dir=$2 fleet=$3 cluster=$4
  local image
  step "operator image"
  image=$("$root/metal/hack/image.sh" build)
  kind load docker-image "$image" --name "$cluster" >&2 \
    || die "kind could not load $image into the cluster. Is the cluster up? mise run lab:up"

  step "operator deployment ($image)"
  write_deploy_dir "$root" "$lab_dir" "$fleet" "$image"
  # Server-side apply: the CRDs are larger than the annotation client-side
  # apply would have to store them in.
  kubectl apply --server-side --force-conflicts -k "$lab_dir/deploy" >&2
  kubectl wait --for=condition=Established crd --all --timeout=60s >&2
  kubectl -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout=180s >&2 \
    || die "the manager did not become ready. Its log says why:
  kubectl --kubeconfig $lab_dir/kubeconfig -n $NAMESPACE logs deployment/$DEPLOYMENT"
}

# Succeeds when the pid file names a live fleet process started from this
# repository. The command line is compared, not just the pid: a pid file outlives
# its process, and the number may belong to something else by now.
fleet_running() {
  local lab_dir=$1 binary=$2 pid
  [[ -s $lab_dir/fleet.pid ]] || return 1
  pid=$(<"$lab_dir/fleet.pid")
  [[ $pid =~ ^[0-9]+$ && -r /proc/$pid/cmdline ]] || return 1
  [[ $(tr '\0' ' ' <"/proc/$pid/cmdline") == "$binary sim fleet "* ]]
}

fleet_up() {
  local root=$1 lab_dir=$2 fleet=$3
  local binary=$root/metal/bin/pk-metal listen deadline pid
  step "simulated fleet"
  [[ -x $binary ]] || die "$binary is missing. Build it: mise run build"
  mkdir -p "$lab_dir"
  if fleet_running "$lab_dir" "$binary"; then
    echo "  already running (pid $(<"$lab_dir/fleet.pid"))" >&2
  else
    # Its own session, so it outlives this script and the terminal; started in
    # the repository root, which the relative paths in the fleet file refer to.
    (
      cd "$root"
      setsid nohup "$binary" sim fleet --config "$fleet" >>"$lab_dir/fleet.log" 2>&1 </dev/null &
      echo $! >"$lab_dir/fleet.pid"
    )
    echo "  started (pid $(<"$lab_dir/fleet.pid"), log $lab_dir/fleet.log)" >&2
  fi

  # The outcome guard: a started process is not a listening BMC. Wait for every
  # endpoint to answer its service root, and stop waiting when the process dies.
  # Liveness is asked of the pid and not of fleet_running: for a moment after the
  # start, the pid still runs setsid or nohup, which exec into pk-metal later.
  deadline=$((SECONDS + 60))
  pid=$(<"$lab_dir/fleet.pid")
  while read -r listen; do
    until curl --silent --insecure --fail --max-time 3 --output /dev/null "https://$listen/redfish/v1"; do
      kill -0 "$pid" 2>/dev/null || die "the fleet process exited. Its last words:
$(tail -n 20 "$lab_dir/fleet.log" 2>/dev/null)
  Full log: $lab_dir/fleet.log"
      (( SECONDS < deadline )) || die "the BMC at https://$listen/redfish/v1 does not answer after 60s.
  Is the lab network up (lab/net.sh status)? Log: $lab_dir/fleet.log"
      sleep 1
    done
    echo "  BMC https://$listen answers" >&2
  done < <(fleet_listens "$fleet")
}

fleet_down() {
  local root=$1 lab_dir=$2
  local binary=$root/metal/bin/pk-metal pid waited=0
  if ! fleet_running "$lab_dir" "$binary"; then
    rm -f "$lab_dir/fleet.pid"
    return 0
  fi
  pid=$(<"$lab_dir/fleet.pid")
  echo "  stopping the fleet (pid $pid)" >&2
  kill "$pid"
  # The fleet powers its machines off on the way out; give it time before
  # insisting, or a disk image is left half written.
  while kill -0 "$pid" 2>/dev/null; do
    if (( waited >= 30 )); then
      echo "  it did not stop within 30s, killing it" >&2
      kill -KILL "$pid" 2>/dev/null || true
      break
    fi
    sleep 1
    waited=$((waited + 1))
  done
  rm -f "$lab_dir/fleet.pid"
}

manifests_apply() {
  local root=$1
  step "lab manifests"
  # As a platform admin: the claim policy lets only that group give a claim an
  # OS intent. kind's own admin is not in it, so impersonate a member.
  kubectl apply --server-side --as=lab-admin --as-group=pk-metal:admins \
    --as-group=system:authenticated -f "$root/lab/manifests/" >&2
}

up() {
  local root=$1 lab_dir=$2 fleet=$3 cluster=$4 min_free_gib=$5 kind_config=$6
  doctor "$lab_dir" "$fleet" "$min_free_gib" "$kind_config"
  network_up "$root" "$fleet"
  cluster_up "$lab_dir" "$cluster" "$kind_config"
  deploy "$root" "$lab_dir" "$fleet" "$cluster"
  fleet_up "$root" "$lab_dir" "$fleet"
  manifests_apply "$root"
  cat >&2 <<EOF

The lab is up. The operator now enrolls the machines and proposes to boot them.
  watch it:      mise run lab:status
  let it happen: mise run lab:approve     (the one human step)
  afterwards:    mise run lab:kubeconfig
EOF
}

# ---------------------------------------------------------------- day to day

require_cluster() {
  local lab_dir=$1
  [[ -s $lab_dir/kubeconfig ]] || die "the lab is not up ($lab_dir/kubeconfig is missing). Bring it up: mise run lab:up"
}

status() {
  local root=$1 lab_dir=$2
  require_cluster "$lab_dir"
  "$root/metal/bin/pk-metal" status
  echo
  kubectl get assets,managementendpoints,hosts,talosclusters,talosnodes,bootassets
  echo
  kubectl get hostclaims,hostoperations --all-namespaces
}

# Prints "<namespace> <name>" for every operation that waits for a person: the
# approval is required, nobody has given it, and the operation has not finished.
pending_operations() {
  kubectl get hostoperations --all-namespaces --output json | jq -r '
    .items[]
    | select(.spec.approval.required != false)
    | select((.spec.approval.approvedBy // "") == "")
    | select((.status.phase // "") != "Succeeded" and (.status.phase // "") != "Failed")
    | "\(.metadata.namespace) \(.metadata.name)"'
}

# Approves every pending operation as the kubeconfig's user and prints how many
# that were. An approval that fails is reported by pk-metal and not counted.
approve_pending() {
  local root=$1
  local namespace name count=0
  while read -r namespace name; do
    [[ -n $name ]] || continue
    if "$root/metal/bin/pk-metal" approve -n "$namespace" "$name" >&2; then
      count=$((count + 1))
    fi
  done < <(pending_operations)
  echo "$count"
}

approve() {
  local root=$1 lab_dir=$2
  local count
  require_cluster "$lab_dir"
  count=$(approve_pending "$root")
  if (( count == 0 )); then
    echo "no operation waits for approval" >&2
  else
    echo "approved $count operation(s)" >&2
  fi
}

# Prints the decoded value of a Secret: the named key, or the only key when the
# Secret has exactly one.
secret_value() {
  local namespace=$1 name=$2 key=$3
  kubectl -n "$namespace" get secret "$name" --output json | jq -r --arg key "$key" '
    .data
    | if has($key) then .[$key]
      elif length == 1 then .[]
      else error("secret has no key \($key) and more than one other")
      end' | base64 --decode
}

# Writes a Secret's value to a file only the owner can read. The file appears
# complete or not at all: callers poll, and half a kubeconfig is worse than none.
# Every step is checked by hand because callers run this under `||`, where the
# shell's errexit does not apply.
secret_to_file() {
  local namespace=$1 name=$2 key=$3 file=$4
  if (umask 077 && secret_value "$namespace" "$name" "$key" >"$file.tmp") 2>/dev/null && [[ -s $file.tmp ]]; then
    mv "$file.tmp" "$file"
  else
    rm -f "$file.tmp"
    return 1
  fi
}

# Writes the workload cluster's kubeconfig and talosconfig into the lab
# directory. Returns non-zero, quietly, while the operator has not published
# them yet: wait() polls this.
kubeconfig_fetch() {
  local lab_dir=$1 cluster_name=$2
  local refs kube_ns kube_name talos_ns talos_name
  refs=$(kubectl get taloscluster "$cluster_name" --output json 2>/dev/null | jq -r '
    [.status.kubeconfigSecretRef.namespace // "", .status.kubeconfigSecretRef.name // "",
     .status.talosconfigSecretRef.namespace // "", .status.talosconfigSecretRef.name // ""]
    | @tsv') || return 1
  IFS=$'\t' read -r kube_ns kube_name talos_ns talos_name <<<"$refs"
  [[ -n $kube_name ]] || return 1
  secret_to_file "${kube_ns:-$NAMESPACE}" "$kube_name" kubeconfig "$lab_dir/workload.kubeconfig" || return 1
  if [[ -n $talos_name ]]; then
    secret_to_file "${talos_ns:-$NAMESPACE}" "$talos_name" talosconfig "$lab_dir/talosconfig" || return 1
  fi
}

kubeconfig() {
  local lab_dir=$1 cluster_name=$2
  require_cluster "$lab_dir"
  kubeconfig_fetch "$lab_dir" "$cluster_name" \
    || die "TalosCluster $cluster_name has not published a kubeconfig yet. It does once the first
  control plane is bootstrapped; see where things stand: mise run lab:status"
  cat >&2 <<EOF
wrote $lab_dir/workload.kubeconfig and $lab_dir/talosconfig
  export KUBECONFIG=$lab_dir/workload.kubeconfig
  export TALOSCONFIG=$lab_dir/talosconfig
EOF
}

# One line that says where the lab stands; wait() prints it when it changes.
progress() {
  local lab_dir=$1
  local hosts operations held nodes="-"
  hosts=$(kubectl get hosts --output json | jq -r '
    [.items[] | .status.phase // "Unknown"] | group_by(.) | map("\(length) \(.[0])") | join(", ")')
  # A failed operation is the usual reason a host stops moving, and nothing
  # else on this line would show it.
  operations=$(kubectl get hostoperations --all-namespaces --output json | jq -r '
    [.items[] | .status.phase // "Unknown"] | group_by(.) | map("\(length) \(.[0])") | join(", ")')
  # A claim the operator holds proposes nothing, so the hosts and operations
  # above stand still without saying why. The hold reason says it.
  held=$(kubectl get hostclaims --all-namespaces --output json | jq -r '
    [.items[] | .status.blockedOn // empty] | group_by(.) | map("\(length) \(.[0])") | join(", ")')
  if [[ -s $lab_dir/workload.kubeconfig ]]; then
    nodes=$(kubectl --kubeconfig "$lab_dir/workload.kubeconfig" --request-timeout=5s get nodes --output json 2>/dev/null | jq -r '
      [.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length') || nodes="-"
  fi
  echo "hosts: ${hosts:-none}; operations: ${operations:-none}; claims held: ${held:-none}; workload nodes Ready: $nodes"
}

converged() {
  local lab_dir=$1 want=$2
  local provisioned ready
  provisioned=$(kubectl get hosts --output json | jq '[.items[] | select(.status.phase == "Provisioned")] | length')
  (( provisioned == want )) || return 1
  [[ -s $lab_dir/workload.kubeconfig ]] || return 1
  ready=$(kubectl --kubeconfig "$lab_dir/workload.kubeconfig" --request-timeout=5s get nodes --output json 2>/dev/null | jq '
    [.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length') || return 1
  (( ready == want ))
}

# The unattended run: stands in for the person by approving whatever is
# proposed, and holds until the lab has done what it is for.
wait_converged() {
  local root=$1 lab_dir=$2 fleet=$3 cluster_name=$4 timeout=$5
  local want line last="" deadline=$((SECONDS + timeout))
  require_cluster "$lab_dir"
  want=$(fleet_count "$fleet")
  step "waiting up to ${timeout}s for $want Provisioned hosts and $want Ready nodes"
  while true; do
    approve_pending "$root" >/dev/null || true
    kubeconfig_fetch "$lab_dir" "$cluster_name" || true
    if converged "$lab_dir" "$want"; then
      echo "converged after $((SECONDS - (deadline - timeout)))s: $(progress "$lab_dir")" >&2
      return 0
    fi
    line=$(progress "$lab_dir" 2>/dev/null || echo "the lab cluster does not answer")
    if [[ $line != "$last" ]]; then
      echo "  [$(date +%H:%M:%S)] $line" >&2
      last=$line
    fi
    (( SECONDS < deadline )) || die "the lab did not converge within ${timeout}s. Last state: $line
  Look at it: mise run lab:status; collect the evidence: mise run lab:collect;
  or wait longer: PK_LAB_WAIT_TIMEOUT=3600 mise run lab:wait"
    sleep 15
  done
}

# Everything worth reading after a failed run, in one directory that is safe to
# publish: object dumps without Secret contents, logs, the machines' consoles.
# The secrets bundle, the kubeconfigs, the disks and the image cache stay out.
collect() {
  local root=$1 lab_dir=$2 fleet=$3
  local out=$lab_dir/diagnostics state_dir log
  rm -rf "$out"
  mkdir -p "$out"
  step "collecting diagnostics into $out"

  "$root/lab/net.sh" status >"$out/network.txt" 2>&1 || true
  cp "$lab_dir/fleet.log" "$out/" 2>/dev/null || true
  state_dir=$(fleet_state_dir "$root" "$fleet")
  if [[ -d $state_dir ]]; then
    while IFS= read -r -d '' log; do
      mkdir -p "$out/machines/$(basename "$(dirname "$log")")"
      cp "$log" "$out/machines/$(basename "$(dirname "$log")")/"
    done < <(find "$state_dir" -type f -name '*.log' -print0)
  fi

  if [[ -s $lab_dir/kubeconfig ]]; then
    "$root/metal/bin/pk-metal" status >"$out/status.txt" 2>&1 || true
    kubectl get assets,managementendpoints,hosts,talosclusters,talosnodes,bootassets --output wide >"$out/get.txt" 2>&1 || true
    kubectl get hostclaims,hostoperations --all-namespaces --output wide >>"$out/get.txt" 2>&1 || true
    kubectl get assets,managementendpoints,hosts,talosclusters,talosnodes,bootassets --output yaml >"$out/objects.yaml" 2>&1 || true
    kubectl get hostclaims,hostoperations --all-namespaces --output yaml >>"$out/objects.yaml" 2>&1 || true
    kubectl get events --all-namespaces --sort-by=.lastTimestamp >"$out/events.txt" 2>&1 || true
    kubectl -n "$NAMESPACE" describe deployment,pod,service,configmap >"$out/operator-describe.txt" 2>&1 || true
    # Names and types only: a describe of a Secret shows sizes, never values.
    kubectl -n "$NAMESPACE" get secrets >"$out/secrets-list.txt" 2>&1 || true
    kubectl -n "$NAMESPACE" logs "deployment/$DEPLOYMENT" --all-containers --tail=-1 >"$out/operator.log" 2>&1 || true
    kubectl -n "$NAMESPACE" logs "deployment/$DEPLOYMENT" --all-containers --previous --tail=-1 >"$out/operator-previous.log" 2>&1 || true
  fi
  if [[ -s $lab_dir/workload.kubeconfig ]]; then
    kubectl --kubeconfig "$lab_dir/workload.kubeconfig" --request-timeout=10s get nodes,pods --all-namespaces --output wide >"$out/workload.txt" 2>&1 || true
  fi
  echo "diagnostics in $out" >&2
}

down() {
  local root=$1 lab_dir=$2 cluster=$3
  step "lab down"
  fleet_down "$root" "$lab_dir"
  if have kind && kind get clusters 2>/dev/null | grep -qx "$cluster"; then
    kind delete cluster --name "$cluster" >&2
  fi
  rm -f "$lab_dir/kubeconfig" "$lab_dir/workload.kubeconfig" "$lab_dir/talosconfig"
  "$root/lab/net.sh" down
  echo "kept: the machines' disks, the image cache and the secrets bundle. Wipe the disks with: mise run lab:reset" >&2
}

reset() {
  local root=$1 lab_dir=$2 fleet=$3
  local state_dir
  fleet_running "$lab_dir" "$root/metal/bin/pk-metal" \
    && die "the fleet is running and has the disks open. Take the lab down first: mise run lab:down"
  state_dir=$(fleet_state_dir "$root" "$fleet")
  # The directory comes out of a file somebody edits. Refuse anything that is
  # not clearly a lab state directory before removing it recursively.
  case "$state_dir" in
    "$root"/?* | "$lab_dir"/?*) ;;
    *) die "stateDir resolves to $state_dir, which is neither under $root nor under $lab_dir.
  Remove it by hand if that is really where the lab's disks are." ;;
  esac
  rm -rf "$state_dir"
  echo "wiped $state_dir. The next lab:up installs every machine from scratch." >&2
}

main() {
  local verb=${1:-}
  case "$verb" in
    doctor) doctor "$LAB_DIR" "$FLEET" "$MIN_FREE_GIB" "$KIND_CONFIG" ;;
    up) up "$ROOT" "$LAB_DIR" "$FLEET" "$CLUSTER" "$MIN_FREE_GIB" "$KIND_CONFIG" ;;
    network) network_up "$ROOT" "$FLEET" ;;
    cluster) cluster_up "$LAB_DIR" "$CLUSTER" "$KIND_CONFIG" ;;
    deploy)
      require_cluster "$LAB_DIR"
      deploy "$ROOT" "$LAB_DIR" "$FLEET" "$CLUSTER"
      ;;
    fleet) fleet_up "$ROOT" "$LAB_DIR" "$FLEET" ;;
    manifests)
      require_cluster "$LAB_DIR"
      manifests_apply "$ROOT"
      ;;
    status) status "$ROOT" "$LAB_DIR" ;;
    approve) approve "$ROOT" "$LAB_DIR" ;;
    kubeconfig) kubeconfig "$LAB_DIR" "$TALOS_CLUSTER" ;;
    wait) wait_converged "$ROOT" "$LAB_DIR" "$FLEET" "$TALOS_CLUSTER" "$WAIT_TIMEOUT" ;;
    collect) collect "$ROOT" "$LAB_DIR" "$FLEET" ;;
    down) down "$ROOT" "$LAB_DIR" "$CLUSTER" ;;
    reset) reset "$ROOT" "$LAB_DIR" "$FLEET" ;;
    -h | --help | help) usage ;;
    *)
      usage
      exit 1
      ;;
  esac
}

main "$@"
