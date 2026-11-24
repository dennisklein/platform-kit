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
  render       render the lab from its spec into <lab directory>/generated
  up           render, network, kind cluster, operator, simulated fleet, lab manifests
  status       the rack as the operator sees it, then every kind
  approve      approve every operation that waits for a person
  apply [path...]
               kubectl apply as a member of the admin group (pk-metal:admins), which
               the claim policy asks for; default: the rendered manifests
  kubeconfig   write the workload cluster's kubeconfig and talosconfig into the lab directory
  wait         approve and wait until every Host is Provisioned and every node Ready
  release <namespace/name>
               delete a claim, approve the shutdown it proposes, and wait until its
               Host is Available again
  day2         on a converged lab: reboot a worker and a control plane with pk-metal
               reboot, approved by a second user, and check each came back
  pivot        move the operator from kind onto the first Talos cluster (pk-metal
               pivot), delete kind, and install the second cluster from there;
               needs a workload section (PK_LAB_WORKLOAD) and a converged lab
  backup-restore
               day 2: back the first cluster's etcd up to the operator's backup
               volume, wipe etcd on every control plane, and restore it through
               pk-metal restore and an approved etcdRecover (needs a converged lab)
  collect      write logs and object dumps to <lab directory>/diagnostics
  down         stop the fleet, delete the kind cluster, remove the network; keeps disks and caches
  reset        wipe the machines' disks and firmware state (the lab must be down)

  parts of up, for a partial rerun (they use what up rendered last):
  network | cluster | deploy | fleet | manifests

