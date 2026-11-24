#!/usr/bin/env bash
# The virtual lab's network: one bridge the workstation routes for, one tap per
# virtual machine, and the firewall rules that let the three parties talk.
#
#   kind node (docker network) ──┐
#                                ├── host routing ── pkbr0 10.77.0.1/24 ── taps ── VMs
#   uplink (NAT egress) ─────────┘
#
# The taps belong to the invoking user, so QEMU runs without root and this is the
# only file in the lab that calls sudo. Everything it creates is marked (an
# interface alias, a rule comment) and `down` removes marked things only: a
# bridge or a rule somebody else made under the same name is left alone and
# reported instead.
#
# What it changes on the workstation, as root:
#   - a bridge (pkbr0 with 10.77.0.1/24 by default) and one tap per machine;
#   - net.ipv4.ip_forward=1, if it was off (down turns it off again);
#   - iptables rules, each with the comment pk-metal-lab: ACCEPT for traffic
#     from and to the bridge (DOCKER-USER, or FORWARD without Docker), ACCEPT
#     for input to 10.77.0.1 from the bridge and from Docker's bridges, and a
#     MASQUERADE for the lab subnet;
#   - with firewalld running: pkbr0 in the zone trusted (runtime only).
# Nothing of it survives a reboot; nothing is written to a file in /etc.
set -euo pipefail

# shellcheck source=lab/distro.sh
source "$(dirname "$0")/distro.sh"

SCRIPT=${0##*/}
HELP="usage: $SCRIPT up [tap...] | down | status [tap...]

  up      create the bridge, the named taps and the firewall rules (idempotent)
  down    remove what up created: marked taps on the bridge, the bridge, the rules
  status  show the bridge, the taps and the rules; non-zero when something is missing

environment variables:
  PK_LAB_BRIDGE     bridge name (default: pkbr0)
  PK_LAB_ADDRESS    the workstation's address on the bridge, with prefix (default: 10.77.0.1/24)
  PK_LAB_TAP_USER   owner of the taps (default: the invoking user, \$SUDO_USER under sudo)"

BRIDGE=${PK_LAB_BRIDGE:-pkbr0}
ADDRESS=${PK_LAB_ADDRESS:-10.77.0.1/24}
TAP_USER=${PK_LAB_TAP_USER:-${SUDO_USER:-$(id -un)}}

# The mark on everything this script creates: the alias of an interface, the
# comment of a firewall rule. `down` trusts nothing else.
MARK=pk-metal-lab

# Whether up turned IPv4 forwarding on is remembered here, so that down can turn
# it off again only in that case. /run does not survive a reboot, and neither
# does the sysctl, so the two cannot disagree.
STATE_DIR=/run/pk-metal-lab

usage() { echo "$HELP" >&2; }

die() {
  echo "Error: $*" >&2
  exit 1
}

step() { echo "  $*" >&2; }

# Runs a command as root. sudo is asked for here and nowhere else in the lab.
as_root() {
  if (( EUID == 0 )); then
    "$@"
  else
    sudo "$@"
  fi
}

need() {
  local tool=$1 fix=$2
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not installed. $fix"
}

preflight() {
  need ip "Install it: $(install_hint iproute2 iproute)"
  need iptables "Install it: $(install_hint iptables iptables-nft)"
  if (( EUID != 0 )); then
    need sudo "Run this script as root, or install sudo."
    sudo -v || die "sudo refused. The lab network needs root for ip and iptables;
  run it from a terminal where sudo can ask, or as root: sudo $SCRIPT $*"
  fi
}

# The network of an address in CIDR notation: 10.77.0.1/24 -> 10.77.0.0/24.
# Only the prefix lengths that fall on a byte boundary are supported, which is
# all a lab needs and keeps this readable.
subnet_of() {
  local address=$1 ip prefix a b c
  ip=${address%/*}
  prefix=${address#*/}
  IFS=. read -r a b c _ <<<"$ip"
  case "$prefix" in
    24) echo "$a.$b.$c.0/24" ;;
    16) echo "$a.$b.0.0/16" ;;
    *) die "PK_LAB_ADDRESS=$address: only /24 and /16 are supported, e.g.: PK_LAB_ADDRESS=10.77.0.1/24" ;;
  esac
}

