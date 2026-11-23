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
set -euo pipefail

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
  need ip "Install it: sudo apt-get install -y iproute2"
  need iptables "Install it: sudo apt-get install -y iptables"
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
  # (ufw, firewalld) would silently drop the operator's Redfish calls.
  echo "filter INPUT -d ${address%/*}/32 -j ACCEPT"
  # Egress for the VMs: image pulls, time, DNS.
  echo "nat POSTROUTING -s $subnet ! -o $bridge -j MASQUERADE"
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
  Remove it (sudo ip link del $tap) or rename the tap in lab/fleet.yaml."
  else
    step "tap $tap (owner $user)"
    as_root ip tuntap add dev "$tap" mode tap user "$user"
    as_root ip link set dev "$tap" alias "$MARK"
  fi
  as_root ip link set dev "$tap" master "$bridge"
  as_root ip link set dev "$tap" up
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
  local missing=0 chain tap table rule_chain rule

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
