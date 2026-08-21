#!/usr/bin/env bash
set -euo pipefail

# Runs a self-hosted GitHub Actions runner inside a throwaway QEMU virtual
# machine.
#
# The VM does the same download / configure / run procedure GitHub shows on the
# Settings > Actions > Runners > New self-hosted runner page: it downloads the
# runner package, verifies its checksum, extracts it, registers with the token
# you give it, and runs it.
#
# The VM is tied to this process. Stop it with Ctrl-C, or send it a signal, and
# the machine is powered off and its disk deleted. Nothing a job did survives,
# which is the point: a long-lived runner accumulates state from every job it
# has run, and a VM is a much harder boundary than a container.
#
# A job gets:
#   - a Docker daemon of its own, inside the VM, rather than a socket to the
#     host's, which is equivalent to root on the host
#   - /dev/kvm, so jobs can boot VMs of their own (nested virtualisation)
#   - a kernel of its own to break
#
# Usage:
#   ./runner-vm.sh [command] [flags]
#
# Commands:
#   run       Boot a VM and run a runner in it until stopped (the default)
#   build     Build the golden image the VMs boot from
#   doctor    Check the host for KVM, nested virtualisation and QEMU
#   list      Show the VMs on this host and the services managing them
#   clean     Stop and delete VMs. Named ones only, or all of them when none is
#             named. Keeps the images unless --all is given
#   install   Install the script, the systemd unit and the service user, build
#             the image and start a runner. Run as root
#   uninstall Remove everything: services, unit, configuration, state and this
#             script
#   print-unit  Print the systemd unit install would write
#
# Flags for run (each has an environment variable equivalent, so an existing
# .env file in the working directory is read if present):
#   --url URL             GITHUB_URL       repository or organisation
#   --token TOKEN         RUNNER_TOKEN     registration token
#   --github-token PAT    GITHUB_TOKEN     PAT, to mint a token automatically
#   --github-token-file F GITHUB_TOKEN_FILE  read the PAT from a file, or "-"
#                                            for stdin, so it is never on a
#                                            command line or in shell history
#   --token-file FILE     RUNNER_TOKEN_FILE  same, for a registration token
#   --app-id ID           GITHUB_APP_ID    GitHub App id, instead of a PAT
#   --app-key FILE        GITHUB_APP_PRIVATE_KEY   the app's PEM private key
#   --name NAME           RUNNER_NAME      runner name
#   --labels a,b          RUNNER_LABELS    extra labels
#   --group GROUP         RUNNER_GROUP     runner group
#   --cpus N              VM_CPUS          vCPUs           (default 2)
#   --memory MB           VM_MEMORY_MB     memory in MiB   (default 4096)
#   --disk GB             VM_DISK_GB       disk in GiB     (default 40)
#   --ephemeral           EPHEMERAL        take one job, then stop
#   --no-nested           VM_NESTED=false  do not expose vmx/svm to the VM
#   --env-file FILE                        default .env, when present
#
# Flags for install:
#   --name NAME           the runner to install and enable (default runner-1).
#                         Its settings go to /etc/runner-vm/env.NAME, so
#                         installing again under another name adds a runner
#                         rather than repointing the ones already there
#   --no-build            do not build the golden image
#   --no-start            do not enable and start the service
#
# Flags for clean:
#   [NAME...]             remove only these VMs, leaving the others alone
#   --all                 also delete the images and the ssh key, i.e. all of
#                         the state directory
#   -y, --yes             do not ask for confirmation
#
# Several repositories on one host:
#   Every runner is an instance of one unit template, and each reads its own
#   configuration, so run install once per repository with a different --name:
#
#     sudo ./runner-vm.sh install --service --name web \
#       --url https://github.com/OWNER/web --github-token-file /root/pat
#     sudo ./runner-vm.sh install --service --name api \
#       --url https://github.com/OWNER/api
#
#   The second one reuses the credential already on the host; pass a token of
#   its own if that repository needs a different one. What ends up where:
#
#     /etc/runner-vm/env             defaults shared by every runner
#     /etc/runner-vm/env.NAME        one runner's settings, read after the above
#     /etc/runner-vm/creds/pat       the credential runners use by default
#     /etc/runner-vm/creds/pat.NAME  one runner's own credential, when it needs
#                                    one (likewise app.pem and app.NAME.pem)
#
# Examples:
#   ./runner-vm.sh --url https://github.com/runyard-ai --token AAA...
#   GITHUB_TOKEN=ghp_... ./runner-vm.sh --url https://github.com/runyard-ai
#   ./runner-vm.sh --url https://github.com/OWNER/REPO --github-token-file /root/pat
#   ./runner-vm.sh --url https://github.com/runyard-ai --app-id 123456 \
#     --app-key /root/app.pem
#   ./runner-vm.sh build --force
#   ./runner-vm.sh clean runner-2
#   ./runner-vm.sh clean --all --yes
#   sudo ./runner-vm.sh install --url https://github.com/OWNER/REPO \
#     --github-token-file /root/pat
#   sudo ./runner-vm.sh install --service --name api \
#     --url https://github.com/OWNER/api
#   ./runner-vm.sh list
#   sudo ./runner-vm.sh uninstall

VERSION=0.1.0

# --------------------------------------------------------------------------
# Configuration
# --------------------------------------------------------------------------

STATE_DIR=${RUNNER_VM_HOME:-${XDG_DATA_HOME:-$HOME/.local/share}/runner-vm}
IMAGES_DIR=$STATE_DIR/images
SSH_KEY=$STATE_DIR/ssh/id_ed25519

# Version of the runner package to bake into the image. See
# https://github.com/actions/runner/releases for the checksums below.
RUNNER_VERSION=${RUNNER_VERSION:-2.336.0}
RUNNER_SHA256_X64=${RUNNER_SHA256_X64:-04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d}
RUNNER_SHA256_ARM64=${RUNNER_SHA256_ARM64:-58b758e420b87093fbd4bfddd368074960053e2f1388f01848c82624b90f27d1}

# Ubuntu cloud image series to base the golden image on.
UBUNTU_RELEASE=${UBUNTU_RELEASE:-noble}

# Size of the golden image. It has to hold the runner, Docker, the toolchain
# below and whatever a job pulls; the VM disk on top of it is sized separately
# with --disk.
GOLDEN_SIZE_GB=${GOLDEN_SIZE_GB:-30}

# What the golden image ships with.
#
# A GitHub-hosted runner comes with an enormous toolchain preinstalled, and
# workflows written against one quietly assume a lot of it is simply there —
# which is why a job that needed no "apt-get install" step on a hosted runner
# suddenly needs one here. The full image is around 90 GB and cannot be
# reproduced, so this is the practical subset: compilers, the interpreters and
# the utilities that workflows reach for without thinking.
#
# Anything language-version specific is deliberately left out: setup-node,
# setup-python, setup-go and setup-java download into the tool cache at job
# time and work exactly as they do on a hosted runner.
# The list below is the apt section of the ubuntu-24.04 image manifest at
# https://github.com/actions/runner-images, plus what a VM needs that a hosted
# runner gets from its host. It is kept close to that manifest on purpose: the
# gap between the two is exactly what makes a job fail here after passing on a
# hosted runner, and "make: command not found" is a poor way to discover it.
#
# Not everything there is worth carrying — Android SDKs, browsers, several
# JDKs and the cloud CLIs are tens of gigabytes — and language versions are
# left to setup-node, setup-python, setup-go and setup-java, which download
# into the tool cache at job time exactly as they do on a hosted runner.
RUNNER_PACKAGES_DEFAULT="\
ca-certificates curl wget gnupg gnupg2 software-properties-common apt-transport-https \
git git-lfs mercurial openssh-client ssh sshpass rsync \
build-essential pkg-config cmake autoconf automake libtool make patch \
bison flex swig texinfo m4 fakeroot dpkg-dev rpm patchelf upx \
python3 python3-pip python3-venv python3-dev python3-setuptools python-is-python3 \
nodejs npm \
jq xz-utils bzip2 zstd lz4 brotli pigz unzip zip tar p7zip-full p7zip-rar aria2 zsync \
libssl-dev zlib1g-dev libffi-dev libbz2-dev libreadline-dev libsqlite3-dev \
libcurl4-openssl-dev libxml2-dev libicu-dev libyaml-dev libnss3-tools sqlite3 \
dnsutils iputils-ping netcat-openbsd net-tools iproute2 telnet ftp \
file tree time parallel moreutils shellcheck acl dbus haveged xvfb mediainfo \
 tk sphinxsearch systemd-coredump pollinate \
locales tzdata sudo lsb-release uuid-runtime fonts-noto-color-emoji \
docker.io docker-compose-v2 \
qemu-system-x86 qemu-utils cpu-checker"

# Added to the list above, for whatever a given fleet also needs:
#   EXTRA_PACKAGES="ffmpeg imagemagick" ./runner-vm.sh build
EXTRA_PACKAGES=${EXTRA_PACKAGES:-}
RUNNER_PACKAGES=${RUNNER_PACKAGES:-$RUNNER_PACKAGES_DEFAULT}

VM_CPUS=${VM_CPUS:-2}
VM_MEMORY_MB=${VM_MEMORY_MB:-4096}
VM_DISK_GB=${VM_DISK_GB:-40}
VM_NESTED=${VM_NESTED:-true}

GITHUB_URL=${GITHUB_URL:-}
RUNNER_TOKEN=${RUNNER_TOKEN:-}
GITHUB_TOKEN=${GITHUB_TOKEN:-${GH_TOKEN:-}}
# A GitHub App is the alternative to a PAT: it belongs to the organisation
# rather than to a person, so it neither expires nor leaves with anyone.
GITHUB_APP_ID=${GITHUB_APP_ID:-}
GITHUB_APP_PRIVATE_KEY=${GITHUB_APP_PRIVATE_KEY:-}
GITHUB_APP_INSTALLATION_ID=${GITHUB_APP_INSTALLATION_ID:-}
# Credentials can come from a file instead, so that nothing sensitive is typed
# on a command line, where ps would show it to every user on the machine, or
# into a shell, where the history file would keep it. "-" reads stdin.
GITHUB_TOKEN_FILE=${GITHUB_TOKEN_FILE:-}
RUNNER_TOKEN_FILE=${RUNNER_TOKEN_FILE:-}
RUNNER_NAME=${RUNNER_NAME:-}
RUNNER_LABELS=${RUNNER_LABELS:-}
RUNNER_GROUP=${RUNNER_GROUP:-Default}
EPHEMERAL=${EPHEMERAL:-false}
DISABLE_UPDATE=${DISABLE_UPDATE:-false}