link_exists() { ip link show dev "$1" >/dev/null 2>&1; }

link_is_ours() {
  local dev=$1
  [[ $(cat "/sys/class/net/$dev/ifalias" 2>/dev/null) == "$MARK" ]]
}

# The filter chain forwarded packets are accepted in. Docker sets the FORWARD
# policy to DROP and owns the rules in FORWARD itself, but it evaluates
# DOCKER-USER first and never touches its contents, so that is where a rule
# survives a docker restart. Without docker there is no such chain and FORWARD
# is ours to use.
forward_chain() {
  if as_root iptables -w -n -L DOCKER-USER >/dev/null 2>&1; then
    echo DOCKER-USER
  else
    echo FORWARD
  fi
}

# Every rule this script owns, one per line as "<table> <chain> <rule...>", for
# the given forward chain. up, down and status all read this one list, so they
# cannot disagree about what the lab's rules are.
rules() {
  local chain=$1 bridge=$2 address=$3 subnet
  subnet=$(subnet_of "$address")
  # VM to VM (bridged frames pass through FORWARD when br_netfilter is loaded,
  # which docker and kind do), VM to the uplink and VM to the kind network.
  echo "filter $chain -i $bridge -j ACCEPT"
  # The way back, and the operator in kind reaching the Talos and Kubernetes
  # APIs of the VMs.
  echo "filter $chain -o $bridge -j ACCEPT"
  # The simulated BMCs and the published media port listen on the bridge address
  # of the workstation itself. A host firewall with a default-deny INPUT policy
  # (ufw, firewalld) would silently drop the operator's Redfish calls. Accepted
  # only from the lab bridge and from Docker's bridges (br-<network id>, where
  # kind's node sits), never from the workstation's own network.
  echo "filter INPUT -i $bridge -d ${address%/*}/32 -j ACCEPT"
  echo "filter INPUT -i br-+ -d ${address%/*}/32 -j ACCEPT"
  # Egress for the VMs: image pulls, time, DNS.
  echo "nat POSTROUTING -s $subnet ! -o $bridge -j MASQUERADE"
}

# Rules earlier versions of this script made and the current one no longer
# does. down removes them too, so an upgrade leaves nothing behind.
retired_rules() {
  local address=$1
  echo "filter INPUT -d ${address%/*}/32 -j ACCEPT"
}

rule_present() {
  local table=$1 chain=$2
  shift 2
  as_root iptables -w -t "$table" -C "$chain" "$@" -m comment --comment "$MARK" 2>/dev/null
}

add_rule() {
  local table=$1 chain=$2
  shift 2
  rule_present "$table" "$chain" "$@" && return 0
  # Inserted at the top: in DOCKER-USER and in a FORWARD chain somebody else
  # also writes to, an appended ACCEPT can sit behind a DROP or a RETURN.
  as_root iptables -w -t "$table" -I "$chain" "$@" -m comment --comment "$MARK"
}

delete_rule() {
  local table=$1 chain=$2
  shift 2
  # A rule added twice by hand would survive a single -D, hence the loop.
  while rule_present "$table" "$chain" "$@"; do
    as_root iptables -w -t "$table" -D "$chain" "$@" -m comment --comment "$MARK"
  done
}