environment variables:
  PK_LAB_DIR            where the lab keeps its state: kubeconfig, logs, generated
                        deployment, secrets bundle (default: <repo>/.lab)
  PK_LAB_SPEC           the lab spec (default: <repo>/lab/lab.yaml)
  PK_LAB_NODES          the lab's size as <control planes>cp+<workers>w, e.g. 3cp+2w
                        (default: the counts in the spec); read by doctor, render and up
  PK_LAB_WORKLOAD       a second cluster for the pivot round, as <cp>cp+<w>w, e.g. 1cp+0w
                        (default: the spec's workload section, usually none)
  PK_LAB_WAIT_TIMEOUT   seconds wait gives the lab to converge (default: 2400)
  PK_LAB_RELEASE_TIMEOUT
                        seconds release gives a machine to come back (default: 900)
  PK_LAB_DAY2_TIMEOUT   seconds day2 gives each reboot (default: 1200)
  PK_LAB_MIN_FREE_GIB   free disk doctor asks for under PK_LAB_DIR (default: 15)
  PK_LAB_KIND_CONFIG    the kind cluster definition (default: <repo>/lab/kind.yaml);
                        a copy with another node image is how a host still on
                        cgroup v1 runs the lab, see lab/README.md"

ROOT=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lab/distro.sh
source "$ROOT/lab/distro.sh"
LAB_DIR=$(realpath -m "${PK_LAB_DIR:-$ROOT/.lab}")
SPEC=$(realpath -m "${PK_LAB_SPEC:-$ROOT/lab/lab.yaml}")
SIZE=${PK_LAB_NODES:-}
WORKLOAD=${PK_LAB_WORKLOAD:-}
# Everything rendered from the spec. Generated, never edited, never committed.
GEN=$LAB_DIR/generated
FLEET=$GEN/fleet.yaml
WAIT_TIMEOUT=${PK_LAB_WAIT_TIMEOUT:-2400}
RELEASE_TIMEOUT=${PK_LAB_RELEASE_TIMEOUT:-900}
DAY2_TIMEOUT=${PK_LAB_DAY2_TIMEOUT:-1200}
MIN_FREE_GIB=${PK_LAB_MIN_FREE_GIB:-15}
KIND_CONFIG=${PK_LAB_KIND_CONFIG:-$ROOT/lab/kind.yaml}

# Fixed names, shared with files that cannot read a variable: lab/kind.yaml names
# the kind cluster, the manifests under metal/config name the namespace and the
# Deployment. The TalosCluster's name and the Secret its secretsRef points at
# come from the spec, through the rendered lab.env.
CLUSTER=pk-metal
NAMESPACE=pk-metal
DEPLOYMENT=pk-metal-manager
# The pivot round: the workstation's registry the first cluster pulls the
# operator image from, and the file that says the operator has moved.
REGISTRY_CONTAINER=pk-metal-lab-registry
REGISTRY_IMAGE=registry:2.8.3
PIVOT_MARK=pivoted

# Every kubectl, kind and pk-metal call below talks to the lab's own kubeconfig.
# The person's ~/.kube/config is never read and never written.
export KUBECONFIG=$LAB_DIR/kubeconfig

usage() { echo "$HELP" >&2; }

die() {
  echo "Error: $*" >&2
  exit 1
}

step() { echo "== $*" >&2; }

# The file fleet_up leaves in the fleet's state directory. reset removes a
# directory outside the lab directory only when it carries this mark.
STATE_MARK=.pk-metal-lab-state

# ---------------------------------------------------------------- the spec

# Renders the lab from its spec into a directory: the fleet file, the manifests,
# the taps, the ledger and lab.env. The work is done in Go (metal/hack/labrender
# and metal/internal/lab), where it is tested; this only runs it.
render_into() {
  local root=$1 spec=$2 size=$3 workload=$4 out=$5
  [[ -r $spec ]] || die "the lab spec $spec is not readable. Point PK_LAB_SPEC at one, or unset it to use lab/lab.yaml"
  have go || die "go is not on the PATH. Run the lab through mise (mise run lab:up), which puts it there"
  mkdir -p "$out"
  (cd "$root/metal" && go run ./hack/labrender --spec "$spec" --out "$out" --nodes "$size" --workload "$workload" --source "${spec#"$root"/}") \
    || die "the lab spec does not render. The message above names what to fix in $spec${size:+ or in PK_LAB_NODES=$size}${workload:+ or in PK_LAB_WORKLOAD=$workload}"
}

render() {
  local root=$1 spec=$2 size=$3 workload=$4 gen=$5
  step "render $spec${size:+ at size $size} into $gen"
  render_into "$root" "$spec" "$size" "$workload" "$gen"
}

# The parts of up and the later verbs work on what up rendered last, so that a
# lab brought up at one size is not resized under its feet by a command run
# without PK_LAB_NODES. Only a lab directory with nothing rendered yet is
# rendered here.
require_rendered() {
  local root=$1 spec=$2 size=$3 workload=$4 gen=$5
  [[ -s $gen/lab.env ]] || render "$root" "$spec" "$size" "$workload" "$gen"
}

# Prints one value of a rendered lab.env. The values are names and numbers the
# renderer validated; the file is read, never sourced.
lab_value() {
  local gen=$1 key=$2 line
  [[ -r $gen/lab.env ]] || die "$gen/lab.env is missing. Render the lab: mise run lab:render"
  while IFS= read -r line; do
    if [[ $line == "$key="* ]]; then
      echo "${line#*=}"
      return 0
    fi
  done <"$gen/lab.env"
  die "$gen/lab.env has no $key. Render the lab again: mise run lab:render"
}

# Prints one value of a rendered lab.env, or the default when the file has no
# such key: the keys of the second cluster are there only when it is.
lab_value_or() {
  local gen=$1 key=$2 default=$3
  if grep -q "^$key=" "$gen/lab.env" 2>/dev/null; then
    lab_value "$gen" "$key"
  else
    echo "$default"
  fi
}

# ---------------------------------------------------------------- fleet file

fleet_query() {
  local fleet=$1 expression=$2
  [[ -r $fleet ]] || die "$fleet is not readable. Render the lab: mise run lab:render"
  yq -r "$expression" "$fleet"
}

# The expression stays inside what jq and yq agree on, so the answer does not
# depend on which of the two yq implementations is on the PATH.
fleet_listens() { fleet_query "$1" '.machines[].listen'; }

# The directory the fleet keeps disks and firmware state in. Relative paths in
# the fleet file are relative to the repository root, which is where `fleet`
# starts the process.
fleet_state_dir() {
  local root=$1 fleet=$2 dir
  dir=$(fleet_query "$fleet" '.stateDir')
  [[ -n $dir && $dir != null ]] || die "$fleet has no stateDir. Render the lab again: mise run lab:render"
  case "$dir" in
    /*) echo "$dir" ;;
    *) echo "$root/$dir" ;;
  esac
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

# A line of information in doctor's output: neither a pass nor a miss.
note() { echo "note  $*" >&2; }

kvm_usable() { [[ -r /dev/kvm && -w /dev/kvm ]]; }

net_tools_present() { have ip && have iptables; }

# The fleet runs $PK_METAL_QEMU, else qemu-system-x86_64 from the PATH, else
# /usr/libexec/qemu-kvm, where RHEL and its rebuilds install QEMU.
qemu_present() {
  if [[ -n "${PK_METAL_QEMU:-}" ]]; then [[ -x "$PK_METAL_QEMU" ]]; return; fi
  have qemu-system-x86_64 || [[ -x /usr/libexec/qemu-kvm ]]
}

qemu_fix() {
  if [[ -n "${PK_METAL_QEMU:-}" ]]; then
    echo "PK_METAL_QEMU=$PK_METAL_QEMU is not an executable file; point it at the QEMU system emulator or unset it"
  else
    install_hint "qemu-system-x86 qemu-utils" "qemu-kvm qemu-img" "qemu-system-x86-core qemu-img"
  fi
}

# The firmware pairs the QEMU driver probes (metal/internal/machine/qemu), in
# its order: Fedora and RHEL (edk2-ovmf), Debian and Ubuntu (ovmf), Arch.
OVMF_PAIRS=(
  "/usr/share/edk2/ovmf/OVMF_CODE.fd /usr/share/edk2/ovmf/OVMF_VARS.fd"
  "/usr/share/OVMF/OVMF_CODE.fd /usr/share/OVMF/OVMF_VARS.fd"
  "/usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_VARS_4M.fd"
  "/usr/share/edk2-ovmf/x64/OVMF_CODE.fd /usr/share/edk2-ovmf/x64/OVMF_VARS.fd"
)

ovmf_present() {
  local pair code vars
  if [[ -n ${PK_METAL_OVMF_CODE:-} ]]; then
    [[ -r $PK_METAL_OVMF_CODE && -r ${PK_METAL_OVMF_VARS:-} ]]
    return
  fi
  for pair in "${OVMF_PAIRS[@]}"; do
    read -r code vars <<<"$pair"
    [[ -r $code && -r $vars ]] && return 0
  done
  return 1
}

# The lab is built on Docker Engine on the host itself: kind's node sits on a
# host bridge the lab network routes to, and lab/net.sh puts its forwarding
# rules into Docker's DOCKER-USER chain. Podman (also behind the podman-docker
# shim, where `docker` is podman) and Docker Desktop (the engine runs in a VM)
# answer `docker info` as well, but neither gives that.
docker_is_engine() {
  local version os
  version=$(docker version 2>/dev/null) || return 1
  grep -qi podman <<<"$version" && return 1
  os=$(docker info --format '{{.OperatingSystem}}' 2>/dev/null) || return 1
  [[ $os != *"Docker Desktop"* ]]
}

# Rootless Docker runs the kind node in a user namespace behind a userspace
# network: the node's traffic does not come from a host bridge and there is no
# DOCKER-USER chain in the host's firewall. Nobody has run the lab that way.
docker_is_rootful() {
  local options
  options=$(docker info --format '{{.SecurityOptions}}' 2>/dev/null) || return 1
  [[ $options != *rootless* ]]
}

# The proxy settings the lab's processes inherit, if any.
proxy_in_use() { [[ -n ${HTTPS_PROXY:-}${https_proxy:-}${HTTP_PROXY:-}${http_proxy:-} ]]; }

# Succeeds when NO_PROXY sends the lab network past the proxy. The simulated
# BMCs fetch boot media from https://10.77.0.1:30443 and honour HTTPS_PROXY,
# and so does curl in this script; a proxy cannot reach the lab bridge. Go and
# curl (7.86 and later) both read CIDR entries.
no_proxy_covers_lab() {
  local entry
  local -a entries
  IFS=, read -r -a entries <<<"${NO_PROXY:-},${no_proxy:-}"
  for entry in "${entries[@]}"; do
    entry=${entry// /}
    case "$entry" in
      10.77.0.0/24 | 10.77.0.0/16 | 10.0.0.0/8 | 10.77.0.1 | '*') return 0 ;;
    esac
  done
  return 1
}

# Any HTTP answer means the host is reachable; only a network or TLS failure
# fails this.
reachable() { curl --silent --output /dev/null --max-time 10 "$1"; }

sudo_usable() { (( EUID == 0 )) || sudo -n true || sudo -v; }

firewalld_active() { have systemctl && systemctl is-active --quiet firewalld 2>/dev/null; }

nftables_service_active() { have systemctl && systemctl is-active --quiet nftables 2>/dev/null; }

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
  local root=$1 lab_dir=$2 spec=$3 size=$4 workload=$5 min_free_gib=$6 kind_config=$7
  local missing=0 need_mib tool preview problem

  for tool in go kind kubectl talosctl yq jq; do
    check "$tool" "mise install (the version is pinned in mise.toml)" have "$tool" || missing=$((missing + 1))
  done
  check "curl" "$(install_hint curl curl)" have curl || missing=$((missing + 1))
  if check "docker daemon answers" \
    "install Docker Engine (https://docs.docker.com/engine/install/), start it (sudo systemctl enable --now docker), and add yourself to its group: sudo usermod -aG docker \$USER, then log in again. Membership of the docker group is as good as root on this host; on a shared machine, decide that on purpose" \
    docker info; then
    check "docker is Docker Engine on this host (not podman, not Docker Desktop)" \
      "the lab needs Docker Engine: kind's node must sit on a host bridge, and lab/net.sh uses Docker's DOCKER-USER chain. Podman and the podman-docker shim are not supported (on RHEL: sudo dnf remove podman-docker, then install Docker Engine from https://docs.docker.com/engine/install/rhel/); Docker Desktop runs the engine in a VM the lab bridge cannot reach" \
      docker_is_engine || missing=$((missing + 1))
    check "docker runs as root (rootless Docker is not supported)" \
      "the lab's network expects the kind node on a host bridge and Docker's rules in the host firewall; rootless Docker gives neither. Use the system Docker Engine: unset DOCKER_HOST, or docker context use default" \
      docker_is_rootful || missing=$((missing + 1))
  else
    missing=$((missing + 1))
  fi
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
  check "QEMU (qemu-system-x86_64, /usr/libexec/qemu-kvm or \$PK_METAL_QEMU)" "$(qemu_fix)" qemu_present || missing=$((missing + 1))
  check "qemu-img" "$(install_hint qemu-utils qemu-img)" have qemu-img || missing=$((missing + 1))
  check "OVMF (UEFI firmware for the machines)" \
    "$(install_hint ovmf edk2-ovmf), or point PK_METAL_OVMF_CODE and PK_METAL_OVMF_VARS at a firmware pair (a CODE file and its VARS template, e.g. under /usr/share/edk2/ovmf or /usr/share/OVMF)" \
    ovmf_present || missing=$((missing + 1))
  check "ip and iptables" "$(install_hint "iproute2 iptables" "iproute iptables-nft")" \
    net_tools_present || missing=$((missing + 1))
  # sudo -v only refreshes sudo's timestamp and runs nothing as root, but it can
  # ask for the password here, in a command that otherwise changes nothing.
  check "sudo (the lab network needs root; this may ask for your password)" \
    "run from a terminal where sudo can ask for the password, or configure passwordless sudo for ip, iptables, sysctl, nft and firewall-cmd" \
    sudo_usable || missing=$((missing + 1))
  if firewalld_active; then
    # firewalld (RHEL, Fedora) keeps its own nftables table, and its reject
    # there wins over the ACCEPT rules lab/net.sh adds through iptables.
    check "firewall-cmd (firewalld is active; lab/net.sh puts pkbr0 into its trusted zone)" \
      "$(install_hint firewalld firewalld)" have firewall-cmd || missing=$((missing + 1))
  fi
  if nftables_service_active; then
    note "nftables.service is active. lab/net.sh up warns if its ruleset drops input or forwarded traffic on pkbr0 by policy."
  fi
  if proxy_in_use; then
    check "NO_PROXY includes the lab network 10.77.0.0/24 (a proxy is set)" \
      "export NO_PROXY=\"\${NO_PROXY:+\$NO_PROXY,}10.77.0.0/24,localhost,127.0.0.1,.local\"; export no_proxy=\"\$NO_PROXY\" (add the kind network too: docker network inspect kind --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}')" \
      no_proxy_covers_lab || missing=$((missing + 1))
    note "a proxy is set. The operator in kind gets it from lab:up; the Talos machines do not: see 'Behind a proxy' in lab/README.md."
  fi
  # The simulated BMCs download installer images from the Talos image factory,
  # and the operator registers its image builds there.
  check "https://factory.talos.dev answers (installer images)" \
    "the lab needs it from this workstation. Behind a proxy, set HTTPS_PROXY and NO_PROXY (lab/README.md, 'Behind a proxy'); without internet, see 'Offline' in docs/metal/concepts-for-sysadmins.md" \
    reachable https://factory.talos.dev/ || missing=$((missing + 1))
  check "https://ghcr.io answers (Talos and Kubernetes images, pulled by the machines through this host's NAT)" \
    "the machines pull from ghcr.io and registry.k8s.io through this host. Behind a proxy, give them one too (lab/README.md, 'Behind a proxy')" \
    reachable https://ghcr.io/ || missing=$((missing + 1))

  # The spec is rendered into a scratch directory: doctor changes nothing, and
  # a lab that is up keeps the files it was rendered from.
  # Its own messages are the fix, so they are shown instead of a generic one.
  # The subshell keeps render_into's die from ending doctor.
  preview=$(mktemp -d)
  if problem=$( (render_into "$root" "$spec" "$size" "$workload" "$preview") 2>&1 ); then
    echo "ok    the lab spec renders${size:+ at size $size}" >&2
    # The machines' memory, summed over the machines this size renders, plus
    # room for the kind node and the fleet process.
    need_mib=$(($(lab_value "$preview" MEMORY_MIB) + 3072))
    check "$need_mib MiB of available memory for $(($(lab_value "$preview" NODES) + $(lab_value_or "$preview" WORKLOAD_NODES 0))) machines" \
      "close something, give the machines less in $spec (memoryMiB; a Talos control plane wants 2048, a worker 1024), or run fewer: PK_LAB_NODES=1cp+1w" \
      enough_memory "$need_mib" || missing=$((missing + 1))
  else
    echo "MISS  the lab spec renders${size:+ at size $size}" >&2
    sed 's/^/      /' <<<"$problem" >&2
    missing=$((missing + 1))
  fi
  rm -rf "$preview"
  check "$min_free_gib GiB free under $lab_dir" \
    "free some space, move the lab (PK_LAB_DIR=/path/with/space), or lower the bar: PK_LAB_MIN_FREE_GIB=10" \
    enough_disk "$lab_dir" "$min_free_gib" || missing=$((missing + 1))

  (( missing == 0 )) || die "$missing prerequisite(s) missing, see the fix under each MISS above."
  echo "the workstation has what the lab needs" >&2
}

# ---------------------------------------------------------------- up, in parts

network_up() {
  local root=$1 gen=$2 taps
  step "lab network"
  mapfile -t taps <"$gen/taps"
  "$root/lab/net.sh" up "${taps[@]}"
}

# The host addresses the kind config publishes ports on, other than "all".
kind_listen_addresses() {
  yq -r '.nodes[] | .extraPortMappings // [] | .[] | .listenAddress // ""' "$1" \
    | grep -v -x -e '' -e '0.0.0.0' -e '::' | sort -u
}

# lab/kind.yaml publishes the media port on the lab bridge address only, so the
# simulated BMCs and the media service do not answer on the workstation's other
# interfaces. Docker can bind that address only while the bridge has it: the
# network comes first, and a node container that stopped while the bridge was
# gone (a reboot) is started again once it is back.
cluster_listen_guard() {
  local kind_config=$1 address
  while read -r address; do
    [[ -n $address ]] || continue
    ip -o addr show | grep -qF " $address/" || die "$kind_config publishes a port on $address, and no interface has that address.
  The lab network gives it to the bridge; bring that up first: mise run lab:up (or: lab/net.sh up)"
  done < <(kind_listen_addresses "$kind_config")
}

cluster_up() {
  local lab_dir=$1 cluster=$2 kind_config=$3 node
  step "kind cluster $cluster"
  mkdir -p "$lab_dir"
  cluster_listen_guard "$kind_config"
  if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
    echo "  exists, reusing it" >&2
    node=$cluster-control-plane
    if [[ $(docker inspect --format '{{.State.Running}}' "$node" 2>/dev/null) == false ]]; then
      echo "  its node container $node is stopped, starting it" >&2
      docker start "$node" >&2 || die "docker could not start $node. Its message above says why; start over with: mise run lab:down && mise run lab:up"
    fi
    kind export kubeconfig --name "$cluster" --kubeconfig "$lab_dir/kubeconfig" >&2
  else
    kind create cluster --name "$cluster" --config "$kind_config" --kubeconfig "$lab_dir/kubeconfig" --wait 180s >&2 \
      || die "kind could not create the cluster. Its own message above says why; a leftover
  from an earlier run is removed with: mise run lab:down"
  fi
  chmod 600 "$lab_dir/kubeconfig"
}

# A YAML string, quoted: the proxy settings are the person's text.
yaml_string() { jq -n --arg v "$1" '$v'; }

# Writes a patch that gives the manager the workstation's proxy, and prints its
# file name; prints nothing, and removes an old patch, when no proxy is set. The
# manager registers image builds with the Talos image factory, which behind a
# proxy only the proxy reaches. The lab network, kind's service and pod networks
# (kind's defaults, lab/kind.yaml changes neither) and the cluster's own names
# must not go through it.
write_proxy_patch() {
  local dir=$1
  local https=${HTTPS_PROXY:-${https_proxy:-}} http=${HTTP_PROXY:-${http_proxy:-}}
  local skip="10.77.0.0/24,10.96.0.0/16,10.244.0.0/16,.svc,.cluster.local,localhost,127.0.0.1"
  local site_skip=${NO_PROXY:-${no_proxy:-}}
  if [[ -z $https$http ]]; then
    rm -f "$dir/proxy.yaml"
    return 0
  fi
  [[ -n $site_skip ]] && skip="$skip,$site_skip"
  {
    echo "# Generated by $SCRIPT from the proxy settings of the shell that ran it."
    echo "apiVersion: apps/v1"
    echo "kind: Deployment"
    echo "metadata:"
    echo "  name: $DEPLOYMENT"
    echo "  namespace: $NAMESPACE"
    echo "spec:"
    echo "  template:"
    echo "    spec:"
    echo "      containers:"
    echo "        - name: manager"
    echo "          env:"
    if [[ -n $https ]]; then
      printf '            - name: HTTPS_PROXY\n              value: %s\n' "$(yaml_string "$https")"
    fi
    if [[ -n $http ]]; then
      printf '            - name: HTTP_PROXY\n              value: %s\n' "$(yaml_string "$http")"
    fi
    printf '            - name: NO_PROXY\n              value: %s\n' "$(yaml_string "$skip")"
  } > "$dir/proxy.yaml"
  echo proxy.yaml
}

# Generates <lab_dir>/deploy: a kustomization on top of metal/config/lab that
# adds what is only known now. One `kubectl apply -k` then carries everything,
# and because the ledger ConfigMap's name follows its content, a changed fleet
# rolls the manager without anybody restarting it.
write_deploy_dir() {
  local root=$1 lab_dir=$2 gen=$3 image=$4
  local dir=$lab_dir/deploy base proxy_patch bundle_secret workload_secret
  bundle_secret=$(lab_value "$gen" BUNDLE_SECRET)
  workload_secret=$(lab_value_or "$gen" WORKLOAD_BUNDLE_SECRET "")
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

  # The second cluster of the pivot round gets its own bundle, made the same
  # way. Its Secret starts on kind and moves with the operator.
  if [[ -n $workload_secret && ! -s $dir/workload-secrets.yaml ]]; then
    echo "  generating a Talos secrets bundle for the second cluster (kept in $dir)" >&2
    (umask 077 && talosctl gen secrets --output-file "$dir/workload-secrets.yaml" >&2) \
      || die "talosctl could not generate a secrets bundle. Check the tool: talosctl version --client"
  fi

  # The ledger the operator joins Assets against: BMC MAC -> the URL its
  # simulator listens on. Rendered with the fleet, so the two cannot disagree.
  cp "$gen/ledger.yaml" "$dir/ledger.yaml"

  base=$(realpath --relative-to="$dir" "$root/metal/config/lab")
  proxy_patch=$(write_proxy_patch "$dir")
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
  - name: $bundle_secret
    namespace: $NAMESPACE
    files:
      - secrets.yaml
    options:
      # The TalosCluster names this Secret in its secretsRef, which is not a
      # reference kustomize knows how to rewrite.
      disableNameSuffixHash: true
EOF
  if [[ -n $workload_secret ]]; then
    cat >> "$dir/kustomization.yaml" <<EOF
  - name: $workload_secret
    namespace: $NAMESPACE
    files:
      - secrets.yaml=workload-secrets.yaml
    options:
      disableNameSuffixHash: true
EOF
  fi
  if [[ -n $proxy_patch ]]; then
    printf 'patches:\n  - path: %s\n' "$proxy_patch" >> "$dir/kustomization.yaml"
    echo "  the manager gets this shell's proxy settings ($dir/$proxy_patch)" >&2
  fi
}

deploy() {
  local root=$1 lab_dir=$2 gen=$3 cluster=$4
  local image
  step "operator image"
  image=$("$root/metal/hack/image.sh" build)
  kind load docker-image "$image" --name "$cluster" >&2 \
    || die "kind could not load $image into the cluster. Is the cluster up? mise run lab:up"

  step "operator deployment ($image)"
  write_deploy_dir "$root" "$lab_dir" "$gen" "$image"
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

# Marks the fleet's state directory as the lab's, for reset. Only a directory
# the lab creates, or one that is empty, gets the mark: a stateDir that points
# at existing content (a typo, a source directory) stays unmarked, and reset
# refuses it.
mark_state_dir() {
  local dir=$1
  if [[ ! -e $dir/$STATE_MARK && -d $dir && -n $(ls -A "$dir") ]]; then
    return 0
  fi
  mkdir -p "$dir"
  echo "The virtual lab's machine disks (stateDir of the rendered fleet). mise run lab:reset removes this directory." \
    >"$dir/$STATE_MARK"
}

fleet_up() {
  local root=$1 lab_dir=$2 fleet=$3
  local binary=$root/metal/bin/pk-metal listen deadline pid
  step "simulated fleet"
  [[ -x $binary ]] || die "$binary is missing. Build it: mise run build"
  mkdir -p "$lab_dir"
  mark_state_dir "$(fleet_state_dir "$root" "$fleet")"
  if fleet_running "$lab_dir" "$binary"; then
    echo "  already running (pid $(<"$lab_dir/fleet.pid"))" >&2
  else
    # What the fleet was started with, for rerender_guard.
    cp "$fleet" "$lab_dir/fleet.running.yaml"
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
    rm -f "$lab_dir/fleet.pid" "$lab_dir/fleet.running.yaml"
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
  rm -f "$lab_dir/fleet.pid" "$lab_dir/fleet.running.yaml"
}

# Applies manifests as a member of the platform admins. The claim policy lets
# only that group give a claim an OS (spec.os) or waive its approval, and kind's
# own admin is not in it, so this impersonates a member: the group
# metal/config/lab binds. A real site binds its own people to the group instead.
admin_apply() {
  local path args=()
  for path in "$@"; do
    [[ -e $path ]] || die "$path does not exist. Name manifest files or directories, e.g.: mise run lab:apply .lab/generated/manifests/30-claims.yaml"
    args+=(-f "$path")
  done
  kubectl apply --server-side --as=lab-admin --as-group=pk-metal:admins \
    --as-group=system:authenticated "${args[@]}" >&2
}

manifests_apply() {
  local gen=$1
  step "lab manifests"
  admin_apply "$gen/manifests/"
}

# A running lab is not resized or given other machines: the fleet reads its
# file once, at the start, and wait, collect and reset go by the files rendered
# for the lab that runs. So when the fleet runs, the spec must still render the
# machines it was started with, before anything under the lab directory is
# rendered anew. Other changes (versions, patches) are fine and are applied.
rerender_guard() {
  local root=$1 lab_dir=$2 spec=$3 size=$4 workload=$5 preview differs=0
  fleet_running "$lab_dir" "$root/metal/bin/pk-metal" || return 0
  # A fleet started some other way has no copy to compare with.
  [[ -e $lab_dir/fleet.running.yaml ]] || return 0
  preview=$(mktemp -d)
  render_into "$root" "$spec" "$size" "$workload" "$preview"
  cmp -s "$preview/fleet.yaml" "$lab_dir/fleet.running.yaml" || differs=1
  rm -rf "$preview"
  (( differs == 0 )) || die "the lab runs other machines than $spec${size:+ at size $size} describes now.
  Take it down first, then bring it up at the new size:
    mise run lab:down && mise run lab:reset && ${size:+PK_LAB_NODES=$size }mise run lab:up
  (or run lab:up with the PK_LAB_NODES it was brought up with)"
}

up() {
  local root=$1 lab_dir=$2 spec=$3 size=$4 workload=$5 gen=$6 cluster=$7 min_free_gib=$8 kind_config=$9
  pivoted_guard "$lab_dir"
  doctor "$root" "$lab_dir" "$spec" "$size" "$workload" "$min_free_gib" "$kind_config"
  rerender_guard "$root" "$lab_dir" "$spec" "$size" "$workload"
  render "$root" "$spec" "$size" "$workload" "$gen"
  network_up "$root" "$gen"
  cluster_up "$lab_dir" "$cluster" "$kind_config"
  deploy "$root" "$lab_dir" "$gen" "$cluster"
  fleet_up "$root" "$lab_dir" "$gen/fleet.yaml"
  manifests_apply "$gen"
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
  # Exits non-zero when something is blocked or failed; the listing that
  # follows is still wanted then.
  "$root/metal/bin/pk-metal" status || true
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
    | select((.status.phase // "") as $p | ["Succeeded", "Failed", "Aborted"] | index($p) | not)
    | "\(.metadata.namespace) \(.metadata.name)"'
}

# Approves every pending operation as the kubeconfig's user and prints how many
# that were. An approval that fails is reported by pk-metal and not counted.
approve_pending() {
  local root=$1
  local namespace name count=0
  while read -r namespace name; do
    [[ -n $name ]] || continue
    # --yes: this is the unattended path; approve would otherwise ask.
    if "$root/metal/bin/pk-metal" approve --yes -n "$namespace" "$name" >&2; then
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

# Writes a cluster's kubeconfig and talosconfig to the files named. Returns
# non-zero, quietly, while the operator has not published them yet: wait()
# polls this.
kubeconfig_fetch() {
  local cluster_name=$1 kube_file=$2 talos_file=$3
  local refs kube_ns kube_name talos_ns talos_name
  refs=$(kubectl get taloscluster "$cluster_name" --output json 2>/dev/null | jq -r '
    [.status.kubeconfigSecretRef.namespace // "", .status.kubeconfigSecretRef.name // "",
     .status.talosconfigSecretRef.namespace // "", .status.talosconfigSecretRef.name // ""]
    | @tsv') || return 1
  IFS=$'\t' read -r kube_ns kube_name talos_ns talos_name <<<"$refs"
  [[ -n $kube_name ]] || return 1
  secret_to_file "${kube_ns:-$NAMESPACE}" "$kube_name" kubeconfig "$kube_file" || return 1
  if [[ -n $talos_name ]]; then
    secret_to_file "${talos_ns:-$NAMESPACE}" "$talos_name" talosconfig "$talos_file" || return 1
  fi
}

kubeconfig() {
  local lab_dir=$1 cluster_name=$2
  require_cluster "$lab_dir"
  kubeconfig_fetch "$cluster_name" "$lab_dir/workload.kubeconfig" "$lab_dir/talosconfig" \
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
  cluster_converged "$want" "$lab_dir/workload.kubeconfig" "$want"
}

# Succeeds when the operator reports hosts Provisioned machines and the
# cluster behind kube_file has nodes Ready nodes.
cluster_converged() {
  local hosts=$1 kube_file=$2 nodes=$3
  local provisioned ready
  provisioned=$(kubectl get hosts --output json | jq '[.items[] | select(.status.phase == "Provisioned")] | length')
  (( provisioned == hosts )) || return 1
  [[ -s $kube_file ]] || return 1
  ready=$(kubectl --kubeconfig "$kube_file" --request-timeout=5s get nodes --output json 2>/dev/null | jq '
    [.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length') || return 1
  (( ready == nodes ))
}

# Prints what failed when a claim is held because its operation failed, or a
# boot asset cannot be built; nothing otherwise. Such a claim waits for a person (pk-metal retry), so an unattended
# wait would only run into its deadline.
failed_operations() {
  local held
  # A boot asset whose build failed, or that has no builder, holds its machine
  # before any operation is proposed; it waits for a person just the same.
  kubectl get bootassets --output json | jq -r '
    .items[] | . as $ba | (.status.conditions // [])[]
    | select(.type == "Ready" and (.reason == "BuildFailed" or .reason == "BuilderUnavailable"))
    | "bootasset \($ba.metadata.name): \(.reason): \(.message)"'
  held=$(kubectl get hostclaims --all-namespaces --output json | jq -r '
    [.items[] | select(.status.blockedOn == "OperationFailed")] | length')
  (( held > 0 )) || return 0
  kubectl get hostoperations --all-namespaces --output json | jq -r '
    .items[] | select(.status.phase == "Failed")
    | "\(.metadata.namespace)/\(.metadata.name): " + (
        [(.status.steps // [])[] | select((.message // "") != "") | "\(.name // .action // "step"): \(.message)"]
        + [(.status.conditions // [])[] | select(.status == "False" or .type == "Failed") | "\(.type) \(.reason): \(.message)"]
        | join("; "))'
}

# The unattended run: stands in for the person by approving whatever is
# proposed, and holds until the lab has done what it is for.
wait_converged() {
  local root=$1 lab_dir=$2 gen=$3 timeout=$4
  local want cluster_name line failed last="" deadline=$((SECONDS + timeout))
  require_cluster "$lab_dir"
  want=$(lab_value "$gen" NODES)
  cluster_name=$(lab_value "$gen" TALOS_CLUSTER)
  step "waiting up to ${timeout}s for $want Provisioned hosts and $want Ready nodes (from $(date '+%F %T %Z'))"
  while true; do
    approve_pending "$root" >/dev/null || true
    kubeconfig_fetch "$cluster_name" "$lab_dir/workload.kubeconfig" "$lab_dir/talosconfig" || true
    if converged "$lab_dir" "$want"; then
      echo "converged after $((SECONDS - (deadline - timeout)))s: $(progress "$lab_dir")" >&2
      return 0
    fi
    line=$(progress "$lab_dir" 2>/dev/null || echo "the lab cluster does not answer")
    if [[ $line != "$last" ]]; then
      echo "  [$(date '+%F %T %Z')] $line" >&2
      last=$line
    fi
    failed=$(failed_operations 2>/dev/null || true)
    [[ -z $failed ]] || die "something failed and waits for a person:
$failed
  Look at it: mise run lab:status; collect the evidence: mise run lab:collect;
  retry it: metal/bin/pk-metal retry <claim>"
    (( SECONDS < deadline )) || die "the lab did not converge within ${timeout}s. Last state: $line
  Look at it: mise run lab:status; collect the evidence: mise run lab:collect;
  or wait longer: PK_LAB_WAIT_TIMEOUT=3600 mise run lab:wait"
    sleep 15
  done
}

# Prints the name of the Host that the claim namespace/name holds, or nothing.
claim_host() {
  local namespace=$1 name=$2
  kubectl get hosts --output json | jq -r --arg ns "$namespace" --arg name "$name" '
    .items[] | select(.spec.claimRef.namespace == $ns and .spec.claimRef.name == $name) | .metadata.name'
}

host_phase() { kubectl get host "$1" --output jsonpath='{.status.phase}' 2>/dev/null; }

claim_gone() { ! kubectl -n "$1" get hostclaim "$2" >/dev/null 2>&1; }

# Deletes a claim and waits until its machine is back: the claim is gone and its
# Host is Available again. A machine that runs its node is shut down only after
# a person approves (unless the claim waived approval); like wait, this stands
# in for that person through pk-metal approve. The outcome is what is checked,
# not the operation: a released machine that still shows its claim, or a Host
# that is not Available, is a failure however the operations ended.
release() {
  local root=$1 lab_dir=$2 claim=$3 timeout=$4
  local namespace name host phase deadline=$((SECONDS + timeout))
  require_cluster "$lab_dir"
  [[ $claim == */* ]] || die "name the claim as namespace/name, e.g.: mise run lab:release default/node-3"
  namespace=${claim%%/*}
  name=${claim#*/}
  kubectl -n "$namespace" get hostclaim "$name" >/dev/null \
    || die "there is no HostClaim $claim. See the claims: kubectl --kubeconfig $lab_dir/kubeconfig get hostclaims -A"
  host=$(claim_host "$namespace" "$name")
  [[ -n $host ]] || die "HostClaim $claim holds no Host yet, so there is nothing to release. See where it stands: mise run lab:status"

  step "releasing $claim (Host $host), waiting up to ${timeout}s for the Host to be Available"
  kubectl -n "$namespace" delete hostclaim "$name" --wait=false >&2
  while true; do
    approve_pending "$root" >/dev/null || true
    phase=$(host_phase "$host") || phase=""
    if claim_gone "$namespace" "$name" && [[ $phase == Available ]]; then
      echo "released after $((SECONDS - (deadline - timeout)))s: Host $host is Available, $claim is gone" >&2
      return 0
    fi
    (( SECONDS < deadline )) || die "the release of $claim did not finish within ${timeout}s: Host $host is ${phase:-unknown}.
  Look at it: mise run lab:status; collect the evidence: mise run lab:collect;
  or wait longer: PK_LAB_RELEASE_TIMEOUT=1800 mise run lab:release $claim"
    sleep 10
  done
}