# Where the script fetches itself from when it was piped into bash and so has
# no file of its own to install. Override when installing from a branch, or
# install would fetch a different version than the one being run.
SCRIPT_URL=${SCRIPT_URL:-https://raw.githubusercontent.com/clems4ever/github-runner/main/runner-vm.sh}

# Where "install" puts things, and who the service runs as.
INSTALL_BIN=${INSTALL_BIN:-/usr/local/bin/runner-vm.sh}
SERVICE_USER=${SERVICE_USER:-runner-vm}
SERVICE_STATE=${SERVICE_STATE:-/var/lib/runner-vm}

# The configuration the service reads. One unit template serves every runner on
# the host, so the per-runner settings cannot live in the unit: ETC_DIR/env
# holds what they share and ETC_DIR/env.NAME what one of them overrides, which
# is how a single host serves several repositories.
#
# Overridable so the tests can exercise the layout without writing to /etc.
ETC_DIR=${RUNNER_VM_ETC:-/etc/runner-vm}
# Credentials sit in a directory of their own, because the unit hands the whole
# directory to systemd and every file in it becomes a credential.
CRED_DIR="${ETC_DIR}/creds"
# systemd names a credential loaded from a directory "<id>_<filename>".
CRED_ID=runner-vm

ENV_FILE=${ENV_FILE:-.env}
FORCE=false
CLEAN_ALL=false
# The VMs "clean" was asked to remove; empty means all of them.
CLEAN_TARGETS=()
ASSUME_YES=false
# install does the whole job by default: a host with a runner on it, not a host
# with the pieces of one.
INSTALL_SERVICE=false
INSTALL_BUILD=true
INSTALL_START=true

# print_help echoes the comment block at the top of this file, so the usage
# text and the documentation cannot drift apart.
print_help() {
  # Piped from curl there is no file to read the comment block out of.
  local self=${BASH_SOURCE[0]:-}
  if [[ -z "$self" || ! -f "$self" ]]; then
    echo "runner-vm ${VERSION} — self-hosted GitHub Actions runners in QEMU VMs"
    echo "Commands: run, build, list, doctor, install, uninstall, clean, print-unit"
    echo "Full help: ${SCRIPT_URL}"
    return 0
  fi
  # Skips the shebang and "set" line, then prints the comment block, stopping
  # at the first line that is not a comment once the block has started — the
  # blank line between the two is why this cannot simply stop at line three.
  awk 'NR <= 2 { next }
       /^#/    { seen = 1; sub(/^# ?/, ""); print; next }
       seen    { exit }' "$self"
}

log()  { echo "[runner-vm] $*"; }
warn() { echo "[runner-vm] warning: $*" >&2; }
die()  { echo "[runner-vm] error: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# pid_alive reports whether a process is still running, without needing
# permission to signal it.
#
# "kill -0" answers a different question: it fails with EPERM on a process
# belonging to another user, which bash reports the same way as "no such
# process". The service runs its VMs as ${SERVICE_USER}, so an unprivileged
# "list" saw every one of them as stopped — with no uptime, since that is read
# in the same branch — while systemd plainly said the service was active.
pid_alive() {
  local pid=${1:-}
  [[ -n "$pid" ]] || return 1
  if [[ -d /proc ]]; then
    [[ -d "/proc/${pid}" ]]
  else
    kill -0 "$pid" 2>/dev/null
  fi
}

# image_virtual_bytes prints the size the guest sees, in bytes.
#
# The human-readable line carries both a rounded figure and the exact one, and
# the exact one is what a comparison needs. It is read rather than the JSON
# output because only the top-level image has this line, whereas
# "virtual-size" appears once per node in the JSON and picking the first match
# depends on the field order of the qemu-img in use.
image_virtual_bytes() {
  qemu-img info "$1" 2>/dev/null \
    | sed -n 's/^virtual size:.*(\([0-9]\{1,\}\) bytes).*/\1/p' | head -1
}

# --------------------------------------------------------------------------
# Host capabilities
# --------------------------------------------------------------------------

# The CPU vendor decides both the name of the KVM module and the CPU flag that
# carries virtualisation support, and asking QEMU for the wrong flag makes it
# refuse to start — so it is read from the CPU rather than guessed.
cpu_vendor() {
  if grep -qm1 '^flags.*\bvmx\b' /proc/cpuinfo 2>/dev/null; then echo intel
  elif grep -qm1 '^flags.*\bsvm\b' /proc/cpuinfo 2>/dev/null; then echo amd
  else echo unknown
  fi
}

kvm_module() {
  case "$(cpu_vendor)" in
    intel) echo kvm_intel ;;
    amd)   echo kvm_amd ;;
    *)     echo "" ;;
  esac
}

nested_flag() {
  case "$(cpu_vendor)" in
    intel) echo vmx ;;
    amd)   echo svm ;;
    *)     echo "" ;;
  esac
}

# Nested virtualisation needs the host's KVM module loaded with nested=1. Guests
# then see vmx/svm through "-cpu host" and can run VMs of their own.
nested_enabled() {
  local mod; mod=$(kvm_module)
  [[ -n "$mod" ]] || return 1
  local value
  value=$(cat "/sys/module/${mod}/parameters/nested" 2>/dev/null || echo N)
  [[ "$value" == "Y" || "$value" == "y" || "$value" == "1" ]]
}

qemu_binary() {
  case "$(uname -m)" in
    aarch64|arm64) echo qemu-system-aarch64 ;;
    *)             echo qemu-system-x86_64 ;;
  esac
}

# seed_tool reports which of the usual ISO builders is installed. cloud-init
# reads its configuration from a small ISO labelled "cidata"; building one is
# the single thing this script cannot do on its own.
seed_tool() {
  for tool in cloud-localds genisoimage xorriso mkisofs; do
    if have "$tool"; then echo "$tool"; return 0; fi
  done
  return 1
}

install_hint() {
  if have apt-get; then
    echo "  sudo apt-get install -y qemu-system-x86 qemu-utils cloud-image-utils openssh-client curl"
  elif have dnf; then
    echo "  sudo dnf install -y qemu-kvm qemu-img cloud-utils openssh-clients curl"
  elif have pacman; then
    echo "  sudo pacman -S qemu-full cloud-image-utils openssh curl"
  else
    echo "  install: qemu-system-x86, qemu-img, cloud-image-utils (or genisoimage)"
  fi
}

cmd_doctor() {
  local failed=0 vendor mod
  vendor=$(cpu_vendor)
  mod=$(kvm_module)
  echo "host: $(uname -m), ${vendor} CPU"
  echo

  if [[ "$vendor" == unknown ]]; then
    echo "  [FAIL] cpu        no vmx/svm flag in /proc/cpuinfo"
    echo "         enable virtualisation in the firmware, or — if this host is itself a"
    echo "         VM — enable nested virtualisation for it"
    failed=$((failed + 1))
  else
    echo "  [ ok ] cpu        ${vendor}"
  fi

  if [[ ! -e /dev/kvm ]]; then
    echo "  [FAIL] kvm        /dev/kvm is missing"
    echo "         load the module: sudo modprobe ${mod:-kvm}"
    failed=$((failed + 1))
  elif [[ ! -r /dev/kvm || ! -w /dev/kvm ]]; then
    echo "  [FAIL] kvm        /dev/kvm is not readable and writable by $(id -un)"
    echo "         sudo usermod -aG $(stat -c %G /dev/kvm) \"\$USER\"   # then log in again"
    failed=$((failed + 1))
  else
    echo "  [ ok ] kvm        /dev/kvm"
  fi

  if nested_enabled; then
    echo "  [ ok ] nested     enabled, VMs get $(nested_flag)"
  else
    local mark="warn"
    [[ "$VM_NESTED" == "true" ]] && { mark="FAIL"; failed=$((failed + 1)); }
    echo "  [$mark] nested     disabled: jobs will not be able to run VMs"
    echo "         sudo modprobe -r ${mod} && sudo modprobe ${mod} nested=1"
    echo "         echo 'options ${mod} nested=1' | sudo tee /etc/modprobe.d/${mod}.conf"
    echo "         or run with --no-nested"
  fi

  local qemu; qemu=$(qemu_binary)
  if have "$qemu"; then
    echo "  [ ok ] qemu       $(command -v "$qemu") ($("$qemu" --version | head -1 | sed 's/.*version //;s/ .*//'))"
  else
    echo "  [FAIL] qemu       ${qemu} not found"
    install_hint
    failed=$((failed + 1))
  fi

  for tool in qemu-img curl ssh; do
    if have "$tool"; then
      echo "  [ ok ] ${tool}$(printf '%*s' $((11 - ${#tool})) '')$(command -v "$tool")"
    else
      echo "  [FAIL] ${tool}$(printf '%*s' $((11 - ${#tool})) '')not found"
      install_hint
      failed=$((failed + 1))
    fi
  done

  if seed_tool >/dev/null; then
    echo "  [ ok ] seed       $(seed_tool)"
  else
    echo "  [FAIL] seed       none of cloud-localds, genisoimage, xorriso, mkisofs found"
    install_hint
    failed=$((failed + 1))
  fi

  echo
  echo "state directory: ${STATE_DIR}"
  local golden; golden=$(golden_path)
  if [[ -f "$golden" ]]; then
    echo "golden image:    ${golden}"
  else
    echo "golden image:    not built yet (run: $0 build)"
  fi

  if [[ $failed -gt 0 ]]; then
    echo
    die "${failed} check(s) failed"
  fi
  echo
  log "this host can run runner VMs"
}

require_host() {
  have "$(qemu_binary)" || { install_hint >&2; die "$(qemu_binary) not found"; }
  have qemu-img || { install_hint >&2; die "qemu-img not found"; }
  seed_tool >/dev/null || { install_hint >&2; die "no ISO builder found (cloud-localds, genisoimage, xorriso or mkisofs)"; }
  # Checked here rather than where they are used: discovering a missing tool
  # part-way through provisioning an image wastes the minutes already spent.
  have curl || { install_hint >&2; die "curl not found"; }
  have ssh-keygen || { install_hint >&2; die "ssh-keygen not found (install openssh-client)"; }
  [[ -r /dev/kvm && -w /dev/kvm ]] || die "/dev/kvm is not usable by $(id -un); run '$0 doctor'"
  if [[ "$VM_NESTED" == "true" ]] && ! nested_enabled; then
    die "nested virtualisation is not enabled on this host; run '$0 doctor', or pass --no-nested"
  fi
}

# --------------------------------------------------------------------------
# Images
# --------------------------------------------------------------------------

deb_arch() {
  case "$(uname -m)" in
    aarch64|arm64) echo arm64 ;;
    *)             echo amd64 ;;
  esac
}

# The architecture names GitHub uses for the runner package.
runner_arch() {
  case "$(uname -m)" in
    aarch64|arm64) echo arm64 ;;
    *)             echo x64 ;;
  esac
}

runner_sha256() {
  case "$(runner_arch)" in
    arm64) echo "$RUNNER_SHA256_ARM64" ;;
    *)     echo "$RUNNER_SHA256_X64" ;;
  esac
}

cloud_image_name() { echo "${UBUNTU_RELEASE}-server-cloudimg-$(deb_arch).img"; }
cloud_image_url()  { echo "https://cloud-images.ubuntu.com/${UBUNTU_RELEASE}/current/$(cloud_image_name)"; }
cloud_image_path() { echo "${IMAGES_DIR}/$(cloud_image_name)"; }

effective_packages() {
  echo "${RUNNER_PACKAGES} ${EXTRA_PACKAGES}" | tr ' ' '\n' | sed '/^$/d' | sort -u
}

# packages_hash goes in the image name so that changing the package list builds
# a new image rather than silently reusing one without the new tools in it.
packages_hash() {
  effective_packages | sha256sum | cut -c1-8
}

# The golden image is named after everything baked into it, so bumping the
# runner version, the release or the package list builds a new one instead of
# silently reusing the old.
golden_path() {
  echo "${IMAGES_DIR}/golden-${UBUNTU_RELEASE}-$(deb_arch)-runner${RUNNER_VERSION}-$(packages_hash).qcow2"
}

# pull_cloud_image downloads the base image and checks it against the
# checksums Canonical publishes next to it.
pull_cloud_image() {
  local dest; dest=$(cloud_image_path)
  if [[ -f "$dest" && "$FORCE" != "true" ]]; then
    log "using cached $(basename "$dest")"
    return 0
  fi

  mkdir -p "$IMAGES_DIR"
  local want
  want=$(curl -fsSL "https://cloud-images.ubuntu.com/${UBUNTU_RELEASE}/current/SHA256SUMS" \
    | awk -v f="*$(cloud_image_name)" '$2 == f { print $1 }')
  [[ -n "$want" ]] || die "cannot find $(cloud_image_name) in the published SHA256SUMS"

  log "downloading $(cloud_image_url)"
  curl -fL --progress-bar -o "${dest}.partial" "$(cloud_image_url)"

  local got
  got=$(sha256sum "${dest}.partial" | cut -d' ' -f1)
  if [[ "$got" != "$want" ]]; then
    rm -f "${dest}.partial"
    die "checksum mismatch for $(cloud_image_name): published ${want}, downloaded ${got}"
  fi
  mv "${dest}.partial" "$dest"
  log "verified sha256 ${got}"
}

# make_seed builds a cloud-init NoCloud ISO from a user-data file. The volume
# label has to be "cidata": that is what cloud-init looks for.
make_seed() {
  local user_data=$1 meta_data=$2 out=$3
  case "$(seed_tool)" in
    cloud-localds)
      cloud-localds "$out" "$user_data" "$meta_data"
      ;;
    genisoimage|mkisofs)
      # -J -r give Joliet and Rock Ridge, so the guest sees the file names
      # verbatim rather than mangled into 8.3 form.
      "$(seed_tool)" -output "$out" -volid cidata -joliet -rock -quiet \
        -graft-points "user-data=${user_data}" "meta-data=${meta_data}"
      ;;
    xorriso)
      xorriso -as mkisofs -output "$out" -volid cidata -joliet -rock -quiet \
        -graft-points "user-data=${user_data}" "meta-data=${meta_data}"
      ;;
    *)
      die "no ISO builder found"
      ;;
  esac
}

# Several of these can run at once on one host — that is the point of naming
# VMs — but a few things are shared: the golden image, the build directory and
# the ssh key. flock serialises those, so the second VM waits for the first to
# finish building rather than both writing the same files.
acquire_lock() {
  mkdir -p "$STATE_DIR"
  exec 9>"${STATE_DIR}/.lock"
  if ! flock -w "${LOCK_TIMEOUT:-3600}" 9; then
    die "timed out waiting for another runner-vm on this host to finish building"
  fi
}

release_lock() {
  flock -u 9 2>/dev/null || true
  exec 9>&- 2>/dev/null || true
}

ensure_ssh_key() {
  if [[ ! -f "$SSH_KEY" ]]; then
    mkdir -p "$(dirname "$SSH_KEY")"
    ssh-keygen -t ed25519 -N '' -C runner-vm -f "$SSH_KEY" >/dev/null
    log "generated an ssh key at ${SSH_KEY}"
  fi
}