up_bridge() {
  local bridge=$1 address=$2
  if link_exists "$bridge"; then
    link_is_ours "$bridge" || die "an interface named $bridge exists and was not made by this script.
  Remove it (sudo ip link del $bridge) or give the lab another name: PK_LAB_BRIDGE=pkbr1 $SCRIPT up"
  else
    step "bridge $bridge"
    # A fixed MAC: a bridge otherwise takes the lowest MAC among its ports and
    # changes it whenever a tap comes or goes, which leaves every VM with a
    # stale neighbour entry for its gateway.
    as_root ip link add name "$bridge" address 02:77:00:00:00:01 type bridge stp_state 0
    as_root ip link set dev "$bridge" alias "$MARK"
  fi
  if ! ip -o -4 addr show dev "$bridge" | grep -qF " $address "; then
    as_root ip addr add "$address" dev "$bridge"
  fi
  as_root ip link set dev "$bridge" up
}

up_tap() {
  local tap=$1 bridge=$2 user=$3
  if link_exists "$tap"; then
    link_is_ours "$tap" || die "an interface named $tap exists and was not made by this script.
  Remove it (sudo ip link del $tap); the lab names its taps pktap1, pktap2, ..."
  else
    step "tap $tap (owner $user)"
    as_root ip tuntap add dev "$tap" mode tap user "$user"
    as_root ip link set dev "$tap" alias "$MARK"
  fi
  as_root ip link set dev "$tap" master "$bridge"
  as_root ip link set dev "$tap" up
}

# ---------------------------------------------------------------- firewalld

# firewalld (RHEL, Fedora) filters in its own nftables table. A packet must pass
# that table as well as the iptables rules above, and firewalld rejects what
# its zones do not allow, whatever iptables accepted. An interface in no zone
# falls into the default zone (public), which allows neither the operator's
# Redfish calls nor the machines' forwarded traffic. The zone trusted allows
# both. Docker puts its own bridges, kind's among them, into its zone docker.
firewalld_running() {
  command -v firewall-cmd >/dev/null 2>&1 && as_root firewall-cmd --state >/dev/null 2>&1
}

firewalld_zone() { as_root firewall-cmd --get-zone-of-interface="$1" 2>/dev/null || true; }

up_firewalld() {
  local bridge=$1 zone
  firewalld_running || return 0
  zone=$(firewalld_zone "$bridge")
  case "$zone" in
    trusted) ;;
    "")
      step "firewalld: $bridge into the zone trusted (runtime only)"
      as_root firewall-cmd --quiet --zone=trusted --add-interface="$bridge"
      as_root mkdir -p "$STATE_DIR"
      as_root touch "$STATE_DIR/firewalld"
      ;;
    *) die "firewalld has $bridge in the zone $zone, which drops the lab's traffic.
  Move it: sudo firewall-cmd --zone=trusted --change-interface=$bridge, then rerun: $SCRIPT up" ;;
  esac
}

down_firewalld() {
  local bridge=$1
  [[ -e $STATE_DIR/firewalld ]] || return 0
  firewalld_running || return 0
  if [[ $(firewalld_zone "$bridge") == trusted ]]; then
    step "firewalld: $bridge out of the zone trusted"
    as_root firewall-cmd --quiet --zone=trusted --remove-interface="$bridge" || true
  fi
}

# ---------------------------------------------------------------- nftables

# Base chains of other nftables tables (an nftables.service ruleset, a hand
# written one) that drop input or forwarded traffic by policy and never name
# the bridge, one per line as "<family> <table> <chain> <hook>". The iptables
# tables (ip filter, ip nat, and their ip6 twins) are left out: that is where
# the rules above live. firewalld's table is handled above. A packet must pass
# every base chain on its hook, so an ACCEPT in iptables does not help against
# a drop there. Reject rules at the end of a chain are not detected.
foreign_drop_chains() {
  local bridge=$1 ruleset
  command -v nft >/dev/null 2>&1 || return 0
  ruleset=$(as_root nft list ruleset 2>/dev/null) || return 0
  awk -v bridge="\"$bridge\"" '
    $1 == "table" { family = $2; table = $3; next }
    $1 == "chain" { chain = $2; hook = ""; drop = 0; named = 0; next }
    /type filter hook (input|forward)/ {
      for (i = 1; i <= NF; i++) if ($i == "hook") hook = $(i + 1)
      if ($0 ~ /policy drop/) drop = 1
    }
    index($0, bridge) { named = 1 }
    $1 == "}" && chain != "" {
      own = (table == "filter" || table == "nat") && (family == "ip" || family == "ip6")
      if (hook != "" && drop && !named && !own && table != "firewalld") print family, table, chain, hook
      chain = ""
    }' <<<"$ruleset"
}