# ---------------------------------------------------------------- day 2

# Writes a kubeconfig for a second person. pk-metal reboot records its caller
# as the proposer, and the approval policy refuses an approval by the proposer,
# so the lab proposes as lab-proposer (the lab's admin impersonates it) and
# approves as itself. system:masters gives the impersonated user the
# permissions; it is not a member of the admin group, which a reboot does not
# need.
proposer_kubeconfig() {
  local lab_dir=$1 file=$2
  (umask 077 && yq '.users[].user.as = "lab-proposer"
    | .users[].user."as-groups" = ["system:masters", "system:authenticated"]' "$lab_dir/kubeconfig" >"$file.tmp") \
    || die "could not write $file from $lab_dir/kubeconfig. Check that yq is on the PATH: mise run lab:doctor"
  mv "$file.tmp" "$file"
}

# Prints the name of a TalosNode of the role whose Host is Provisioned, or
# nothing. A released machine runs no node, so it is skipped.
running_node() {
  local role=$1
  local placed
  placed=$(kubectl get hosts --output json | jq -c '
    [.items[] | select(.status.phase == "Provisioned") | .spec.os.nodeRef.name // empty]')
  kubectl get talosnodes --output json | jq -r --arg role "$role" --argjson placed "$placed" '
    [.items[] | select(.spec.role == $role) | .metadata.name | select(. as $n | $placed | index($n))]
    | sort | first // empty'
}

control_plane_count() {
  local cluster_name=$1
  kubectl get talosnodes --output json | jq --arg cluster "$cluster_name" '
    [.items[] | select(.spec.clusterRef.name == $cluster and .spec.role == "controlplane")] | length'
}

# Prints the kubelet's boot ID of a workload node. It changes on every boot,
# whoever rebooted the machine, which makes it the outcome a reboot is checked
# against instead of the operation's own word.
boot_id() {
  local kube_file=$1 node=$2
  kubectl --kubeconfig "$kube_file" --request-timeout=5s get node "$node" \
    --output jsonpath='{.status.nodeInfo.bootID}' 2>/dev/null
}

# Succeeds when the workload node is Ready and schedulable.
node_back() {
  local kube_file=$1 node=$2
  kubectl --kubeconfig "$kube_file" --request-timeout=5s get node "$node" --output json 2>/dev/null | jq -e '
    (.spec.unschedulable // false | not)
    and any(.status.conditions[]?; .type == "Ready" and .status == "True")' >/dev/null
}

# Reboots one node the way a person does: pk-metal reboot proposes it, a
# second person approves it, and the operator cordons, drains, reboots and
# uncordons. The outcome is checked on the workload cluster, not only on the
# operation: the node's boot ID changed, and it is Ready and schedulable.
day2_reboot_node() {
  local root=$1 lab_dir=$2 proposer=$3 node=$4 override=$5 timeout=$6
  local kube_file=$lab_dir/workload.kubeconfig
  local before after ref namespace name phase message deadline=$((SECONDS + timeout))
  before=$(boot_id "$kube_file" "$node") \
    || die "the workload cluster does not answer for node $node. Fetch its kubeconfig: mise run lab:kubeconfig"
  [[ -n $before ]] || die "node $node reports no boot ID. See the nodes: kubectl --kubeconfig $kube_file get nodes"

  step "rebooting $node through pk-metal reboot, waiting up to ${timeout}s"
  ref=$("$root/metal/bin/pk-metal" --kubeconfig "$proposer" reboot "$node" | tee /dev/stderr \
    | sed -n 's/^proposed \([^:]*\):.*/\1/p')
  [[ $ref == */* ]] || die "pk-metal reboot $node proposed no operation it named. Run it by hand to see why:
  KUBECONFIG=$proposer metal/bin/pk-metal reboot $node"
  namespace=${ref%%/*}
  name=${ref#*/}
  if [[ -n $override ]]; then
    # A cluster with one control plane has no etcd majority without it, so
    # the cluster guard would hold its reboot forever. This is the case the
    # override is documented for (docs/metal/day2-actions.md).
    kubectl -n "$namespace" annotate hostoperation "$name" "metal.pk.io/override-cluster-guard=$override" >&2
  fi

  while true; do
    approve_pending "$root" >/dev/null || true
    phase=$(kubectl -n "$namespace" get hostoperation "$name" --output jsonpath='{.status.phase}' 2>/dev/null) || phase=""
    case "$phase" in
      Succeeded)
        after=$(boot_id "$kube_file" "$node") || after=""
        [[ -n $after && $after != "$before" ]] || die "operation $ref succeeded, but node $node did not boot again
  (boot ID $before before, ${after:-unknown} after). Collect the evidence: mise run lab:collect"
        node_back "$kube_file" "$node" || die "operation $ref succeeded, but node $node is not Ready and schedulable.
  See it: kubectl --kubeconfig $kube_file describe node $node"
        echo "rebooted $node after $((SECONDS - (deadline - timeout)))s: boot ID $before -> $after" >&2
        return 0
        ;;
      Failed | Aborted)
        message=$(kubectl -n "$namespace" get hostoperation "$name" --output json | jq -r '
          (.status.conditions // [])[] | select(.type == "Executed") | "\(.reason): \(.message)"')
        die "operation $ref ended $phase: $message
  Look at it: metal/bin/pk-metal status; collect the evidence: mise run lab:collect"
        ;;
    esac
    (( SECONDS < deadline )) || die "the reboot of $node did not finish within ${timeout}s (operation $ref is ${phase:-unknown}).
  Look at it: metal/bin/pk-metal status; or wait longer: PK_LAB_DAY2_TIMEOUT=2400 mise run lab:day2"
    sleep 10
  done
}

# Reboots a worker, then a control plane. A lab without workers skips the
# first; every lab has a control plane.
day2_reboot() {
  local root=$1 lab_dir=$2 gen=$3 timeout=$4
  local cluster_name proposer worker cp override=""
  cluster_name=$(lab_value "$gen" TALOS_CLUSTER)
  proposer=$lab_dir/proposer.kubeconfig
  proposer_kubeconfig "$lab_dir" "$proposer"
  worker=$(running_node worker)
  if [[ -n $worker ]]; then
    day2_reboot_node "$root" "$lab_dir" "$proposer" "$worker" "" "$timeout"
  else
    note "no running worker; only a control plane is rebooted"
  fi
  cp=$(running_node controlplane)
  [[ -n $cp ]] || die "no control plane runs on a Provisioned host. See where the lab stands: mise run lab:status"
  if (( $(control_plane_count "$cluster_name") == 1 )); then
    override="lab: cluster $cluster_name has a single control plane"
  fi
  day2_reboot_node "$root" "$lab_dir" "$proposer" "$cp" "$override" "$timeout"
}

# Placeholders for the rolling upgrade and the node add and remove rounds,
# which come with their own operations; until then they say so and pass.
day2_rollout() { note "day2: no rolling upgrade round in this lab yet"; }
day2_lifecycle() { note "day2: no node add and remove round in this lab yet"; }

# The day-2 rounds on a converged lab, in an order where each leaves the
# cluster whole for the next.
day2() {
  local root=$1 lab_dir=$2 gen=$3 timeout=$4
  require_cluster "$lab_dir"
  kubeconfig_fetch "$(lab_value "$gen" TALOS_CLUSTER)" "$lab_dir/workload.kubeconfig" "$lab_dir/talosconfig" \
    || die "the workload cluster has published no kubeconfig yet. Converge the lab first: mise run lab:wait"
  day2_reboot "$root" "$lab_dir" "$gen" "$timeout"
  day2_rollout
  day2_lifecycle
}

# ---------------------------------------------------------------- pivot

# After lab:pivot the lab kubeconfig names the first Talos cluster, and kind is
# gone. Bringing kind back with an operator in it would put two operators on
# the same machines.
pivoted_guard() {
  local lab_dir=$1
  [[ ! -e $lab_dir/$PIVOT_MARK ]] || die "the operator has moved to the first Talos cluster (mise run lab:pivot), and kind is gone.
  Starting kind with an operator again would put two operators on the same machines.
  Take the lab down first: mise run lab:down"
}

# Runs the workstation's image registry: on 127.0.0.1 for the push, and on the
# lab bridge address for the machines. It holds nothing that is not rebuilt.
registry_up() {
  local registry=$1 port=${1##*:}
  step "image registry on $registry"
  if [[ $(docker inspect --format '{{.State.Running}}' "$REGISTRY_CONTAINER" 2>/dev/null) == true ]]; then
    echo "  runs, reusing it" >&2
    return 0
  fi
  docker rm --force "$REGISTRY_CONTAINER" >/dev/null 2>&1 || true
  docker run --detach --name "$REGISTRY_CONTAINER" \
    --publish "127.0.0.1:$port:5000" --publish "$registry:5000" "$REGISTRY_IMAGE" >&2 \
    || die "docker could not start the registry ($REGISTRY_IMAGE). Its message above says why.
  The lab network must be up for the address ${registry%:*}: mise run lab:up"
}

registry_down() {
  if have docker; then
    docker rm --force "$REGISTRY_CONTAINER" >/dev/null 2>&1 || true
  fi
}

# Builds the operator image, pushes it to the registry and prints the
# reference the first cluster pulls it by.
image_push() {
  local root=$1 registry=$2 image name
  step "operator image to $registry"
  image=$("$root/metal/hack/image.sh" build)
  name=${image##*/}
  docker tag "$image" "localhost:${registry##*:}/$name" >&2
  docker push "localhost:${registry##*:}/$name" >&2 \
    || die "docker could not push $image to the registry. Is it running? docker ps --filter name=$REGISTRY_CONTAINER"
  echo "$registry/$name"
}

# The pivot round. The operator moves from kind onto the first Talos cluster
# with pk-metal pivot, kind is deleted, and from its new place the operator
# installs the second cluster. Rerun after a failure, it goes on from where it
# stopped: the move itself resumes, and once kind is gone only the second
# cluster is left to do.
pivot() {
  local root=$1 lab_dir=$2 gen=$3 cluster=$4 timeout=$5
  local first second registry ref media_node want_hosts want_nodes line failed last="" deadline=$((SECONDS + timeout))
  require_cluster "$lab_dir"
  [[ -n $(lab_value_or "$gen" WORKLOAD_CLUSTER "") ]] || die "the lab has no second cluster to install after the move.
  Bring it up with one: mise run lab:down && PK_LAB_WORKLOAD=1cp+0w mise run lab:up && mise run lab:wait"
  first=$(lab_value "$gen" TALOS_CLUSTER)
  second=$(lab_value "$gen" WORKLOAD_CLUSTER)
  registry=$(lab_value "$gen" REGISTRY)

  if [[ ! -e $lab_dir/$PIVOT_MARK ]]; then
    kubeconfig_fetch "$first" "$lab_dir/workload.kubeconfig" "$lab_dir/talosconfig" \
      || die "TalosCluster $first has not published a kubeconfig, so there is nowhere to move to yet.
  Install it first: mise run lab:wait"
    registry_up "$registry"
    ref=$(image_push "$root" "$registry")
    # kube-proxy answers a NodePort on the node addresses, not on the VIP
    # (the first run was refused on 10.77.0.10:30443), so the BMCs fetch from
    # the first control-plane node.
    media_node=$(kubectl --kubeconfig "$lab_dir/workload.kubeconfig" get nodes \
      --selector node-role.kubernetes.io/control-plane --output json \
      | jq -r '[.items[].status.addresses[] | select(.type == "InternalIP") | .address][0] // empty')
    [[ -n $media_node ]] || die "TalosCluster $first lists no control-plane node address, so the BMCs would have no media address.
  Look at it: kubectl --kubeconfig $lab_dir/workload.kubeconfig get nodes -o wide"
    step "pk-metal pivot to TalosCluster $first"
    # The lab's operator mounts the media store (config/lab), and the first
    # Talos cluster is bare: no StorageClass, so pivot would refuse to make
    # the claim there. --media-store=ephemeral mounts an emptyDir instead.
    # The second cluster's machines have two-slot BMCs, which need no store;
    # a one-slot machine there would wait with BuilderUnavailable, because
    # the imager Jobs cannot write into the manager's emptyDir.
    "$root/metal/bin/pk-metal" pivot --to "$lab_dir/workload.kubeconfig" --image "$ref" \
      --media-url "https://$media_node:30443" --media-store ephemeral --timeout 600s --yes >&2 \
      || die "the move did not finish. Its message above says what to do; rerun it with: mise run lab:pivot"
    step "deleting the kind cluster $cluster"
    kind delete cluster --name "$cluster" >&2
    cp "$lab_dir/workload.kubeconfig" "$lab_dir/kubeconfig"
    chmod 600 "$lab_dir/kubeconfig"
    echo "$first" >"$lab_dir/$PIVOT_MARK"
    echo "  the lab's kubeconfig names TalosCluster $first now" >&2
  fi

  step "TalosCluster $second, installed by the operator on $first"
  admin_apply "$gen/workload/"
  want_hosts=$(($(lab_value "$gen" NODES) + $(lab_value "$gen" WORKLOAD_NODES)))
  want_nodes=$(lab_value "$gen" WORKLOAD_NODES)
  while true; do
    approve_pending "$root" >/dev/null || true
    kubeconfig_fetch "$second" "$lab_dir/second.kubeconfig" "$lab_dir/second.talosconfig" || true
    if cluster_converged "$want_hosts" "$lab_dir/second.kubeconfig" "$want_nodes"; then
      echo "converged after $((SECONDS - (deadline - timeout)))s: $want_hosts Provisioned hosts, $want_nodes Ready nodes in $second" >&2
      break
    fi
    line=$(progress "$lab_dir" 2>/dev/null || echo "the first cluster does not answer")
    if [[ $line != "$last" ]]; then
      echo "  [$(date '+%F %T %Z')] $line" >&2
      last=$line
    fi
    failed=$(failed_operations 2>/dev/null || true)
    [[ -z $failed ]] || die "something failed on the operator's new cluster and waits for a person:
$failed
  Look at it: mise run lab:status; collect the evidence: mise run lab:collect"
    (( SECONDS < deadline )) || die "TalosCluster $second did not come up within ${timeout}s. Last state: $line
  Look at it: mise run lab:status; collect the evidence: mise run lab:collect"
    sleep 15
  done
  cat >&2 <<EOF

The operator runs on TalosCluster $first and installed $second from there.
  the operator's cluster: $lab_dir/kubeconfig (also $lab_dir/workload.kubeconfig)
  the second cluster:     $lab_dir/second.kubeconfig and $lab_dir/second.talosconfig
EOF
}

# ---------------------------------------------------------------- day 2: etcd backup and restore

# The backup volume (config/etcd-backup) on the running operator. The lab's
# deployment does not carry it, so that the default lab stays as it is; this
# adds the claim and patches the Deployment with the component's own patch.
backup_volume_up() {
  local root=$1 namespace=$2 deployment=$3
  local component=$root/metal/config/etcd-backup
  step "etcd backup volume"
  kubectl apply --server-side -f "$component/pvc.yaml" >&2
  if ! kubectl -n "$namespace" get deployment "$deployment" --output json \
    | jq -e '.spec.template.spec.containers[0].args // [] | any(startswith("--etcd-backup-dir="))' >/dev/null; then
    kubectl -n "$namespace" patch deployment "$deployment" --type json --patch-file "$component/manager.yaml" >&2
  fi
  kubectl -n "$namespace" rollout status "deployment/$deployment" --timeout=300s >&2
}

# An age identity for the lab's restorer, made once. The operator only ever
# sees the public key, which this prints.
age_identity() {
  local root=$1 file=$2
  if [[ ! -s $file ]]; then
    (umask 077 && cd "$root/metal" && go run filippo.io/age/cmd/age-keygen -o "$file") 2>/dev/null \
      || die "could not make an age identity with go run filippo.io/age/cmd/age-keygen. Check that Go works: cd metal && go env"
  fi
  sed -n 's/^# public key: //p' "$file"
}

# A kubeconfig that acts as another person: the lab's admin impersonating
# user. Proposing and approving must be two people (the approval policy
# refuses an approval by whoever proposed), and the lab has one admin.
impersonating_kubeconfig() {
  local from=$1 to=$2 user=$3
  (umask 077 && IMPERSONATE=$user yq '
    .users[].user.as = strenv(IMPERSONATE)
    | .users[].user["as-groups"] = ["system:masters", "pk-metal:admins", "system:authenticated"]' "$from" >"$to")
}

# Sets a marker ConfigMap in the workload cluster: one written before the
# snapshot must come back with the restore, one written after must not.
marker_set() {
  local kube_file=$1 name=$2
  kubectl --kubeconfig "$kube_file" --request-timeout=20s -n default create configmap "$name" \
    --from-literal=written="$(date -u +%FT%TZ)" --dry-run=client --output yaml \
    | kubectl --kubeconfig "$kube_file" --request-timeout=20s apply -f - >&2
}

marker_present() {
  local kube_file=$1 name=$2
  kubectl --kubeconfig "$kube_file" --request-timeout=10s -n default get configmap "$name" >/dev/null 2>&1
}

# Succeeds when the TalosCluster's Bootstrapped condition has this status
# and, if given, this reason.
bootstrapped_is() {
  local cluster_name=$1 status=$2 reason=${3:-}
  kubectl get taloscluster "$cluster_name" --output json | jq -e --arg s "$status" --arg r "$reason" '
    [.status.conditions[]? | select(.type == "Bootstrapped")][0]
    | . != null and .status == $s and ($r == "" or .reason == $r)' >/dev/null
}

# Copies one file off the backup volume, through a pod that mounts the claim
# as the manager's user (the volume is the operator's; nothing else reads it).
backup_volume_copy() {
  local namespace=$1 deployment=$2 key=$3 out=$4
  local node
  node=$(kubectl -n "$namespace" get pod --selector app.kubernetes.io/name=pk-metal,app.kubernetes.io/component=manager \
    --output jsonpath='{.items[0].spec.nodeName}')
  kubectl -n "$namespace" delete pod etcd-backup-reader --ignore-not-found --wait=true >&2
  kubectl apply -f - >&2 <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: etcd-backup-reader
  namespace: $namespace
spec:
  nodeName: $node
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: reader
      image: busybox:1.37
      command: ["sleep", "3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: ["ALL"] }
      volumeMounts:
        - { name: backups, mountPath: /backups, readOnly: true }
  volumes:
    - name: backups
      persistentVolumeClaim: { claimName: pk-metal-etcd-backups, readOnly: true }
EOF
  kubectl -n "$namespace" wait --for=condition=Ready pod/etcd-backup-reader --timeout=180s >&2
  kubectl -n "$namespace" exec etcd-backup-reader -- cat "/backups/$key" >"$out"
  kubectl -n "$namespace" delete pod etcd-backup-reader --wait=false >&2
  [[ -s $out ]] || die "copied nothing from /backups/$key on the backup volume (deployment $deployment). Look at it:
  kubectl -n $namespace get etcdsnapshots; mise run lab:collect"
}

# The total-loss round on the lab's first cluster: back it up to the backup
# volume, wipe the etcd data of every control plane, and restore it the way
# docs/metal/disaster-recovery.md says, through pk-metal restore and an
# approved etcdRecover operation. The outcome is checked, not the steps: a
# marker written before the snapshot is back, one written after it is not,
# and the TalosCluster is Bootstrapped again.
day2_backup_restore() {
  local root=$1 lab_dir=$2 gen=$3 namespace=$4 deployment=$5 timeout=$6
  local cli=$root/metal/bin/pk-metal dir=$lab_dir/backup-restore
  local cluster_name policy recipient snap key op addr addrs line last="" deadline=$((SECONDS + timeout))
  require_cluster "$lab_dir"
  cluster_name=$(lab_value "$gen" TALOS_CLUSTER)
  kubeconfig_fetch "$cluster_name" "$lab_dir/workload.kubeconfig" "$lab_dir/talosconfig" \
    || die "TalosCluster $cluster_name has not published a kubeconfig, so there is nothing to back up yet.
  Install it first: mise run lab:wait"
  mkdir -p "$dir"
  chmod 700 "$dir"
  policy=$cluster_name-dr

  backup_volume_up "$root" "$namespace" "$deployment"
  recipient=$(age_identity "$root" "$dir/identity.txt")
  [[ $recipient == age1* ]] || die "$dir/identity.txt has no public key line. Remove it and run again."

  step "EtcdBackupPolicy $policy, a snapshot, and markers around it"
  kubectl apply --server-side -f - >&2 <<EOF
apiVersion: os.pk.io/v1alpha1
kind: EtcdBackupPolicy
metadata:
  name: $policy
spec:
  clusterRef:
    name: $cluster_name
  interval: 24h
  sink:
    pvc: {}
  encryption:
    ageRecipients:
      - $recipient
EOF
  marker_set "$lab_dir/workload.kubeconfig" dr-before
  "$cli" backup now "$policy" >&2 || die "the snapshot did not complete. Look at it: kubectl get etcdsnapshots; kubectl describe etcdbackuppolicy $policy"
  snap=$(kubectl get etcdsnapshots --selector "os.pk.io/etcd-backup-policy=$policy" --output json \
    | jq -r '[.items[] | select(any(.status.conditions[]?; .type == "Verified" and .status == "True"))]
      | sort_by(.status.takenAt) | last | .metadata.name // empty')
  [[ -n $snap ]] || die "no verified snapshot of policy $policy. Look at it: kubectl get etcdsnapshots -o yaml"
  key=$(kubectl get etcdsnapshot "$snap" --output jsonpath='{.status.key}')
  "$cli" backup list "$cluster_name" >&2
  marker_set "$lab_dir/workload.kubeconfig" dr-after

  step "losing etcd: wiping EPHEMERAL on every control plane of $cluster_name"
  addrs=$(kubectl get talosnodes --output json | jq -r --arg c "$cluster_name" '
    .items[] | select(.spec.clusterRef.name == $c and .spec.role == "controlplane") | .spec.address // empty')
  [[ -n $addrs ]] || die "TalosCluster $cluster_name lists no control-plane address. Look at it: kubectl get talosnodes -o wide"
  for addr in $addrs; do
    talosctl --talosconfig "$lab_dir/talosconfig" --endpoints "$addr" --nodes "$addr" \
      reset --graceful=false --reboot --system-labels-to-wipe EPHEMERAL --wait=false >&2
  done
  while ! bootstrapped_is "$cluster_name" False EtcdDataLost; do
    (( SECONDS < deadline )) || die "TalosCluster $cluster_name did not report EtcdDataLost within ${timeout}s. Look at it:
  kubectl describe taloscluster $cluster_name"
    sleep 10
  done
  echo "  the operator sees the loss and does not bootstrap: $(kubectl get taloscluster "$cluster_name" \
    --output jsonpath='{.status.conditions[?(@.type=="Bootstrapped")].message}')" >&2

  step "restoring $snap"
  backup_volume_copy "$namespace" "$deployment" "$key" "$dir/snapshot.db.age"
  "$cli" backup decrypt --in "$dir/snapshot.db.age" --identity "$dir/identity.txt" >&2
  impersonating_kubeconfig "$lab_dir/kubeconfig" "$dir/restorer.kubeconfig" lab-restorer
  # A control plane comes back from the reset some time after; until every
  # one answers, the plan may propose nothing. Proposing again is harmless:
  # an operation that exists is left as it is.
  while true; do
    KUBECONFIG=$dir/restorer.kubeconfig "$cli" restore plan --snapshot "$snap" --propose "$cluster_name" >&2 || true
    op=$(kubectl -n "$namespace" get hostoperations --selector "os.pk.io/etcd-restore=$cluster_name" --output json \
      | jq -r '.items[] | select(any(.spec.actions[]?; .etcdRecover != null)) | .metadata.name' | head -n 1)
    [[ -z $op ]] || break
    (( SECONDS < deadline )) || die "pk-metal restore plan proposed no etcdRecover operation within ${timeout}s; its output above says why"
    sleep 15
  done
  # The lab's admin approves what lab-restorer proposed: two people.
  approve_pending "$root" >/dev/null
  until "$cli" restore upload "$namespace/$op" --identity "$dir/identity.txt" --from-file "$dir/snapshot.db.age" >&2; do
    (( SECONDS < deadline )) || die "the snapshot upload to $op did not succeed within ${timeout}s; its output above says why"
    approve_pending "$root" >/dev/null || true
    sleep 15
  done

  step "waiting for $op and for etcd"
  while true; do
    approve_pending "$root" >/dev/null || true
    line="$(kubectl -n "$namespace" get hostoperation "$op" --output jsonpath='{.status.phase}' 2>/dev/null): $(
      kubectl -n "$namespace" get hostoperation "$op" \
        --output jsonpath='{.status.conditions[?(@.type=="Executed")].message}' 2>/dev/null)"
    if [[ $line == Succeeded:* ]] && bootstrapped_is "$cluster_name" True \
      && marker_present "$lab_dir/workload.kubeconfig" dr-before; then
      break
    fi
    [[ $line != Failed:* ]] || die "the restore operation $op failed: ${line#Failed: }
  Look at it: mise run lab:status; collect the evidence: mise run lab:collect"
    if [[ $line != "$last" ]]; then
      echo "  [$(date '+%F %T %Z')] $op $line" >&2
      last=$line
    fi
    (( SECONDS < deadline )) || die "the restore did not finish within ${timeout}s. Last state: $op $line
  Look at it: mise run lab:status; collect the evidence: mise run lab:collect"
    sleep 15
  done
  ! marker_present "$lab_dir/workload.kubeconfig" dr-after \
    || die "ConfigMap default/dr-after, written after the snapshot, is in the restored cluster: this is not the snapshot's state"
  echo "restored after $((SECONDS - (deadline - timeout)))s: $cluster_name is Bootstrapped from $snap; dr-before is back, dr-after is not" >&2
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
    kubectl get assets,managementendpoints,hosts,talosclusters,talosnodes,bootassets,etcdbackuppolicies,etcdsnapshots --output wide >"$out/get.txt" 2>&1 || true
    kubectl get hostclaims,hostoperations --all-namespaces --output wide >>"$out/get.txt" 2>&1 || true
    kubectl get assets,managementendpoints,hosts,talosclusters,talosnodes,bootassets,etcdbackuppolicies,etcdsnapshots --output yaml >"$out/objects.yaml" 2>&1 || true
    kubectl get hostclaims,hostoperations --all-namespaces --output yaml >>"$out/objects.yaml" 2>&1 || true
    kubectl get events --all-namespaces --sort-by=.lastTimestamp >"$out/events.txt" 2>&1 || true
    kubectl -n "$NAMESPACE" describe deployment,pod,service,configmap,job,persistentvolumeclaim >"$out/operator-describe.txt" 2>&1 || true
    # Names and types only: a describe of a Secret shows sizes, never values.
    kubectl -n "$NAMESPACE" get secrets >"$out/secrets-list.txt" 2>&1 || true
    kubectl -n "$NAMESPACE" logs "deployment/$DEPLOYMENT" --all-containers --tail=-1 >"$out/operator.log" 2>&1 || true
    kubectl -n "$NAMESPACE" logs "deployment/$DEPLOYMENT" --all-containers --previous --tail=-1 >"$out/operator-previous.log" 2>&1 || true
    # The installer builds for one-slot BMCs. Their log never holds the
    # machine config: the build withholds the imager's output and prints a
    # redacted tail only when it fails.
    kubectl -n "$NAMESPACE" logs --selector app.kubernetes.io/component=imager --all-containers --prefix --tail=-1 >"$out/imager.log" 2>&1 || true
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
  registry_down
  rm -f "$lab_dir/kubeconfig" "$lab_dir/workload.kubeconfig" "$lab_dir/talosconfig" \
    "$lab_dir/second.kubeconfig" "$lab_dir/second.talosconfig" "$lab_dir/$PIVOT_MARK"
  "$root/lab/net.sh" down
  echo "kept: the machines' disks, the image cache and the secrets bundle. Wipe the disks with: mise run lab:reset" >&2
}

# Succeeds when a directory may be removed as the fleet's state: it is not the
# file system root, the home directory, the repository or a directory above it,
# and it is either inside the lab directory or carries the mark fleet_up
# leaves. The path comes out of a file somebody edits; `stateDir: .` or
# `stateDir: metal` must not take the source tree with it.
state_dir_removable() {
  local dir=$1 root=$2 lab_dir=$3
  dir=$(realpath -m "$dir")
  root=$(realpath -m "$root")
  lab_dir=$(realpath -m "$lab_dir")
  [[ $dir != / && $dir != "$(realpath -m ~)" ]] || return 1
  case "$root/" in
    "$dir"/*) return 1 ;;
  esac
  # Inside the lab directory counts only while the lab directory is not the
  # repository itself (PK_LAB_DIR=. would make the whole tree "inside").
  case "$root/" in
    "$lab_dir"/*) ;;
    *)
      case "$dir" in
        "$lab_dir"/?*) return 0 ;;
      esac
      ;;
  esac
  [[ -f $dir/$STATE_MARK ]]
}

reset() {
  local root=$1 lab_dir=$2 fleet=$3
  local state_dir
  fleet_running "$lab_dir" "$root/metal/bin/pk-metal" \
    && die "the fleet is running and has the disks open. Take the lab down first: mise run lab:down"
  state_dir=$(fleet_state_dir "$root" "$fleet")
  if [[ ! -e $state_dir ]]; then
    echo "nothing to wipe: $state_dir does not exist" >&2
    return 0
  fi
  state_dir_removable "$state_dir" "$root" "$lab_dir" \
    || die "stateDir in $fleet resolves to $state_dir. That is not inside $lab_dir and has no
  $STATE_MARK file, so it may not be the lab's (or it holds the repository). Nothing was removed.
  Remove the directory by hand if it really holds the lab's disks."
  rm -rf "$state_dir"
  echo "wiped $state_dir. The next lab:up installs every machine from scratch." >&2
}

main() {
  local verb=${1:-}
  case "$verb" in
    doctor) doctor "$ROOT" "$LAB_DIR" "$SPEC" "$SIZE" "$WORKLOAD" "$MIN_FREE_GIB" "$KIND_CONFIG" ;;
    render) render "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN" ;;
    up) up "$ROOT" "$LAB_DIR" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN" "$CLUSTER" "$MIN_FREE_GIB" "$KIND_CONFIG" ;;
    network)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      network_up "$ROOT" "$GEN"
      ;;
    cluster)
      pivoted_guard "$LAB_DIR"
      cluster_up "$LAB_DIR" "$CLUSTER" "$KIND_CONFIG"
      ;;
    deploy)
      pivoted_guard "$LAB_DIR"
      require_cluster "$LAB_DIR"
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      deploy "$ROOT" "$LAB_DIR" "$GEN" "$CLUSTER"
      ;;
    fleet)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      fleet_up "$ROOT" "$LAB_DIR" "$FLEET"
      ;;
    manifests)
      require_cluster "$LAB_DIR"
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      manifests_apply "$GEN"
      ;;
    apply)
      require_cluster "$LAB_DIR"
      shift
      if (( $# == 0 )); then
        require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
        manifests_apply "$GEN"
      else
        admin_apply "$@"
      fi
      ;;
    status) status "$ROOT" "$LAB_DIR" ;;
    approve) approve "$ROOT" "$LAB_DIR" ;;
    kubeconfig)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      kubeconfig "$LAB_DIR" "$(lab_value "$GEN" TALOS_CLUSTER)"
      ;;
    wait)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      wait_converged "$ROOT" "$LAB_DIR" "$GEN" "$WAIT_TIMEOUT"
      ;;
    release)
      (( $# == 2 )) || die "name one claim, e.g.: mise run lab:release default/node-3"
      release "$ROOT" "$LAB_DIR" "$2" "$RELEASE_TIMEOUT"
      ;;
    day2)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      day2 "$ROOT" "$LAB_DIR" "$GEN" "$DAY2_TIMEOUT"
      ;;
    pivot)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      pivot "$ROOT" "$LAB_DIR" "$GEN" "$CLUSTER" "$WAIT_TIMEOUT"
      ;;
    backup-restore)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      day2_backup_restore "$ROOT" "$LAB_DIR" "$GEN" "$NAMESPACE" "$DEPLOYMENT" "$WAIT_TIMEOUT"
      ;;
    collect)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      collect "$ROOT" "$LAB_DIR" "$FLEET"
      ;;
    down) down "$ROOT" "$LAB_DIR" "$CLUSTER" ;;
    reset)
      require_rendered "$ROOT" "$SPEC" "$SIZE" "$WORKLOAD" "$GEN"
      reset "$ROOT" "$LAB_DIR" "$FLEET"
      ;;
    -h | --help | help) usage ;;
    *)
      usage
      exit 1
      ;;
  esac
}

main "$@"