# --------------------------------------------------------------------------
# Golden image
# --------------------------------------------------------------------------

BUILD_OK_SENTINEL="RUNNER-VM-BUILD-OK"
BUILD_FAIL_SENTINEL="RUNNER-VM-BUILD-FAILED"

# sentinel_seen looks for a sentinel the guest echoed to its console, and only
# at the start of a line. The provisioning script runs under "set -x", so its
# trace contains the trap that would print the failure sentinel — matching that
# anywhere in the log would abort every build the moment provisioning started.
sentinel_seen() {
  local sentinel=$1 console=$2
  grep -qE "^${sentinel}" "$console" 2>/dev/null
}

# build_provision_script is what actually provisions the image: the runner
# package and its dependencies, a Docker daemon, and the packages a job needs
# to use /dev/kvm.
#
# It is shipped as a file of its own rather than inlined into runcmd because
# cloud-init writes runcmd into a script that already starts with "#!/bin/sh" —
# dash on Ubuntu — where neither "set -o pipefail" nor an ERR trap exists.
build_provision_script() {
  cat <<EOF
#!/bin/bash
set -euxo pipefail

# Report failure rather than bake a half-provisioned image: the wrapper watches
# the console for one of these two lines.
trap 'echo "${BUILD_FAIL_SENTINEL}" > /dev/console; sync; poweroff -f' ERR

# A cloud image runs unattended-upgrades on boot, which holds the apt lock and
# would collide with a job installing packages.
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer \
  unattended-upgrades.service 2>/dev/null || true

# The runner refuses to run as root. 1001 because the cloud image already ships
# uid 1000 as "ubuntu".
id runner >/dev/null 2>&1 || useradd --create-home --shell /bin/bash --uid 1001 runner
echo "runner ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/runner
chmod 0440 /etc/sudoers.d/runner

# Docker for jobs: a daemon of the VM's own, so nothing is shared with the host
# the way a mounted docker.sock would be.
usermod -aG docker runner
systemctl enable docker

# /dev/kvm inside the VM is rw for the kvm group only, so jobs can only use
# nested virtualisation if the runner is a member.
getent group kvm >/dev/null || groupadd -r kvm
usermod -aG kvm runner

# The GitHub CLI is on a hosted runner and is not in the Ubuntu archive, so it
# needs its own repository. Workflows use it for releases and for anything
# awkward to express as an action.
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
echo "deb [arch=\$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
  > /etc/apt/sources.list.d/github-cli.list
apt-get update
apt-get install -y --no-install-recommends gh

# A hosted runner has a UTF-8 locale, and tools that format output assume one.
locale-gen en_US.UTF-8
update-locale LANG=en_US.UTF-8

# Download the runner package, verify its checksum, extract it and
# install its dependencies, at the path the guest runner script expects.
install -d -o runner -g runner /home/runner/actions-runner /home/runner/_work
cd /home/runner/actions-runner
tarball="actions-runner-linux-$(runner_arch)-${RUNNER_VERSION}.tar.gz"
curl -fsSL --retry 3 --retry-delay 5 -o "\$tarball" \
  "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/\${tarball}"
echo "$(runner_sha256)  \${tarball}" | sha256sum -c -
tar xzf "\$tarball"
rm -f "\$tarball"
./bin/installdependencies.sh
chown -R runner:runner /home/runner

# Reset cloud-init so the next boot, from a fresh seed, counts as a new instance
# and reruns the modules that configure the runner.
cloud-init clean --logs --seed
echo "${BUILD_OK_SENTINEL}" > /dev/console
sync
poweroff -f
EOF
}

build_user_data() {
  cat <<EOF
#cloud-config
hostname: runner-vm-build

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $(cat "${SSH_KEY}.pub")

package_update: true
package_upgrade: true
packages:
$(effective_packages | sed 's/^/  - /')

write_files:
  - path: /usr/local/bin/runner-vm-provision.sh
    permissions: '0755'
    owner: 'root:root'
    content: |
$(build_provision_script | sed 's/^/      /')

runcmd:
  - [ /usr/local/bin/runner-vm-provision.sh ]
EOF
}

cmd_build() {
  require_host
  mkdir -p "$IMAGES_DIR"

  # Held for the whole build: two VMs starting together on a host with no image
  # yet would otherwise both provision into the same directory. The second one
  # waits here and finds the finished image below.
  acquire_lock
  trap release_lock RETURN

  ensure_ssh_key

  local golden; golden=$(golden_path)
  if [[ -f "$golden" && "$FORCE" != "true" ]]; then
    log "golden image already built: ${golden}"
    log "rebuild it with: $0 build --force"
    return 0
  fi

  pull_cloud_image

  # Built in the state directory rather than /tmp: the image is several
  # gigabytes, and /tmp is a tmpfs — that is, memory — on many distributions.
  local work="${STATE_DIR}/build"
  rm -rf "$work"
  mkdir -p "$work"

  build_user_data > "${work}/user-data"
  echo -e "instance-id: runner-vm-build-$(date +%s)\nlocal-hostname: runner-vm-build" > "${work}/meta-data"
  make_seed "${work}/user-data" "${work}/meta-data" "${work}/seed.iso"

  # Build into a copy, and only publish it as the golden image once the guest
  # has said it finished, so an interrupted build cannot leave a broken image
  # behind for the next run to boot.
  log "provisioning the golden image (a few minutes: it installs Docker, QEMU and the runner)"
  qemu-img convert -O qcow2 "$(cloud_image_path)" "${work}/golden.qcow2"
  qemu-img resize -q "${work}/golden.qcow2" "${GOLDEN_SIZE_GB}G"

  local console="${work}/console.log"
  : > "$console"

  "$(qemu_binary)" \
    -name runner-vm-build \
    -machine "$(qemu_machine)" \
    -cpu host \
    -accel kvm \
    -smp 2 \
    -m 2048 \
    -drive "file=${work}/golden.qcow2,if=virtio,format=qcow2,cache=writeback" \
    -drive "file=${work}/seed.iso,if=virtio,format=raw,readonly=on" \
    -netdev user,id=net0 \
    -device virtio-net-pci,netdev=net0 \
    -device virtio-rng-pci \
    -display none \
    -serial "file:${console}" \
    -no-reboot \
    -pidfile "${work}/qemu.pid" \
    -daemonize

  local pid; pid=$(cat "${work}/qemu.pid")
  # Stop the build VM if this script is interrupted while it is provisioning.
  trap 'kill '"$pid"' 2>/dev/null || true; rm -rf "'"$work"'"; exit 130' INT TERM

  local waited=0 timeout=${BUILD_TIMEOUT:-1800}
  while kill -0 "$pid" 2>/dev/null; do
    if sentinel_seen "$BUILD_FAIL_SENTINEL" "$console"; then
      kill "$pid" 2>/dev/null || true
      tail -40 "$console" >&2
      die "provisioning failed, see the console output above"
    fi
    if sentinel_seen "$BUILD_OK_SENTINEL" "$console"; then
      break
    fi
    if [[ $waited -ge $timeout ]]; then
      kill "$pid" 2>/dev/null || true
      die "provisioning timed out after ${timeout}s (console: ${console})"
    fi
    sleep 5
    waited=$((waited + 5))
    [[ $((waited % 60)) -eq 0 ]] && log "still provisioning (${waited}s)"
  done

  if ! sentinel_seen "$BUILD_OK_SENTINEL" "$console"; then
    tail -40 "$console" >&2
    die "the build VM exited before provisioning finished, see the console output above"
  fi

  # Wait for the guest to finish powering off, so the image is not copied
  # mid-write.
  local settle=0
  while kill -0 "$pid" 2>/dev/null && [[ $settle -lt 60 ]]; do
    sleep 1
    settle=$((settle + 1))
  done
  kill -9 "$pid" 2>/dev/null || true

  mv "${work}/golden.qcow2" "$golden"
  trap - INT TERM
  rm -rf "$work"
  log "built ${golden}"
}

qemu_machine() {
  case "$(uname -m)" in
    aarch64|arm64) echo virt ;;
    *)             echo q35 ;;
  esac
}

# --------------------------------------------------------------------------
# Running a runner
# --------------------------------------------------------------------------