warn_foreign_drops() {
  local bridge=$1 address=$2 family table chain hook
  while read -r family table chain hook; do
    [[ -n $chain ]] || continue
    echo "  WARNING: nftables chain $family $table $chain drops $hook traffic by policy and does not mention $bridge." >&2
    echo "  The lab's iptables rules cannot override it. Allow the lab's traffic there:" >&2
    if [[ $hook == input ]]; then
      echo "    sudo nft insert rule $family $table $chain iifname \"$bridge\" accept" >&2
      echo "    sudo nft insert rule $family $table $chain iifname \"br-*\" ip daddr ${address%/*} accept" >&2
    else
      echo "    sudo nft insert rule $family $table $chain iifname \"$bridge\" accept" >&2
      echo "    sudo nft insert rule $family $table $chain oifname \"$bridge\" accept" >&2
    fi
  done < <(foreign_drop_chains "$bridge")
}

up_forwarding() {
  if [[ $(cat /proc/sys/net/ipv4/ip_forward) != 1 ]]; then
    step "enable IPv4 forwarding"
    as_root mkdir -p "$STATE_DIR"
    as_root touch "$STATE_DIR/ip_forward"
    as_root sysctl -q -w net.ipv4.ip_forward=1
  fi
}

up() {
  local bridge=$1 address=$2 user=$3
  shift 3
  local chain tap table rule_chain rule
  id -u "$user" >/dev/null 2>&1 \
    || die "user $user does not exist. Name the owner of the taps: PK_LAB_TAP_USER=<user> $SCRIPT up"
  [[ -c /dev/net/tun ]] \
    || die "/dev/net/tun is missing. Load the module: sudo modprobe tun"

  up_bridge "$bridge" "$address"
  for tap in "$@"; do
    up_tap "$tap" "$bridge" "$user"
  done
  up_forwarding

  chain=$(forward_chain)
  step "firewall rules (forwarding in $chain)"
  while read -r table rule_chain rule; do
    # $rule is a list of iptables arguments built by rules() from this script's
    # own constants and the two validated settings; it is split on purpose.
    # shellcheck disable=SC2086
    add_rule "$table" "$rule_chain" $rule
  done < <(rules "$chain" "$bridge" "$address")
  up_firewalld "$bridge"
  warn_foreign_drops "$bridge" "$address"

  # The outcome guard: everything above can succeed and the lab still be
  # unusable (a rule removed by a firewall manager, an address that did not
  # stick). Checking the result is cheaper than debugging a VM without network.
  status "$bridge" "$address" "$@" >/dev/null \
    || die "the lab network is not in the state up just asked for. See: $SCRIPT status $*"
  echo "lab network up: $bridge ${address} (${#} taps, owner $user)" >&2
}

