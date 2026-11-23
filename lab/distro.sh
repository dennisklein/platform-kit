# shellcheck shell=bash
# Sourced by lab/lab.sh and lab/net.sh, never run: what the workstation's
# distribution calls the packages the lab needs. Every fix the two scripts
# print goes through here, so a RHEL host is not told to run apt-get.

# Prints the distribution family named in an os-release file: debian, fedora,
# rhel or unknown. Rocky, Alma, CentOS Stream and Oracle Linux name rhel in
# ID_LIKE and are rhel here; Fedora is told apart because it packages QEMU the
# upstream way.
os_family() {
  local os_release=$1 id="" like=""
  if [[ -r $os_release ]]; then
    # shellcheck disable=SC1090 # a distribution file, read in a subshell
    id=$(. "$os_release" && echo "${ID:-}")
    # shellcheck disable=SC1090
    like=$(. "$os_release" && echo "${ID_LIKE:-}")
  fi
  case "$id" in
    fedora) echo fedora; return ;;
    debian | ubuntu) echo debian; return ;;
    rhel | centos | rocky | almalinux | ol) echo rhel; return ;;
  esac
  case " $like " in
    *" rhel "* | *" centos "*) echo rhel ;;
    *" fedora "*) echo fedora ;;
    *" debian "* | *" ubuntu "*) echo debian ;;
    *) echo unknown ;;
  esac
}

# Prints the command that installs a set of packages here. The arguments are
# the package lists for Debian/Ubuntu, for RHEL and its rebuilds, and for
# Fedora (default: the RHEL list).
install_hint() {
  local deb=$1 rhel=$2 fedora=${3:-$2}
  case "$(os_family /etc/os-release)" in
    debian) echo "sudo apt-get install -y $deb" ;;
    rhel) echo "sudo dnf install -y $rhel" ;;
    fedora) echo "sudo dnf install -y $fedora" ;;
    *) echo "sudo apt-get install -y $deb (Debian, Ubuntu), or sudo dnf install -y $rhel (RHEL, Rocky, Alma)" ;;
  esac
}