# github_api_prefix prints the API root for the runner endpoints of a URL. The
# endpoints differ only in whether they hang off a repository or an
# organisation, which is also what decides the PAT scope needed.
github_api_prefix() {
  local url=$1 host scope api
  host=$(sed -E 's#^https?://##; s#/.*##' <<<"$url")
  scope=$(sed -E 's#^https?://[^/]+/##; s#/$##' <<<"$url")
  if [[ "$host" == "github.com" ]]; then api="https://api.github.com"; else api="https://${host}/api/v3"; fi
  if [[ "$scope" == */* ]]; then
    echo "${api}/repos/${scope}"
  else
    echo "${api}/orgs/${scope}"
  fi
}

api_call() {
  local method=$1 url=$2
  curl -fsSL -X "$method" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    -H "Authorization: Bearer ${GITHUB_TOKEN}" \
    "$url"
}

# HTTP_STATUS and HTTP_BODY are set by http_call, which keeps both rather than
# letting curl -f throw the response away. What GitHub says about a refusal is
# the only thing that distinguishes a bad token from a missing permission, and
# guessing between them sends people to the wrong settings page.
HTTP_STATUS=""
HTTP_BODY=""

http_call() {
  local method=$1 url=$2 response
  response=$(curl -sS -X "$method" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    -H "Authorization: Bearer ${GITHUB_TOKEN}" \
    -w $'\n%{http_code}' \
    "$url" 2>&1) || {
      HTTP_STATUS="000"
      HTTP_BODY=$response
      return 1
    }
  HTTP_STATUS=${response##*$'\n'}
  HTTP_BODY=${response%$'\n'*}
  [[ "$HTTP_STATUS" == 2* ]]
}

# api_error turns a refusal into the specific thing to go and check. The three
# statuses have genuinely different causes, and the advice for one is useless
# for the others.
api_error() {
  local what=$1 scope=$2 message
  message=$(json_str message <<<"$HTTP_BODY")
  [[ -n "$message" ]] || message=$(head -c 200 <<<"$HTTP_BODY")

  case "$HTTP_STATUS" in
    000)
      die "${what}: could not reach GitHub
  ${message}"
      ;;
    401)
      die "${what}: GitHub rejected the credential (401 ${message})
  The token itself is wrong, not its permissions. Check the value actually
  reaching the script — a truncated paste or a leftover placeholder looks
  exactly like this:
    sudo grep -c . ${ETC_DIR}/env.${RUNNER_NAME:-<runner>}   # confirm the file has the lines you expect
  A fine-grained token starts with github_pat_ and a classic one with ghp_."
      ;;
    403)
      die "${what}: GitHub refused (403 ${message})
  The credential is valid but not allowed to do this. For ${scope}:
    fine-grained PAT  Administration: Read and write, with ${scope} listed
                      under Repository access
    classic PAT       the 'repo' scope, or 'admin:org' for an organisation
  If the organisation uses SAML, the token also has to be authorised for it."
      ;;
    404)
      die "${what}: GitHub returned 404 for ${scope}
  A 404 here usually means the credential cannot see the repository at all,
  rather than that it is missing:
    - the token's Resource owner is the wrong account or organisation
    - ${scope} is not listed under the token's Repository access
    - the name in GITHUB_URL is misspelled
  ${message}"
      ;;
    *)
      die "${what}: GitHub returned ${HTTP_STATUS}
  ${message}"
      ;;
  esac
}

# json_str pulls a top-level string field out of a JSON response. jq when it is
# available, because the sed fallback cannot cope with nesting — which is fine
# for the two flat responses read here.
json_str() {
  local field=$1
  if have jq; then
    jq -r --arg f "$field" '.[$f] // empty'
  else
    sed -n "s/.*\"${field}\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1
  fi
}

# --- GitHub App authentication -------------------------------------------
#
# A PAT is a long-lived credential belonging to a person: it expires, and it
# stops working when they leave. A GitHub App belongs to the organisation
# instead, and its private key mints an installation token that lasts an hour.
# For a server expected to reboot unattended for months, that is the difference
# between "restarts for ever" and "restarts until the PAT expires".
#
# Nothing here is stored: the JWT is signed per call and the installation token
# is used immediately to mint a registration token.

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# app_jwt signs the short-lived assertion that authenticates as the app itself.
app_jwt() {
  local now iat exp header payload unsigned sig
  now=$(date +%s)
  # Backdated by a minute against clock skew, and well inside the ten minutes
  # GitHub allows.
  iat=$((now - 60))
  exp=$((now + 480))
  header='{"alg":"RS256","typ":"JWT"}'
  payload="{\"iat\":${iat},\"exp\":${exp},\"iss\":\"${GITHUB_APP_ID}\"}"
  unsigned="$(printf '%s' "$header" | b64url).$(printf '%s' "$payload" | b64url)"
  sig=$(printf '%s' "$unsigned" \
    | openssl dgst -sha256 -sign "$GITHUB_APP_PRIVATE_KEY" -binary \
    | b64url) || die "cannot sign with ${GITHUB_APP_PRIVATE_KEY} (is it the app's PEM private key?)"
  printf '%s.%s' "$unsigned" "$sig"
}

app_api_call() {
  local method=$1 url=$2 jwt=$3
  curl -fsSL -X "$method" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    -H "Authorization: Bearer ${jwt}" \
    "$url"
}

# app_installation_token exchanges the app's private key for an installation
# token, which is then used exactly like a PAT.
app_installation_token() {
  local url=$1 jwt id token api
  [[ -r "$GITHUB_APP_PRIVATE_KEY" ]] || die "cannot read the app private key at ${GITHUB_APP_PRIVATE_KEY}"
  have openssl || die "openssl is required for GitHub App authentication"

  jwt=$(app_jwt)
  api=$(github_api_prefix "$url")

  id=$GITHUB_APP_INSTALLATION_ID
  if [[ -z "$id" ]]; then
    # The app knows where it is installed, so there is no need to make the
    # operator hunt for an installation id.
    id=$(app_api_call GET "${api}/installation" "$jwt" 2>/dev/null | json_str id) \
      || die "the app is not installed on $(sed -E 's#^https?://[^/]+/##' <<<"$url"), or GITHUB_APP_ID is wrong"
    [[ -n "$id" ]] || die "cannot find the app's installation on $(sed -E 's#^https?://[^/]+/##' <<<"$url"): install the app there, or set GITHUB_APP_INSTALLATION_ID"
  fi

  local base
  base=$(sed -E 's#(https://[^/]+(/api/v3)?)/.*#\1#' <<<"$api")
  token=$(app_api_call POST "${base}/app/installations/${id}/access_tokens" "$jwt" 2>/dev/null | json_str token) \
    || die "GitHub refused an installation token for installation ${id}"
  [[ -n "$token" ]] || die "GitHub returned no installation token"
  echo "$token"
}

# resolve_github_token fills in GITHUB_TOKEN from whichever long-lived
# credential is configured. Both paths end in the same place: a credential that
# can mint a registration token per boot, so a reboot needs no human.
resolve_github_token() {
  resolve_token_files
  [[ -n "$GITHUB_TOKEN" ]] && return 0
  [[ -n "$GITHUB_APP_ID" && -n "$GITHUB_APP_PRIVATE_KEY" ]] || return 0
  log "authenticating as GitHub App ${GITHUB_APP_ID}"
  GITHUB_TOKEN=$(app_installation_token "$GITHUB_URL")
}

# read_secret reads a credential from a file, or from standard input when the
# path is "-", and trims the trailing newline an editor or "echo" leaves behind.
#
# A file beats both of the alternatives: a token on the command line is visible
# in ps to every user on the machine, and one typed into the shell is kept in
# the history file.
read_secret() {
  local path=$1 value
  if [[ "$path" == "-" ]]; then
    IFS= read -r value || true
  else
    [[ -r "$path" ]] || die "cannot read the credential file ${path}"
    IFS= read -r value < "$path" || true
  fi
  # Trim whitespace, so a stray space or CR in the file is not sent to GitHub.
  value=${value#"${value%%[![:space:]]*}"}
  value=${value%"${value##*[![:space:]]}"}
  [[ -n "$value" ]] || die "the credential file ${path} is empty"
  printf '%s' "$value"
}

# service_credential prints the file systemd loaded for this runner, or nothing
# when there is none. KIND is "pat" or "app.pem".
#
# The unit loads ${CRED_DIR} whole rather than one named file, since a template
# unit cannot say "this file if it exists": naming a missing file fails the unit
# outright. Loading the directory instead lets the choice happen here, where a
# runner takes the credential named after it — pat.NAME — and falls back to the
# host-wide pat, so several runners share one PAT until one of them needs its
# own.
service_credential() {
  local kind=$1 dir=${CREDENTIALS_DIRECTORY:-} name=${RUNNER_NAME:-}
  [[ -n "$dir" ]] || return 0

  # app.pem keeps its suffix, so the runner name goes before it rather than at
  # the end: app.build1.pem, not app.pem.build1.
  local stem=${kind%%.*} suffix=${kind#"${kind%%.*}"}
  if [[ -n "$name" && -r "${dir}/${CRED_ID}_${stem}.${name}${suffix}" ]]; then
    printf '%s' "${dir}/${CRED_ID}_${stem}.${name}${suffix}"
  elif [[ -r "${dir}/${CRED_ID}_${kind}" ]]; then
    printf '%s' "${dir}/${CRED_ID}_${kind}"
  fi
}

resolve_token_files() {
  # Only when nothing was configured by hand: a flag, the environment or an
  # older unit that names the file itself all win over what systemd loaded.
  if [[ -z "$GITHUB_TOKEN" && -z "$GITHUB_TOKEN_FILE" ]]; then
    GITHUB_TOKEN_FILE=$(service_credential pat)
  fi
  if [[ -z "$GITHUB_APP_PRIVATE_KEY" ]]; then
    GITHUB_APP_PRIVATE_KEY=$(service_credential app.pem)
  fi
  if [[ -z "$GITHUB_TOKEN" && -n "$GITHUB_TOKEN_FILE" ]]; then
    GITHUB_TOKEN=$(read_secret "$GITHUB_TOKEN_FILE")
  fi
  if [[ -z "$RUNNER_TOKEN" && -n "$RUNNER_TOKEN_FILE" ]]; then
    RUNNER_TOKEN=$(read_secret "$RUNNER_TOKEN_FILE")
  fi
}

# mint_token exchanges the long-lived credential for a registration token,
# which is what a runner actually registers with. One is minted per boot,
# because a registration token expires an hour after GitHub issues it and a VM
# never keeps its registration.
mint_token() {
  local url=$1 scope
  scope=$(sed -E 's#^https?://[^/]+/##; s#/$##' <<<"$url")

  http_call POST "$(github_api_prefix "$url")/actions/runners/registration-token" \
    || api_error "cannot mint a registration token" "$scope"

  json_str token <<<"$HTTP_BODY"
}

# deregister_runner removes the runner entry a non-ephemeral VM leaves behind.
# Nothing de-registers it otherwise — that needs a credential of its own — so
# it would sit in the runners list as permanently offline.
deregister_runner() {
  [[ -n "$GITHUB_TOKEN" ]] || return 0
  have jq || { warn "jq is not installed, leaving ${RUNNER_NAME} in the runners list"; return 0; }

  local prefix id
  prefix=$(github_api_prefix "$GITHUB_URL")
  id=$(api_call GET "${prefix}/actions/runners?per_page=100" 2>/dev/null \
    | jq -r --arg n "$RUNNER_NAME" '.runners[]? | select(.name == $n) | .id' | head -1) || return 0

  if [[ -n "$id" ]]; then
    if api_call DELETE "${prefix}/actions/runners/${id}" >/dev/null 2>&1; then
      log "deregistered ${RUNNER_NAME}"
    else
      warn "could not deregister ${RUNNER_NAME}, it will show as offline"
    fi
  fi
}

# guest_runner_script is what actually registers the runner, run inside the VM
# by the systemd unit below. Its configuration arrives as environment
# variables, from the unit's EnvironmentFile.
#
# It is short because the machine is thrown away: there is no saved
# registration to restore, no state directory to take ownership of, and no
# second boot to plan for. It registers, runs, and the VM is deleted.
guest_runner_script() {
  cat <<'GUEST'
#!/usr/bin/env bash
set -euo pipefail

# Registers this VM as a self-hosted GitHub Actions runner and runs it. The
# runner refuses to run as root, so the unit starts this as "runner".

: "${GITHUB_URL:?GITHUB_URL is required}"
: "${RUNNER_TOKEN:?RUNNER_TOKEN is required}"
RUNNER_NAME=${RUNNER_NAME:-$(hostname)}
RUNNER_GROUP=${RUNNER_GROUP:-Default}
EPHEMERAL=${EPHEMERAL:-false}
DISABLE_UPDATE=${DISABLE_UPDATE:-false}

cd /home/runner/actions-runner

args=(
  --url "$GITHUB_URL"
  --name "$RUNNER_NAME"
  --runnergroup "$RUNNER_GROUP"
  --work /home/runner/_work
  --unattended
  # Take over an entry of the same name left behind by an earlier VM. Nothing
  # de-registers one when a VM is deleted, as that needs a token of its own.
  --replace
)
[[ -n "${RUNNER_LABELS:-}" ]] && args+=(--labels "$RUNNER_LABELS")
[[ "$EPHEMERAL" == "true" ]] && args+=(--ephemeral)
[[ "$DISABLE_UPDATE" == "true" ]] && args+=(--disableupdate)

echo "registering runner '${RUNNER_NAME}' on ${GITHUB_URL}"
if ! ./config.sh "${args[@]}" --token "$RUNNER_TOKEN"; then
  echo "registration failed: a registration token expires one hour after it is issued" >&2
  exit 1
fi

# exec so that the runner receives the unit's SIGTERM directly and can finish
# the job it is on before stopping.
exec ./run.sh
GUEST
}

# env_quote renders a value for a systemd EnvironmentFile.
#
# Double quotes rather than single: systemd processes C-style escapes inside
# them, so a value containing a quote or a backslash survives. Writing values
# raw meant a label or token containing a quote produced a file that parsed as
# something else entirely.
env_quote() {
  local v=${1:-}
  v=${v//\\/\\\\}
  v=${v//\"/\\\"}
  printf '"%s"' "$v"
}

# run_user_data drops the configuration and the runner script into the guest and
# starts it under systemd. The service writes to the console, so its output
# lands in the log this script streams.
run_user_data() {
  cat <<EOF
#cloud-config
hostname: ${VM_NAME}
preserve_hostname: false

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $(cat "${SSH_KEY}.pub")

write_files:
  # 0600 and root-owned: the registration token is short-lived, but a job has
  # no reason to be able to read it.
  - path: /etc/runner-vm/runner.env
    permissions: '0600'
    owner: 'root:root'
    content: |
      GITHUB_URL=$(env_quote "$GITHUB_URL")
      RUNNER_TOKEN=$(env_quote "$RUNNER_TOKEN")
      RUNNER_NAME=$(env_quote "$RUNNER_NAME")
      RUNNER_LABELS=$(env_quote "$RUNNER_LABELS")
      RUNNER_GROUP=$(env_quote "$RUNNER_GROUP")
      EPHEMERAL=$(env_quote "$EPHEMERAL")
      DISABLE_UPDATE=$(env_quote "$DISABLE_UPDATE")

  - path: /usr/local/bin/runner-vm-runner.sh
    permissions: '0755'
    owner: 'root:root'
    content: |
$(guest_runner_script | sed 's/^/      /')

  - path: /etc/systemd/system/github-runner.service
    permissions: '0644'
    content: |
      [Unit]
      Description=GitHub Actions self-hosted runner
      After=network-online.target docker.service
      Wants=network-online.target

      [Service]
      Type=simple
      User=runner
      WorkingDirectory=/home/runner/actions-runner
      EnvironmentFile=/etc/runner-vm/runner.env
      ExecStart=/usr/local/bin/runner-vm-runner.sh
      # The runner has to finish the job it is on before it stops.
      KillSignal=SIGTERM
      TimeoutStopSec=3h
      Restart=no
      StandardOutput=journal+console
      StandardError=journal+console
      # When the runner stops for any reason the VM has nothing left to do, and
      # powering off is what tells the wrapper to clean up.
      #
      # The "+" prefix runs this with full privileges: ExecStopPost would
      # otherwise inherit User=runner, and polkit refuses a non-interactive
      # poweroff from an unprivileged user with "Interactive authentication
      # required", leaving the VM sitting at a login prompt for ever.
      ExecStopPost=+/usr/bin/systemctl poweroff --no-block

      [Install]
      WantedBy=multi-user.target

runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, start, --no-block, github-runner.service ]
EOF
}

# free_port finds a loopback port for the ssh forward.
#
# Testing whether a port is free is not enough on its own: QEMU only binds it
# seconds later, so two VMs starting together would both pick the same one and
# the second would fail to boot. Each VM therefore writes its choice into its
# own directory, and this skips anything already claimed.
free_port() {
  local port claimed
  claimed=" $(cat "$STATE_DIR"/vms/*/ssh_port 2>/dev/null | tr '\n' ' ') "
  for port in $(seq 2222 4222); do
    [[ "$claimed" == *" ${port} "* ]] && continue
    if ! (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
      echo "$port"
      return 0
    fi
    exec 3>&- 2>/dev/null || true
  done
  die "no free port between 2222 and 4222"
}

# claim_port reserves the port under this VM's directory, then confirms no
# other VM claimed the same one in the meantime. The window is small, and
# losing the race simply means trying the next port.
claim_port() {
  local port other
  for _ in 1 2 3 4 5; do
    port=$(free_port)
    echo "$port" > "${VM_DIR}/ssh_port"
    other=$(grep -l "^${port}$" "$STATE_DIR"/vms/*/ssh_port 2>/dev/null | grep -cv "^${VM_DIR}/ssh_port$" || true)
    if [[ "$other" -eq 0 ]]; then
      echo "$port"
      return 0
    fi
  done
  die "could not claim a free ssh port after 5 attempts"
}

QEMU_PID=""
TAIL_PID=""
VM_DIR=""
CLEANED=false

# cleanup drops the VM. It runs on every exit path — a normal one, Ctrl-C, or a
# signal — because a VM outliving the process that owns it is exactly what this
# script is meant to avoid.
cleanup() {
  [[ "$CLEANED" == "true" ]] && return 0
  CLEANED=true
  trap - EXIT INT TERM

  if [[ -n "$TAIL_PID" ]]; then kill "$TAIL_PID" 2>/dev/null || true; fi

  if [[ -n "$QEMU_PID" ]] && kill -0 "$QEMU_PID" 2>/dev/null; then
    echo
    log "stopping the VM"
    # Ask the guest to power off first: that lets systemd stop the runner
    # service, which gives the runner a chance to finish the job it is on
    # rather than have the machine pulled out from under it.
    if [[ -n "${SSH_PORT:-}" ]] && ssh_guest "sudo systemctl poweroff --no-block" >/dev/null 2>&1; then
      local waited=0
      while kill -0 "$QEMU_PID" 2>/dev/null && [[ $waited -lt ${SHUTDOWN_TIMEOUT:-60} ]]; do
        sleep 1
        waited=$((waited + 1))
      done
    fi
    if kill -0 "$QEMU_PID" 2>/dev/null; then
      kill "$QEMU_PID" 2>/dev/null || true
      sleep 2
      kill -9 "$QEMU_PID" 2>/dev/null || true
    fi
  fi

  if [[ -n "$VM_DIR" && -d "$VM_DIR" ]]; then
    rm -rf "$VM_DIR"
    log "deleted the VM disk"
  fi

  # An ephemeral runner removes itself from GitHub once its job ends; a
  # long-lived one has to be removed here, while we still have the PAT.
  if [[ "$EPHEMERAL" != "true" && -n "${RUNNER_NAME:-}" ]]; then
    deregister_runner || true
  fi
}

ssh_guest() {
  ssh -i "$SSH_KEY" -p "$SSH_PORT" \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR \
    -o ConnectTimeout=5 \
    ubuntu@127.0.0.1 "$@"
}

cmd_run() {
  [[ -n "$GITHUB_URL" ]] || die "no GitHub URL: pass --url https://github.com/OWNER/REPO (or set GITHUB_URL)"

  require_host
  acquire_lock
  ensure_ssh_key
  release_lock

  local golden; golden=$(golden_path)
  if [[ ! -f "$golden" ]]; then
    log "no golden image yet, building it once"
    cmd_build
  fi

  resolve_github_token

  if [[ -z "$RUNNER_TOKEN" ]]; then
    if [[ -n "$GITHUB_TOKEN" ]]; then
      log "minting a registration token"
      RUNNER_TOKEN=$(mint_token "$GITHUB_URL")
      [[ -n "$RUNNER_TOKEN" ]] || die "GitHub returned no registration token"
    else
      die "no way to register a runner. Either:
  --token TOKEN         a registration token from ${GITHUB_URL}/settings/actions/runners/new
                        (good for an hour, so it will not survive a reboot)
  --github-token PAT    mints one per boot; expires when the PAT does
  --github-token-file F reads that PAT from a file, or from stdin given -
  --app-id / --app-key  mints one per boot from a GitHub App, which belongs to
                        the organisation rather than to a person"
    fi
  fi

  VM_NAME=${RUNNER_NAME:-"vm-$(hostname -s)-$$"}
  RUNNER_NAME=$VM_NAME
  # The disk lives in the state directory, not /tmp: a job can write tens of
  # gigabytes into the overlay, and /tmp is memory on many distributions.
  VM_DIR="${STATE_DIR}/vms/${VM_NAME}"

  # Several VMs can share a host, but not a name: they would share a directory,
  # a disk and a runner registration. Refuse rather than delete someone else's
  # machine out from under them.
  if [[ -f "${VM_DIR}/qemu.pid" ]] && pid_alive "$(cat "${VM_DIR}/qemu.pid" 2>/dev/null)"; then
    die "a VM named '${VM_NAME}' is already running on this host
  give this one a different name with --name, or stop that one first"
  fi
  rm -rf "$VM_DIR"
  mkdir -p "$VM_DIR"
  SSH_PORT=$(claim_port)

  # A signal has to end the script, not just run the handler and resume the
  # wait loop below, or Ctrl-C would leave the terminal apparently idle.
  trap cleanup EXIT
  trap 'cleanup; exit 130' INT TERM

  run_user_data > "${VM_DIR}/user-data"
  # A new instance ID on every boot, or cloud-init treats the golden image's
  # own build as the current instance and skips configuring the runner.
  echo -e "instance-id: ${VM_NAME}-$(date +%s)\nlocal-hostname: ${VM_NAME}" > "${VM_DIR}/meta-data"
  make_seed "${VM_DIR}/user-data" "${VM_DIR}/meta-data" "${VM_DIR}/seed.iso"

  # An overlay cannot be smaller than the image it is backed by.
  local golden_bytes golden_gb
  golden_bytes=$(image_virtual_bytes "$golden")
  [[ -n "$golden_bytes" ]] || die "cannot read the size of ${golden}; rebuild it with: $0 build --force"
  golden_gb=$(( (golden_bytes + 1073741823) / 1073741824 ))
  if [[ "$VM_DISK_GB" -lt "$golden_gb" ]]; then
    warn "--disk ${VM_DISK_GB}G is smaller than the golden image (${golden_gb}G), using ${golden_gb}G"
    VM_DISK_GB=$golden_gb
  fi

  # A copy-on-write overlay: the golden image stays untouched, and this disk is
  # a few megabytes until the job fills it.
  qemu-img create -q -f qcow2 -F qcow2 -b "$golden" "${VM_DIR}/disk.qcow2" "${VM_DISK_GB}G"

  # qemu-img creates an overlay smaller than its backing image without so much
  # as a warning, and the guest then hangs in an initramfs shell: truncating the
  # disk cuts off the backup GPT and leaves the primary one describing a device
  # larger than the one present, so the kernel enumerates no partitions at all
  # and the root filesystem never appears. Checked here because that failure is
  # otherwise silent — the VM stays up, the runner never registers, and the
  # only clue is 'LABEL=cloudimg-rootfs does not exist' in the console log.
  local overlay_bytes
  overlay_bytes=$(image_virtual_bytes "${VM_DIR}/disk.qcow2")
  if [[ -z "$overlay_bytes" || "$overlay_bytes" -lt "$golden_bytes" ]]; then
    die "the VM disk (${overlay_bytes:-unknown} bytes) is smaller than the golden image it is backed by (${golden_bytes} bytes), and a guest cannot boot from that
  give it at least --disk ${golden_gb}"
  fi

  # "-cpu host" is what exposes the CPU's virtualisation extensions; naming the
  # flag explicitly makes a host without nested virtualisation fail here rather
  # than surface as a mysteriously broken job later.
  local cpu=host
  [[ "$VM_NESTED" == "true" ]] && cpu="host,+$(nested_flag)"

  local console="${VM_DIR}/console.log"
  : > "$console"

  log "booting ${VM_NAME}: ${VM_CPUS} vCPU, ${VM_MEMORY_MB} MiB, ${VM_DISK_GB} GiB disk"
  # QEMU_EXTRA_ARGS is deliberately unquoted: it is an escape hatch for
  # host-specific options, such as a device passthrough, and has to word-split.
  "$(qemu_binary)" \
    -name "$VM_NAME" \
    -machine "$(qemu_machine)" \
    -cpu "$cpu" \
    -accel kvm \
    -smp "$VM_CPUS" \
    -m "$VM_MEMORY_MB" \
    -drive "file=${VM_DIR}/disk.qcow2,if=virtio,format=qcow2,cache=writeback,discard=unmap" \
    -drive "file=${VM_DIR}/seed.iso,if=virtio,format=raw,readonly=on" \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${SSH_PORT}-:22" \
    -device virtio-net-pci,netdev=net0 \
    -device virtio-rng-pci \
    -display none \
    -serial "file:${console}" \
    -no-reboot \
    -pidfile "${VM_DIR}/qemu.pid" \
    -daemonize \
    ${QEMU_EXTRA_ARGS:-}

  QEMU_PID=$(cat "${VM_DIR}/qemu.pid")

  # What "list" reports. Written after the boot succeeds, so a directory with
  # no meta file is a VM that never got off the ground.
  cat > "${VM_DIR}/meta" <<META
NAME=${VM_NAME}
URL=${GITHUB_URL}
LABELS=${RUNNER_LABELS}
EPHEMERAL=${EPHEMERAL}
CPUS=${VM_CPUS}
MEMORY_MB=${VM_MEMORY_MB}
DISK_GB=${VM_DISK_GB}
NESTED=${VM_NESTED}
STARTED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
META
  log "VM pid ${QEMU_PID}, ssh: ssh -i ${SSH_KEY} -p ${SSH_PORT} ubuntu@127.0.0.1"
  log "registering '${VM_NAME}' on ${GITHUB_URL} — Ctrl-C stops the runner and deletes the VM"
  echo

  # Stream the guest console: cloud-init, then the runner's own output.
  tail -n +1 -f "$console" &
  TAIL_PID=$!

  # Wait for the VM rather than the tail. The guest powers itself off when the
  # runner stops, so this returns on its own if the runner exits.
  while kill -0 "$QEMU_PID" 2>/dev/null; do
    sleep 2
  done

  echo
  log "the VM powered off"
  cleanup
}

# runner_vm_services lists every runner-vm@ instance systemd knows about.
#
# --all matters: an instance that is failed, or restart-looping after a bad
# credential, is exactly the one in the way, and a plain listing shows only the
# active ones. --plain drops the bullet systemd puts in front of failed units,
# which would otherwise end up in the unit name.
runner_vm_services() {
  have systemctl || return 0
  systemctl list-units --all --plain --no-legend 'runner-vm@*.service' 2>/dev/null \
    | awk '{ print $1 }' | grep . || true
}

# stop_services stops those instances. Cleaning up underneath a running service
# is pointless: systemd starts a replacement VM within seconds.
# stop_services stops the instances named, or every one when given nothing.
stop_services() {
  local unit failed=false services

  if [[ $# -gt 0 ]]; then
    # Only the instances that exist: naming a VM that no service manages is
    # normal, not an error.
    services=""
    local name all; all=$(runner_vm_services)
    for name in "$@"; do
      grep -qx "runner-vm@${name}.service" <<<"$all" && services+="runner-vm@${name}.service"$'\n'
    done
  else
    services=$(runner_vm_services)
  fi
  [[ -n "$services" ]] || return 0

  while read -r unit; do
    [[ -n "$unit" ]] || continue
    log "stopping ${unit}"
    if ! systemctl stop "$unit" 2>/dev/null; then
      failed=true
      warn "could not stop ${unit}"
    fi
    # A failed unit keeps its restart counter, which would refuse to start
    # again later with "start request repeated too quickly".
    systemctl reset-failed "$unit" 2>/dev/null || true
  done <<<"$services"

  if [[ "$failed" == "true" ]]; then
    die "could not stop every service; rerun this as root:
  sudo $0 clean${CLEAN_ALL:+ --all} --yes"
  fi
}

# stop_stray_vms shuts down VMs whose owning process is gone — a wrapper killed
# with SIGKILL, or a host that lost power — and deletes what they left behind.
# stop_stray_vms removes the VMs named, or every one when given nothing.
stop_stray_vms() {
  local dir name pid waited
  shopt -s nullglob
  for dir in "$STATE_DIR"/vms/*/; do
    name=$(basename "$dir")
    # With names given, skip everything else.
    if [[ $# -gt 0 ]] && ! printf '%s\n' "$@" | grep -qx "$name"; then
      continue
    fi
    pid=""
    [[ -f "${dir}qemu.pid" ]] && pid=$(cat "${dir}qemu.pid" 2>/dev/null || true)

    if pid_alive "$pid"; then
      log "stopping VM ${name} (pid ${pid})"
      kill "$pid" 2>/dev/null || true
      waited=0
      while pid_alive "$pid" && [[ $waited -lt 15 ]]; do
        sleep 1
        waited=$((waited + 1))
      done
      kill -9 "$pid" 2>/dev/null || true
      sleep 1
      # Signalling someone else's VM needs root, and deleting the disk out from
      # under one still running would corrupt a job rather than clean up after
      # it.
      if pid_alive "$pid"; then
        warn "the VM ${name} (pid ${pid}) is still running and was left alone; rerun this as root"
        continue
      fi
    fi

    rm -rf "$dir"
    log "removed the VM ${name}"
  done
  shopt -u nullglob
}

# cmd_clean throws local state away so that a run can start over — most often
# after a credential turned out to be wrong, when a half-registered runner and
# a stale VM only make the next attempt harder to read.
#
# The images are caches and are kept: nothing about a token lives in them, and
# rebuilding the golden image costs a download and several minutes. --all is
# there for when the image itself is the problem, such as a runner version that
# needs rebaking.
cmd_clean() {
  local all=$CLEAN_ALL
  local targets=("${CLEAN_TARGETS[@]}")

  if [[ ${#targets[@]} -gt 0 && "$all" == "true" ]]; then
    die "--all removes everything, so it cannot be combined with a VM name"
  fi

  echo "This will remove:"
  if [[ ${#targets[@]} -gt 0 ]]; then
    local name found=false
    for name in "${targets[@]}"; do
      if [[ -d "${STATE_DIR}/vms/${name}" ]] || runner_vm_services | grep -qx "runner-vm@${name}.service"; then
        found=true
        echo "  - the VM ${name}, stopping it if it is running"
      else
        die "no VM or service named '${name}'; run '$0 list' to see them"
      fi
    done
    [[ "$found" == "true" ]] || die "nothing to remove"
    echo "  everything else is left alone"
  else
    echo "  - every runner VM in ${STATE_DIR}/vms, stopping any that are running"
    if [[ "$all" == "true" ]]; then
      echo "  - the golden image and the cached cloud image"
      echo "  - the ssh key used to reach the VMs"
      echo "  - ${STATE_DIR}, entirely"
    else
      echo "  the images are kept, so the next run starts in seconds rather than"
      echo "  rebuilding; --all removes those and the ssh key too"
    fi
  fi

  local services
  if [[ ${#targets[@]} -gt 0 ]]; then
    services=""
    local all_units; all_units=$(runner_vm_services)
    local t
    for t in "${targets[@]}"; do
      grep -qx "runner-vm@${t}.service" <<<"$all_units" && services+="runner-vm@${t}.service"$'\n'
    done
    services=$(grep . <<<"$services" || true)
  else
    services=$(runner_vm_services)
  fi
  if [[ -n "$services" ]]; then
    echo "  - and first, these services will be stopped, since systemd would"
    echo "    otherwise start a new VM the moment this one is removed:"
    sed 's/^/      /' <<<"$services"
  fi

  if [[ "$ASSUME_YES" != "true" ]]; then
    if [[ -t 0 ]]; then
      local reply
      read -r -p "Continue? [y/N] " reply
      [[ "$reply" == [yY]* ]] || die "nothing was removed"
    else
      die "refusing to remove anything without a terminal to confirm at; pass --yes"
    fi
  fi

  stop_services "${targets[@]}"
  deregister_known_runners "${targets[@]}"
  stop_stray_vms "${targets[@]}"

  if [[ ${#targets[@]} -gt 0 ]]; then
    log "done; the other VMs and the images were left alone"
    return 0
  fi

  if [[ "$all" == "true" ]]; then
    rm -rf "$STATE_DIR"
    log "removed ${STATE_DIR}"
  else
    log "kept the golden image, so the next run starts in seconds"
  fi

  log "clean. Start again with a new token:"
  log "  $0 --url ${GITHUB_URL:-https://github.com/OWNER/REPO} --github-token-file /path/to/pat"
}

# deregister_known_runners removes the GitHub entries for the VMs still on
# disk, so a fresh start does not leave a list of offline runners behind. It is
# best effort: the credential is often exactly what is being replaced.
deregister_known_runners() {
  [[ -n "$GITHUB_URL" ]] || return 0
  resolve_token_files
  [[ -n "$GITHUB_TOKEN" ]] || return 0
  have jq || return 0

  local dir name
  shopt -s nullglob
  for dir in "$STATE_DIR"/vms/*/; do
    name=$(basename "$dir")
    if [[ $# -gt 0 ]] && ! printf '%s\n' "$@" | grep -qx "$name"; then
      continue
    fi
    RUNNER_NAME=$name deregister_runner 2>/dev/null || true
  done
  shopt -u nullglob
}

# cmd_list shows the VMs on this host and the services that manage them.
#
# A VM whose process is gone but whose directory remains is reported as stopped
# rather than hidden: that is the state a crashed wrapper leaves behind, and
# "clean" is what clears it.
# state_dirs lists the directories VMs might be under.
#
# A runner started by hand keeps its VM under the invoking user's state
# directory, while the service keeps its own under /var/lib/runner-vm. Listing
# only the first is why "list" could report no VMs while a service was plainly
# running.
state_dirs() {
  echo "$STATE_DIR"
  [[ "$SERVICE_STATE" != "$STATE_DIR" && -d "$SERVICE_STATE" ]] && echo "$SERVICE_STATE"
  return 0
}

# vm_dir_for finds a named VM in any of those directories.
vm_dir_for() {
  local name=$1 root
  while read -r root; do
    [[ -d "${root}/vms/${name}" ]] && { echo "${root}/vms/${name}"; return 0; }
  done < <(state_dirs)
  return 1
}

meta_field() {
  local dir=$1 field=$2
  [[ -f "${dir}/meta" ]] || return 0
  sed -n "s/^${field}=//p" "${dir}/meta"
}

# configured_field prints what the service configuration says a named runner
# will use, in the order systemd reads the files: the runner's own overrides
# the host-wide one.
configured_field() {
  local name=$1 field=$2 file value
  for file in "${ETC_DIR}/env.${name}" "${ETC_DIR}/env"; do
    [[ -r "$file" ]] || continue
    # The last assignment wins, as it would in the file systemd reads.
    value=$(sed -n "s/^${field}=//p" "$file" | tail -1)
    [[ -n "$value" ]] && { printf '%s' "$value"; return 0; }
  done
  return 0
}

# short_scope turns a URL into owner/repo, which is what identifies a runner at
# a glance; the scheme and host are the same for every row.
short_scope() {
  sed -E 's#^https?://[^/]+/##; s#/$##' <<<"${1:-}"
}

cmd_list() {
  local names="" roots="" name dir root unit
  shopt -s nullglob

  # Every VM on disk, plus every service instance, since a service that has not
  # booted its VM yet still has a repository and a size worth showing.
  while read -r root; do
    for dir in "${root}"/vms/*/; do
      names+="$(basename "$dir")"$'\n'
    done
  done < <(state_dirs)
  while read -r unit; do
    [[ -n "$unit" ]] || continue
    # runner-vm@NAME.service
    name=${unit#runner-vm@}
    names+="${name%.service}"$'\n'
  done <<<"$(runner_vm_services)"
  shopt -u nullglob

  names=$(sort -u <<<"$names" | sed '/^$/d')
  if [[ -z "$names" ]]; then
    echo "(no VMs and no services)"
    return 0
  fi

  printf '%-16s %-9s %-9s %-5s %-5s %-7s %-6s %-10s %s\n' \
    NAME STATE SERVICE CPU MEM DISK SSH UPTIME REPO

  local pid port state uptime cpus mem disk repo service ephemeral
  while read -r name; do
    [[ -n "$name" ]] || continue
    dir=$(vm_dir_for "$name" || true)

    pid=""; port="-"; uptime="-"; ephemeral=""
    cpus="-"; mem="-"; disk="-"; repo="-"

    if [[ -n "$dir" ]]; then
      roots+="${dir%/vms/*}"$'\n'
      [[ -f "${dir}/qemu.pid" ]] && pid=$(cat "${dir}/qemu.pid" 2>/dev/null || true)
      [[ -f "${dir}/ssh_port" ]] && port=$(cat "${dir}/ssh_port" 2>/dev/null || true)
      cpus=$(meta_field "$dir" CPUS); cpus=${cpus:--}
      mem=$(meta_field "$dir" MEMORY_MB); mem=${mem:+${mem}M}; mem=${mem:--}
      disk=$(meta_field "$dir" DISK_GB); disk=${disk:+${disk}G}; disk=${disk:--}
      repo=$(short_scope "$(meta_field "$dir" URL)"); repo=${repo:--}
      [[ "$(meta_field "$dir" EPHEMERAL)" == "true" ]] && ephemeral="*"
    fi

    # Fall back to the configuration for a service whose VM has not booted, so
    # the row still says what it would run and how big it would be.
    # For a service whose VM has not booted, the configuration is what it will
    # use — which is not the same as this script's defaults once install has
    # recorded a size. Read per runner, or every row would claim the repository
    # of whichever one happens to be the host-wide default.
    local configured
    if [[ "$repo" == "-" ]]; then
      repo=$(short_scope "$(configured_field "$name" GITHUB_URL)")
      repo=${repo:--}
    fi
    if [[ "$cpus" == "-" ]]; then
      configured=$(configured_field "$name" VM_CPUS)
      [[ -n "$configured" ]] && cpus=$configured
    fi
    if [[ "$mem" == "-" ]]; then
      configured=$(configured_field "$name" VM_MEMORY_MB)
      [[ -n "$configured" ]] && mem="${configured}M"
    fi
    if [[ "$disk" == "-" ]]; then
      configured=$(configured_field "$name" VM_DISK_GB)
      [[ -n "$configured" ]] && disk="${configured}G"
    fi
    [[ "$cpus" == "-" ]] && cpus=$VM_CPUS
    [[ "$mem"  == "-" ]] && mem="${VM_MEMORY_MB}M"
    [[ "$disk" == "-" ]] && disk="${VM_DISK_GB}G"

    if pid_alive "$pid"; then
      state=running
      uptime=$(ps -o etime= -p "$pid" 2>/dev/null | tr -d ' ' || true)
      uptime=${uptime:--}
    elif [[ -n "$dir" ]]; then
      state=stopped
      pid="-"
    else
      state="-"
      pid="-"
    fi

    service="-"
    if have systemctl; then
      service=$(systemctl is-active "runner-vm@${name}.service" 2>/dev/null || true)
      [[ -n "$service" ]] || service="-"
    fi

    printf '%-16s %-9s %-9s %-5s %-5s %-7s %-6s %-10s %s\n' \
      "$name" "$state" "$service" "$cpus" "$mem" "$disk" "$port" "$uptime" "${repo}${ephemeral}"
  done <<<"$names"

  echo
  echo "* ephemeral: the VM stops after one job and the service starts a clean one"

  # The ssh key is per state directory, and the VMs a service owns live under
  # its own: printing this script's key against those would hand out a key that
  # cannot log into any of them.
  local dirs count
  dirs=$(sort -u <<<"$roots" | sed '/^$/d')
  [[ -n "$dirs" ]] || dirs=$STATE_DIR
  count=$(grep -c . <<<"$dirs")
  if [[ "$count" -eq 1 ]]; then
    echo "ssh into one with:  ssh -i ${dirs}/ssh/id_ed25519 -p <SSH> ubuntu@127.0.0.1"
    echo "consoles are under: ${dirs}/vms/<NAME>/console.log"
  else
    echo "ssh into one with:  ssh -i <state dir>/ssh/id_ed25519 -p <SSH> ubuntu@127.0.0.1"
    echo "consoles are under: <state dir>/vms/<NAME>/console.log"
    echo "state dirs:         $(tr '\n' ' ' <<<"$dirs")"
  fi
  if [[ "$SERVICE_STATE" != "$STATE_DIR" && -d "$SERVICE_STATE" && ! -r "$SERVICE_STATE/vms" ]]; then
    echo
    warn "cannot read ${SERVICE_STATE}; rerun with sudo to see the VMs the service owns"
  fi
}

# systemd_unit prints the template unit. It lives here rather than in a file of
# its own so that a single copied script can install itself, and so the two
# cannot drift apart.
systemd_unit() {
  cat <<UNIT
# Installed by runner-vm.sh install. Regenerate with: runner-vm.sh print-unit
#
# A template unit, so the instance name is the runner name:
#   systemctl enable --now runner-vm@build1
#
# Nothing in here names a repository or a credential, so one unit serves every
# runner on the host: %i picks the configuration and the credential out of
# ${ETC_DIR}, and two runners on two repositories differ only by their
# instance name.

[Unit]
Description=GitHub Actions runner in a QEMU VM (%i)
Documentation=https://github.com/clems4ever/github-runner
# The runner polls GitHub over the network, so wait for that.
#
# There is deliberately no dependency on dev-kvm.device: systemd only creates
# device units for devices udev tags with "systemd", which never includes misc,
# where kvm lives. Requiring it fails on every host. The script checks /dev/kvm
# itself, and Restart= covers a module that loads late.
After=network-online.target
Wants=network-online.target

# A runner that cannot register would otherwise restart every 15 seconds for
# ever. Ten starts in five minutes is clear of normal ephemeral churn.
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=simple

# An unprivileged user in the kvm group: the VM needs /dev/kvm, and nothing
# else on the host.
User=${SERVICE_USER}
SupplementaryGroups=kvm

StateDirectory=runner-vm
Environment=RUNNER_VM_HOME=${SERVICE_STATE}
WorkingDirectory=${SERVICE_STATE}

# systemd reads these as root before dropping to User=, so they stay 0600
# root-owned and are never readable by the service user.
#
# The host-wide file first, then this runner's own: systemd lets a later
# EnvironmentFile override an earlier one, which is what makes per-repository
# settings possible without a unit per repository. Both are optional so that a
# host configured entirely one way or the other still starts.
EnvironmentFile=-${ETC_DIR}/env
EnvironmentFile=-${ETC_DIR}/env.%i

# The whole directory, not a named file: a template unit cannot make one
# LoadCredential= conditional, and naming a file that is not there fails the
# unit. Every file in it becomes a credential named ${CRED_ID}_<filename>, and
# the script picks this runner's out of them.
LoadCredential=${CRED_ID}:${CRED_DIR}

ExecStart=${INSTALL_BIN} run --name %i

# Every start registers a new runner, because a VM keeps nothing, and that
# needs a credential that can mint registration tokens. Fail loudly rather than
# boot a VM that cannot register.
# %d is the credentials directory, so this asks whether systemd loaded anything
# at all — a literal path rather than \$CREDENTIALS_DIRECTORY, since systemd
# expands variables in command lines itself and the result would depend on
# whether it had that one set at expansion time.
ExecStartPre=/bin/sh -c '[ -n "\$GITHUB_TOKEN" ] || [ -n "\$GITHUB_TOKEN_FILE" ] || [ -n "\$GITHUB_APP_ID" ] || [ -n "\$(ls -A %d 2>/dev/null)" ] || { echo "no credential for %i: put a PAT in ${CRED_DIR}/pat.%i, or in ${CRED_DIR}/pat to share one between runners, or set GITHUB_APP_ID with an app key beside it. A pasted registration token will not do: it expires an hour after it is issued and will not survive a reboot" >&2; exit 1; }'

Restart=always
RestartSec=15

# SIGTERM must reach the script alone. QEMU daemonises itself but stays in the
# service cgroup, so the default would signal it directly — the equivalent of
# pulling the VM's power cord — instead of letting the script shut the guest
# down.
KillMode=mixed
KillSignal=SIGTERM

# Stopping is not instant by design: the runner finishes the job it is on
# first. These two have to stay in step.
Environment=SHUTDOWN_TIMEOUT=3600
TimeoutStopSec=3660

[Install]
WantedBy=multi-user.target
UNIT
}

# print_getting_started is what a fresh install leaves on the screen: three
# commands and where to get the token. Everything else — services, PATs,
# ephemeral runners — is in --help and the README, and printing it here only
# buries the one thing someone wants to do next.
print_getting_started() {
  local cmd=$INSTALL_BIN
  # /usr/local/bin is on PATH, so the bare name is what they will actually type.
  [[ "$cmd" == /usr/local/bin/* ]] && cmd=$(basename "$cmd")

  cat <<GUIDE

Installed. To boot a runner:

  sudo ${cmd} doctor     # check this host can run VMs
  sudo ${cmd} build      # build the VM image, once per host
  sudo ${cmd} run --url https://github.com/OWNER/REPO --token AAAA...

The token is on Settings > Actions > Runners > New self-hosted runner.

For runners that start with the host, and credentials that outlive an hour:
  ${cmd} --help

GUIDE
}

# install_source is the file to install.
#
# Piped into bash — "curl ... | sudo bash -s -- install" — there is no file to
# copy, so the canonical one is fetched instead. That is also why SCRIPT_URL
# has to point at the same branch being piped, or the installed copy would be a
# different version than the one that ran.
install_source() {
  local self=${BASH_SOURCE[0]:-} src=""
  [[ -n "$self" ]] && src=$(readlink -f "$self" 2>/dev/null || true)
  if [[ -n "$src" && -f "$src" ]]; then
    echo "$src"
    return 0
  fi

  local tmp; tmp=$(mktemp)
  log "fetching ${SCRIPT_URL}" >&2
  curl -fsSL -o "$tmp" "$SCRIPT_URL" || die "cannot fetch ${SCRIPT_URL}"
  bash -n "$tmp" || die "${SCRIPT_URL} is not a valid script"
  echo "$tmp"
}

# service_env_file renders the configuration the unit reads for one runner.
#
# The unit runs "run --name %i" and takes everything else from here, so a
# setting that is not written is one that install accepted and then silently
# ignored — which is what happened to --cpus. The sizing is written even at its
# default value, so the file says what a VM will actually be and is the one
# place to change it afterwards.
#
# An empty value is left out rather than written blank: this file is read after
# the host-wide one, so a blank line here would not fall back to the shared
# setting, it would override it with nothing.
service_env_file() {
  local name=${1:-}
  [[ -n "$name" ]] && printf '# Configuration for runner-vm@%s. Overrides %s/env.\n' "$name" "$ETC_DIR"
  [[ -n "$GITHUB_URL" ]] && printf 'GITHUB_URL=%s\n' "$GITHUB_URL"
  printf 'VM_CPUS=%s\n' "$VM_CPUS"
  printf 'VM_MEMORY_MB=%s\n' "$VM_MEMORY_MB"
  printf 'VM_DISK_GB=%s\n' "$VM_DISK_GB"
  printf 'VM_NESTED=%s\n' "$VM_NESTED"
  printf 'RUNNER_GROUP=%s\n' "$RUNNER_GROUP"
  printf 'EPHEMERAL=%s\n' "$EPHEMERAL"
  [[ -n "$RUNNER_LABELS" ]] && printf 'RUNNER_LABELS=%s\n' "$RUNNER_LABELS"
  # The app id is not secret, but the unit needs it alongside the key.
  [[ -n "$GITHUB_APP_ID" ]] && printf 'GITHUB_APP_ID=%s\n' "$GITHUB_APP_ID"
  true
}

# shared_env_file renders the host-wide file, which install creates once and
# then never touches again — a runner that has no file of its own falls back to
# it, so rewriting it would reach runners the current install was not about.
shared_env_file() {
  cat <<ENV
# Host-wide defaults for every runner on this machine.
#
# runner-vm@NAME reads this file first and then ${ETC_DIR}/env.NAME, so
# anything set here is a default that a runner can override. Per-repository
# settings — GITHUB_URL above all — belong in the per-runner file, or every
# runner on the host ends up pointed at the same repository.
ENV
}

# stored_credential prints the PAT file a runner will use, preferring its own
# over the host-wide one, the same order the service resolves them in. Empty
# when the host has neither.
#
# The flat path comes last and is the older layout, which is still what an
# upgrading host has at the point install looks for a credential: the move into
# the credentials directory deliberately happens later, once install is
# committed to writing, so a run that fails before then leaves the credential
# where the unit already on the host expects it.
stored_credential() {
  local name=$1
  if [[ -n "$name" && -r "${CRED_DIR}/pat.${name}" ]]; then
    printf '%s' "${CRED_DIR}/pat.${name}"
  elif [[ -r "${CRED_DIR}/pat" ]]; then
    printf '%s' "${CRED_DIR}/pat"
  elif [[ -r "${ETC_DIR}/pat" ]]; then
    printf '%s' "${ETC_DIR}/pat"
  fi
}

# migrate_flat_credentials moves an older layout — one credential sitting
# directly in ETC_DIR — into the credentials directory, so upgrading a host by
# rerunning install does not leave its runners without a token.
migrate_flat_credentials() {
  local name
  for name in pat app.pem; do
    if [[ -f "${ETC_DIR}/${name}" && ! -e "${CRED_DIR}/${name}" ]]; then
      mv "${ETC_DIR}/${name}" "${CRED_DIR}/${name}"
      log "moved ${ETC_DIR}/${name} to ${CRED_DIR}/${name}"
    fi
  done
}

# store_credentials files whatever credential this install brought with it.
#
# The first one on the host becomes the host-wide default, so a second
# repository added later needs no token of its own; a different one is stored
# under the runner's name and wins for that runner alone. A token identical to
# the shared one is not copied — two files holding the same secret are two
# files to rotate.
store_credentials() {
  local instance=$1 token=$2 dest

  if [[ -n "$token" ]]; then
    if [[ ! -e "${CRED_DIR}/pat" ]]; then
      dest="${CRED_DIR}/pat"
    elif [[ "$(read_secret "${CRED_DIR}/pat")" == "$token" ]]; then
      dest=""
      rm -f "${CRED_DIR}/pat.${instance}"
    else
      dest="${CRED_DIR}/pat.${instance}"
    fi
    if [[ -n "$dest" ]]; then
      # Created empty at 0600 before anything is written to it: a plain
      # redirect would make it world-readable for the instant between creation
      # and chmod.
      install -m 0600 /dev/null "$dest"
      printf '%s' "$token" > "$dest"
      log "stored the credential in ${dest} (0600, root)"
    else
      log "the credential is already in ${CRED_DIR}/pat, shared by every runner here"
    fi
  fi

  # An app key that is already in place — a rerun naming the same file — is
  # left where it is rather than copied onto itself.
  if [[ -n "$GITHUB_APP_PRIVATE_KEY" && "$GITHUB_APP_PRIVATE_KEY" != "${CRED_DIR}"/* ]]; then
    if [[ ! -e "${CRED_DIR}/app.pem" ]]; then
      dest="${CRED_DIR}/app.pem"
    elif cmp -s "$GITHUB_APP_PRIVATE_KEY" "${CRED_DIR}/app.pem"; then
      dest=""
      rm -f "${CRED_DIR}/app.${instance}.pem"
    else
      dest="${CRED_DIR}/app.${instance}.pem"
    fi
    if [[ -n "$dest" ]]; then
      install -m 0600 "$GITHUB_APP_PRIVATE_KEY" "$dest"
      log "stored the app key in ${dest} (0600, root)"
    fi
  fi
}

# cmd_install puts the script, the unit and the service user in place. It is
# the runbook, so that getting a host ready is one command rather than ten
# that are easy to get subtly wrong.
cmd_install() {
  [[ $(id -u) -eq 0 ]] || die "install has to run as root: sudo $0 install ..."

  local source; source=$(install_source)
  if [[ "$source" -ef "$INSTALL_BIN" ]]; then
    # Already the installed copy: "runner-vm.sh install --service" run from
    # /usr/local/bin is a normal thing to do, and copying a file onto itself is
    # an error rather than a no-op.
    log "already installed at ${INSTALL_BIN}"
  else
    # Written beside the destination and renamed into place, rather than
    # copied over it: the destination may be the script currently executing,
    # and bash reads a script as it goes. A rename leaves the running inode
    # alone.
    local staged="${INSTALL_BIN}.new.$$"
    cp "$source" "$staged"
    chmod 0755 "$staged"
    mv -f "$staged" "$INSTALL_BIN"
    log "installed ${INSTALL_BIN}"
  fi

  # Plain "install" puts the management script on the host and stops there,
  # touching nothing else. Setting a machine up involves decisions — which
  # repository, which credential, how many runners — so printing them beats
  # guessing, and leaves a host that is easy to undo.
  if [[ "$INSTALL_SERVICE" != "true" ]]; then
    print_getting_started
    return 0
  fi

  require_host

  local instance=${RUNNER_NAME:-runner-1}

  # Check the credential before anything slow. Discovering a bad token after
  # six minutes of building an image is the wrong order to find out.
  resolve_token_files
  # Whether this run brought a credential of its own, which decides below
  # whether one is written. Adding a second repository to a host with
  # "install --service --url ..." and no token is meant to reuse the PAT that
  # is already there, not to fail.
  local supplied_token=$GITHUB_TOKEN
  if [[ -z "$GITHUB_TOKEN" && -z "$GITHUB_APP_ID" && -z "$RUNNER_TOKEN" ]]; then
    local existing; existing=$(stored_credential "$instance")
    if [[ -n "$existing" ]]; then
      GITHUB_TOKEN=$(read_secret "$existing")
      log "no credential given; using the one already in ${existing}"
    fi
  fi
  if [[ -n "$GITHUB_URL" && -n "$GITHUB_TOKEN" ]]; then
    log "checking the credential against ${GITHUB_URL}"
    mint_token "$GITHUB_URL" >/dev/null
    log "the credential can register runners"
  elif [[ -z "$GITHUB_TOKEN" && -z "$GITHUB_APP_ID" && -z "$RUNNER_TOKEN" ]]; then
    # Refuse rather than warn. Without a credential the service cannot start,
    # so carrying on means several minutes building an image and then a unit
    # that fails immediately.
    die "no credential given, and the service cannot start without one.

  Pass one and it is stored in ${CRED_DIR}, root-owned and 0600, and
  wired into the unit through systemd's credential mechanism. Any of:

    sudo ${INSTALL_BIN} install --service --url ${GITHUB_URL:-<repo>} --github-token github_pat_...
    sudo GITHUB_TOKEN=github_pat_... ${INSTALL_BIN} install --service --url ${GITHUB_URL:-<repo>}
    sudo ${INSTALL_BIN} install --service --url ${GITHUB_URL:-<repo>} --github-token-file /path/to/pat
    printf %s \"\$PAT\" | sudo ${INSTALL_BIN} install --service --url ${GITHUB_URL:-<repo>} --github-token-file -

  The last two keep it off the command line, where ps shows it to every user on
  the machine. Note that sudo drops the environment, so GITHUB_TOKEN has to be
  set on the sudo line itself as above, or passed through with sudo -E.

  A registration token (--token) will not do here: it expires an hour after it
  is issued, and a VM registers afresh on every boot. Add --no-start to set the
  host up without starting anything."
  fi

  if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --create-home --home-dir "$SERVICE_STATE" "$SERVICE_USER"
    log "created the ${SERVICE_USER} user"
  fi
  # /dev/kvm is rw for its group only, and the service user is not in it by
  # default. The group is read from the device rather than assumed, since its
  # name and gid both vary by distribution.
  local kvm_group kvm_gid
  kvm_group=$(stat -c %G /dev/kvm 2>/dev/null || true)
  if [[ -z "$kvm_group" || "$kvm_group" == "UNKNOWN" ]]; then
    # The device's gid has no entry in /etc/group, which happens in containers
    # and on hosts where the module was loaded before the package that names
    # the group. usermod cannot add anyone to a group that has no name.
    kvm_gid=$(stat -c %g /dev/kvm 2>/dev/null || true)
    if [[ -n "$kvm_gid" ]]; then
      getent group "$kvm_gid" >/dev/null || groupadd -g "$kvm_gid" kvm || true
      kvm_group=$(getent group "$kvm_gid" | cut -d: -f1)
    fi
  fi
  if [[ -n "$kvm_group" && "$kvm_group" != "UNKNOWN" ]]; then
    usermod -aG "$kvm_group" "$SERVICE_USER" \
      || warn "could not add ${SERVICE_USER} to the ${kvm_group} group; it will not be able to use /dev/kvm"
  else
    warn "cannot tell which group owns /dev/kvm, so ${SERVICE_USER} was not added to it"
  fi
  install -d -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$SERVICE_STATE"

  install -d -m 0755 "$ETC_DIR"
  install -d -m 0700 "$CRED_DIR"
  migrate_flat_credentials

  # This runner's own file, never the shared one: installing a second
  # repository must not repoint the runners already on the host, which is
  # exactly what writing GITHUB_URL to the shared file used to do.
  local instance_env="${ETC_DIR}/env.${instance}"
  install -m 0600 /dev/null "$instance_env"
  service_env_file "$instance" > "$instance_env"
  log "wrote ${instance_env} (${VM_CPUS} vCPU, ${VM_MEMORY_MB} MiB, ${VM_DISK_GB} GiB)"
  [[ -n "$GITHUB_URL" ]] || warn "no --url given; put GITHUB_URL in ${instance_env} before starting"

  # The shared file is created once and then left alone: it is what runners
  # without a file of their own fall back to, so rewriting it here would reach
  # runners this install was not about.
  if [[ ! -e "${ETC_DIR}/env" ]]; then
    install -m 0600 /dev/null "${ETC_DIR}/env"
    shared_env_file > "${ETC_DIR}/env"
  else
    local shared_url; shared_url=$(sed -n 's/^GITHUB_URL=//p' "${ETC_DIR}/env" | tail -1)
    if [[ -n "$shared_url" && "$shared_url" != "$GITHUB_URL" ]]; then
      warn "${ETC_DIR}/env still sets GITHUB_URL=${shared_url}; it is the host-wide
  default now, used only by runners with no ${ETC_DIR}/env.NAME of their own"
    fi
  fi

  store_credentials "$instance" "$supplied_token"

  systemd_unit > /etc/systemd/system/runner-vm@.service
  chmod 0644 /etc/systemd/system/runner-vm@.service
  log "wrote /etc/systemd/system/runner-vm@.service"
  systemctl daemon-reload

  if [[ "$INSTALL_BUILD" == "true" ]]; then
    log "building the golden image (a few minutes, once per host)"
    sudo -u "$SERVICE_USER" \
      env RUNNER_VM_HOME="$SERVICE_STATE" SCRIPT_URL="$SCRIPT_URL" \
      "$INSTALL_BIN" build \
      || die "the image build failed; fix it and rerun, or use --no-build"
  fi

  if [[ "$INSTALL_START" == "true" ]]; then
    # A rerun for a runner that is already up has just rewritten its
    # configuration, and "enable --now" would report success while the VM kept
    # running with the old one. Restarting it here is not the answer either:
    # stopping waits for the job in flight, which can be an hour.
    local was_active=false
    systemctl is-active --quiet "runner-vm@${instance}" && was_active=true

    systemctl enable --now "runner-vm@${instance}"
    log "started runner-vm@${instance}"
    if [[ "$was_active" == "true" ]]; then
      log "it was already running, so it is still on the previous configuration"
      log "pick this one up when the runner is idle with:"
      log "  sudo systemctl restart runner-vm@${instance}"
    fi
    log ""
    log "the runner appears at ${GITHUB_URL:-<your repo>}/settings/actions/runners in about a minute"
    log "watch it come up with:"
    log "  sudo journalctl -u runner-vm@${instance} -f"
  else
    log "installed. Start a runner with:"
    log "  sudo systemctl enable --now runner-vm@${instance}"
  fi
  log ""
  log "another runner on this host, for another repository:"
  log "  sudo ${INSTALL_BIN} install --service --name <name> --url https://github.com/OWNER/OTHER"
}

# cmd_uninstall removes the whole installation: the services, the unit, the
# configuration, the state and the script itself. "clean" deliberately leaves
# all of that alone — it is for starting a run over — so this is the one that
# genuinely puts the host back as it was.
cmd_uninstall() {
  local unit=/etc/systemd/system/runner-vm@.service
  local script=$INSTALL_BIN
  # The service keeps its state here rather than under the invoking user's
  # home, so it has to be named explicitly or uninstalling as root would miss
  # it entirely.
  local service_state=$SERVICE_STATE

  echo "This will remove:"
  local services; services=$(runner_vm_services)
  if [[ -n "$services" ]]; then
    echo "  - these services, stopped and disabled:"
    sed 's/^/      /' <<<"$services"
  fi
  [[ -f "$unit" ]] && echo "  - ${unit}"
  [[ -d "$ETC_DIR" ]] && echo "  - ${ETC_DIR}, including every runner's configuration and credentials"
  [[ -d "$STATE_DIR" ]] && echo "  - ${STATE_DIR} (VMs, images, ssh key)"
  [[ -d "$service_state" && "$service_state" != "$STATE_DIR" ]] && echo "  - ${service_state}"
  [[ -f "$script" ]] && echo "  - ${script}"
  echo
  echo "The ${SERVICE_USER} user is left alone; remove it with: sudo userdel -r ${SERVICE_USER}"

  if [[ "$ASSUME_YES" != "true" ]]; then
    if [[ -t 0 ]]; then
      local reply
      read -r -p "Continue? [y/N] " reply
      [[ "$reply" == [yY]* ]] || die "nothing was removed"
    else
      die "refusing to remove anything without a terminal to confirm at; pass --yes"
    fi
  fi

  local u
  if [[ -n "$services" ]]; then
    while read -r u; do
      [[ -n "$u" ]] || continue
      systemctl disable --now "$u" 2>/dev/null || warn "could not disable ${u}"
      systemctl reset-failed "$u" 2>/dev/null || true
    done <<<"$services"
  fi

  stop_stray_vms

  rm -f "$unit"
  rm -rf "$ETC_DIR" "$STATE_DIR"
  [[ "$service_state" != "$STATE_DIR" ]] && rm -rf "$service_state"
  have systemctl && systemctl daemon-reload 2>/dev/null || true

  # Last, because it is the file being executed.
  rm -f "$script"
  log "uninstalled"
}

# --------------------------------------------------------------------------
# Entry point
# --------------------------------------------------------------------------


main() {
  local command=run
  case "${1:-}" in
    run|build|doctor|clean|install|uninstall|list|ls|print-unit) command=$1; shift ;;
    -h|--help|help) print_help; exit 0 ;;
    --version) echo "runner-vm ${VERSION}"; exit 0 ;;
  esac

  # An .env in the working directory is read if present; explicit flags win.
  local env_file=$ENV_FILE
  local -a args=("$@")
  for ((i = 0; i < ${#args[@]}; i++)); do
    case "${args[$i]}" in
      --env-file) env_file=${args[$((i + 1))]} ;;
      --env-file=*) env_file=${args[$i]#*=} ;;
    esac
  done
  if [[ -f "$env_file" ]]; then
    log "reading ${env_file}"
    set -a
    # shellcheck disable=SC1090
    source "$env_file"
    set +a
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --url)            GITHUB_URL=$2; shift 2 ;;
      --url=*)          GITHUB_URL=${1#*=}; shift ;;
      --token)          RUNNER_TOKEN=$2; shift 2 ;;
      --token=*)        RUNNER_TOKEN=${1#*=}; shift ;;
      --github-token)   GITHUB_TOKEN=$2; shift 2 ;;
      --github-token=*) GITHUB_TOKEN=${1#*=}; shift ;;
      --github-token-file)   GITHUB_TOKEN_FILE=$2; shift 2 ;;
      --github-token-file=*) GITHUB_TOKEN_FILE=${1#*=}; shift ;;
      --token-file)     RUNNER_TOKEN_FILE=$2; shift 2 ;;
      --token-file=*)   RUNNER_TOKEN_FILE=${1#*=}; shift ;;
      --app-id)         GITHUB_APP_ID=$2; shift 2 ;;
      --app-id=*)       GITHUB_APP_ID=${1#*=}; shift ;;
      --app-key)        GITHUB_APP_PRIVATE_KEY=$2; shift 2 ;;
      --app-key=*)      GITHUB_APP_PRIVATE_KEY=${1#*=}; shift ;;
      --app-installation-id)   GITHUB_APP_INSTALLATION_ID=$2; shift 2 ;;
      --app-installation-id=*) GITHUB_APP_INSTALLATION_ID=${1#*=}; shift ;;
      --name)           RUNNER_NAME=$2; shift 2 ;;
      --name=*)         RUNNER_NAME=${1#*=}; shift ;;
      --labels)         RUNNER_LABELS=$2; shift 2 ;;
      --labels=*)       RUNNER_LABELS=${1#*=}; shift ;;
      --group)          RUNNER_GROUP=$2; shift 2 ;;
      --group=*)        RUNNER_GROUP=${1#*=}; shift ;;
      --cpus)           VM_CPUS=$2; shift 2 ;;
      --cpus=*)         VM_CPUS=${1#*=}; shift ;;
      --memory)         VM_MEMORY_MB=$2; shift 2 ;;
      --memory=*)       VM_MEMORY_MB=${1#*=}; shift ;;
      --disk)           VM_DISK_GB=$2; shift 2 ;;
      --disk=*)         VM_DISK_GB=${1#*=}; shift ;;
      --ephemeral)      EPHEMERAL=true; shift ;;
      --nested)         VM_NESTED=true; shift ;;
      --no-nested)      VM_NESTED=false; shift ;;
      --force)          FORCE=true; shift ;;
      --all)            CLEAN_ALL=true; shift ;;
      --service)        INSTALL_SERVICE=true; shift ;;
      --no-build)       INSTALL_BUILD=false; shift ;;
      --no-start)       INSTALL_START=false; shift ;;
      -y|--yes)         ASSUME_YES=true; shift ;;
      # Already handled before the loop, when the file was sourced.
      --env-file)       shift; [[ $# -gt 0 ]] && shift ;;
      --env-file=*)     shift ;;
      -h|--help)        print_help; exit 0 ;;
      -*)               die "unknown flag: $1 (try --help)" ;;
      # A bare word is a VM name, which only clean takes.
      *)                CLEAN_TARGETS+=("$1"); shift ;;
    esac
  done

  mkdir -p "$STATE_DIR" "$IMAGES_DIR"

  case "$command" in
    run)    cmd_run ;;
    build)  cmd_build ;;
    doctor) cmd_doctor ;;
    clean)  cmd_clean ;;
    install)   cmd_install ;;
    uninstall) cmd_uninstall ;;
    list|ls)   cmd_list ;;
    print-unit) systemd_unit ;;
  esac
}

# Run unless sourced, so the tests can call the functions above directly.
#
# The first condition matters as much as the second: piped into bash there is
# no BASH_SOURCE at all, and that is the one-liner install, which must still
# run main.
if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