down() {
  local bridge=$1 address=$2
  local chain dev table rule_chain rule

  # Rules first, from both chains they can live in: docker may have appeared or
  # gone since up, and the rule sits where up found the chain then.
  for chain in DOCKER-USER FORWARD; do
    as_root iptables -w -n -L "$chain" >/dev/null 2>&1 || continue
    while read -r table rule_chain rule; do
      # shellcheck disable=SC2086 # see up(): a list of iptables arguments.
      delete_rule "$table" "$rule_chain" $rule
    done < <(rules "$chain" "$bridge" "$address")
  done
  while read -r table rule_chain rule; do
    # shellcheck disable=SC2086 # see up(): a list of iptables arguments.
    delete_rule "$table" "$rule_chain" $rule
  done < <(retired_rules "$address")
  down_firewalld "$bridge"

  if link_exists "$bridge"; then
    link_is_ours "$bridge" || die "$bridge was not made by this script, leaving it alone.
  Remove it yourself if it is a leftover: sudo ip link del $bridge"
    for dev in "/sys/class/net/$bridge/brif"/*; do
      [[ -e $dev ]] || continue
      dev=${dev##*/}
      if link_is_ours "$dev"; then
        step "remove tap $dev"
        as_root ip link del dev "$dev"
      else
        # Somebody else's port on our bridge: detach it rather than delete it.
        step "detach $dev (not made by this script)"
        as_root ip link set dev "$dev" nomaster
      fi
    done
    step "remove bridge $bridge"
    as_root ip link del dev "$bridge"
  fi

  if [[ -e $STATE_DIR/ip_forward ]]; then
    step "disable IPv4 forwarding (up had enabled it)"
    as_root sysctl -q -w net.ipv4.ip_forward=0
  fi
  as_root rm -rf "$STATE_DIR"
  echo "lab network down" >&2
}

# Prints one line per part to stdout and returns non-zero when a part is missing.
status() {
  local bridge=$1 address=$2
  shift 2
  local missing=0 chain tap table rule_chain rule zone family hook

  if link_exists "$bridge" && ip -o -4 addr show dev "$bridge" | grep -qF " $address "; then
    echo "bridge   $bridge $address"
  else
    echo "bridge   $bridge MISSING"
    missing=1
  fi
  for tap in "$@"; do
    if [[ -e /sys/class/net/$bridge/brif/$tap ]]; then
      echo "tap      $tap"
    else
      echo "tap      $tap MISSING"
      missing=1
    fi
  done
  if [[ $(cat /proc/sys/net/ipv4/ip_forward) == 1 ]]; then
    echo "forward  net.ipv4.ip_forward=1"
  else
    echo "forward  net.ipv4.ip_forward=0 MISSING"
    missing=1
  fi
  chain=$(forward_chain)
  while read -r table rule_chain rule; do
    # shellcheck disable=SC2086 # see up(): a list of iptables arguments.
    if rule_present "$table" "$rule_chain" $rule; then
      echo "rule     $table $rule_chain $rule"
    else
      echo "rule     $table $rule_chain $rule MISSING"
      missing=1
    fi
  done < <(rules "$chain" "$bridge" "$address")
  if firewalld_running; then
    zone=$(firewalld_zone "$bridge")
    if [[ $zone == trusted ]]; then
      echo "firewall firewalld: $bridge in the zone trusted"
    else
      echo "firewall firewalld: $bridge in the zone ${zone:-<none, so the default zone>} MISSING (want trusted)"
      missing=1
    fi
  fi
  while read -r family table rule_chain hook; do
    [[ -n $rule_chain ]] || continue
    echo "firewall nftables $family $table $rule_chain drops $hook traffic by policy and does not mention $bridge (see $SCRIPT up)"
  done < <(foreign_drop_chains "$bridge")
  return "$missing"
}

main() {
  local verb=${1:-}
  [[ $# -gt 0 ]] && shift
  case "$verb" in
    up)
      preflight up "$@"
      up "$BRIDGE" "$ADDRESS" "$TAP_USER" "$@"
      ;;
    down)
      preflight down
      down "$BRIDGE" "$ADDRESS"
      ;;
    status)
      preflight status "$@"
      status "$BRIDGE" "$ADDRESS" "$@" \
        || die "the lab network is not (completely) up. Bring it up: mise run lab:up"
      ;;
    -h | --help | help)
      usage
      ;;
    *)
      usage
      exit 1
      ;;
  esac
}

main "$@"
