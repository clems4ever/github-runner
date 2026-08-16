#!/usr/bin/env bash
set -euo pipefail

# Runs a self-hosted GitHub Actions runner inside a throwaway QEMU virtual
# machine.
#
# This is a wrapper around entrypoint.sh: the VM does what the Dockerfile does
# — download the runner package, verify it, extract it — and then runs
# entrypoint.sh exactly as the container does, so the download / configure /
# run procedure lives in one place.
#
# The VM is tied to this process. Stop it with Ctrl-C, or send it a signal, and
# the machine is powered off and its disk deleted. Nothing a job did survives,
# which is the point: a long-lived runner accumulates state from every job it
# has run, and a VM is a much harder boundary than a container.
#
# Compared with running the container, a job here gets:
#   - a Docker daemon of its own, rather than a socket to the host's, which is
#     equivalent to root on the host
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
#   clean     Stop the services and every VM, and delete local state, to start
#             over after a bad credential. Keeps the images unless --all is given
#   install   Install the script, the systemd unit and the service user, build
#             the image and start a runner. Run as root
#   uninstall Remove everything: services, unit, configuration, state and this
#             script
#   print-unit  Print the systemd unit install would write
#
# Flags for run (each has an environment variable equivalent, so an existing
# .env from the container setup works unchanged):
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
#   --name NAME           the runner to enable (default runner-1)
#   --no-build            do not build the golden image
#   --no-start            do not enable and start the service
#
# Flags for clean:
#   --all                 also delete the images and the ssh key, i.e. all of
#                         the state directory
#   -y, --yes             do not ask for confirmation
#
# Examples:
#   ./runner-vm.sh --url https://github.com/runyard-ai --token AAA...
#   GITHUB_TOKEN=ghp_... ./runner-vm.sh --url https://github.com/runyard-ai
#   ./runner-vm.sh --url https://github.com/OWNER/REPO --github-token-file /etc/runner-vm/pat
#   ./runner-vm.sh --url https://github.com/runyard-ai --app-id 123456 \
#     --app-key /etc/runner-vm/app.pem
#   ./runner-vm.sh build --force
#   ./runner-vm.sh clean --all --yes
#   sudo ./runner-vm.sh install --url https://github.com/OWNER/REPO \
#     --github-token-file /root/pat
#   ./runner-vm.sh list
#   sudo ./runner-vm.sh uninstall

VERSION=0.1.0

# --------------------------------------------------------------------------
# Configuration
# --------------------------------------------------------------------------

STATE_DIR=${RUNNER_VM_HOME:-${XDG_DATA_HOME:-$HOME/.local/share}/runner-vm}
IMAGES_DIR=$STATE_DIR/images
SSH_KEY=$STATE_DIR/ssh/id_ed25519

# Version of the runner package to bake in, kept in step with the Dockerfile so
# the VM and the container run the same runner.
RUNNER_VERSION=${RUNNER_VERSION:-2.336.0}
RUNNER_SHA256_X64=${RUNNER_SHA256_X64:-04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d}
RUNNER_SHA256_ARM64=${RUNNER_SHA256_ARM64:-58b758e420b87093fbd4bfddd368074960053e2f1388f01848c82624b90f27d1}

# Ubuntu cloud image series to base the golden image on.
UBUNTU_RELEASE=${UBUNTU_RELEASE:-noble}

# Where entrypoint.sh comes from: the copy next to this script when it is run
# from a clone, otherwise the published one, so the script also works on its
# own.
ENTRYPOINT_URL=${ENTRYPOINT_URL:-https://raw.githubusercontent.com/clems4ever/github-runner/main/entrypoint.sh}

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
RUNNER_PACKAGES_DEFAULT="\
ca-certificates curl wget gnupg software-properties-common apt-transport-https \
git git-lfs openssh-client rsync \
build-essential pkg-config cmake autoconf automake libtool make patch \
python3 python3-pip python3-venv python3-dev python3-setuptools \
nodejs npm \
jq xz-utils bzip2 zstd unzip zip tar \
libssl-dev zlib1g-dev libffi-dev libbz2-dev libreadline-dev libsqlite3-dev \
libcurl4-openssl-dev libxml2-dev sqlite3 \
dnsutils iputils-ping netcat-openbsd net-tools \
file tree time parallel moreutils shellcheck \
locales tzdata sudo lsb-release uuid-runtime \
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
SCRIPT_URL=${SCRIPT_URL:-https://raw.githubusercontent.com/clems4ever/github-runner/main/vm/runner-vm.sh}

# Where "install" puts things, and who the service runs as.
INSTALL_BIN=${INSTALL_BIN:-/usr/local/bin/runner-vm.sh}
SERVICE_USER=${SERVICE_USER:-runner-vm}
SERVICE_STATE=${SERVICE_STATE:-/var/lib/runner-vm}

ENV_FILE=${ENV_FILE:-.env}
FORCE=false
CLEAN_ALL=false
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
    echo "  sudo apt-get install -y qemu-system-x86 qemu-utils cloud-image-utils"
  elif have dnf; then
    echo "  sudo dnf install -y qemu-kvm qemu-img cloud-utils"
  elif have pacman; then
    echo "  sudo pacman -S qemu-full cloud-image-utils"
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

# build_provision_script is what actually provisions the image: everything the
# Dockerfile installs, plus a Docker daemon and the packages a job needs to use
# /dev/kvm.
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
# uid 1000 as "ubuntu" — the same reasoning as the Dockerfile.
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

# What the Dockerfile does: download the runner package, verify it, extract it,
# install its dependencies. entrypoint.sh expects to find it at this path.
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
    sudo grep -c . /etc/runner-vm/env   # confirm the file has the lines you expect
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

resolve_token_files() {
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

# fetch_entrypoint returns the path to entrypoint.sh: the copy next to this
# script when run from a clone, otherwise a downloaded one.
fetch_entrypoint() {
  local work=$1 local_copy=""
  # Next to the script when it was copied out on its own, or one level up in a
  # clone, where entrypoint.sh sits at the root and is shared with the
  # container setup.
  for candidate in "${SCRIPT_DIR}/entrypoint.sh" "${SCRIPT_DIR}/../entrypoint.sh"; do
    if [[ -f "$candidate" ]]; then local_copy=$candidate; break; fi
  done

  if [[ -n "$local_copy" ]]; then
    cp "$local_copy" "${work}/entrypoint.sh"
  else
    curl -fsSL -o "${work}/entrypoint.sh" "$ENTRYPOINT_URL" \
      || die "cannot fetch entrypoint.sh from ${ENTRYPOINT_URL}"
  fi
  echo "${work}/entrypoint.sh"
}

# run_user_data drops the configuration and entrypoint.sh into the guest and
# starts it under systemd. The service writes to the console, so its output
# lands in the log this script streams.
run_user_data() {
  local entrypoint=$1
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
      GITHUB_URL='${GITHUB_URL}'
      RUNNER_TOKEN='${RUNNER_TOKEN}'
      RUNNER_NAME='${RUNNER_NAME}'
      RUNNER_LABELS='${RUNNER_LABELS}'
      RUNNER_GROUP='${RUNNER_GROUP}'
      EPHEMERAL='${EPHEMERAL}'
      DISABLE_UPDATE='${DISABLE_UPDATE}'
      RUNNER_STATE_DIR='/home/runner/.runner-state'

  # The very same script the container runs.
  - path: /usr/local/bin/entrypoint.sh
    permissions: '0755'
    owner: 'root:root'
    content: |
$(sed 's/^/      /' "$entrypoint")

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
      ExecStart=/usr/local/bin/entrypoint.sh
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
  if [[ -f "${VM_DIR}/qemu.pid" ]] && kill -0 "$(cat "${VM_DIR}/qemu.pid" 2>/dev/null)" 2>/dev/null; then
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

  local entrypoint; entrypoint=$(fetch_entrypoint "$VM_DIR")
  run_user_data "$entrypoint" > "${VM_DIR}/user-data"
  # A new instance ID on every boot, or cloud-init treats the golden image's
  # own build as the current instance and skips configuring the runner.
  echo -e "instance-id: ${VM_NAME}-$(date +%s)\nlocal-hostname: ${VM_NAME}" > "${VM_DIR}/meta-data"
  make_seed "${VM_DIR}/user-data" "${VM_DIR}/meta-data" "${VM_DIR}/seed.iso"

  # An overlay cannot be smaller than the image it is backed by.
  local golden_gb
  golden_gb=$(qemu-img info --output=json "$golden" | sed -n 's/.*"virtual-size":[[:space:]]*\([0-9]*\).*/\1/p' | head -1)
  golden_gb=$(( (golden_gb + 1073741823) / 1073741824 ))
  if [[ "$VM_DISK_GB" -lt "$golden_gb" ]]; then
    warn "--disk ${VM_DISK_GB}G is smaller than the golden image (${golden_gb}G), using ${golden_gb}G"
    VM_DISK_GB=$golden_gb
  fi

  # A copy-on-write overlay: the golden image stays untouched, and this disk is
  # a few megabytes until the job fills it.
  qemu-img create -q -f qcow2 -F qcow2 -b "$golden" "${VM_DIR}/disk.qcow2" "${VM_DISK_GB}G"

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
stop_services() {
  local unit failed=false
  local services; services=$(runner_vm_services)
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
stop_stray_vms() {
  local dir name pid waited
  shopt -s nullglob
  for dir in "$STATE_DIR"/vms/*/; do
    name=$(basename "$dir")
    pid=""
    [[ -f "${dir}qemu.pid" ]] && pid=$(cat "${dir}qemu.pid" 2>/dev/null || true)

    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      log "stopping VM ${name} (pid ${pid})"
      kill "$pid" 2>/dev/null || true
      waited=0
      while kill -0 "$pid" 2>/dev/null && [[ $waited -lt 15 ]]; do
        sleep 1
        waited=$((waited + 1))
      done
      kill -9 "$pid" 2>/dev/null || true
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

  echo "This will remove:"
  echo "  - every runner VM in ${STATE_DIR}/vms, stopping any that are running"
  if [[ "$all" == "true" ]]; then
    echo "  - the golden image and the cached cloud image"
    echo "  - the ssh key used to reach the VMs"
    echo "  - ${STATE_DIR}, entirely"
  else
    echo "  the images are kept, so the next run starts in seconds rather than"
    echo "  rebuilding; --all removes those and the ssh key too"
  fi

  local services; services=$(runner_vm_services)
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

  stop_services
  deregister_known_runners
  stop_stray_vms

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

# short_scope turns a URL into owner/repo, which is what identifies a runner at
# a glance; the scheme and host are the same for every row.
short_scope() {
  sed -E 's#^https?://[^/]+/##; s#/$##' <<<"${1:-}"
}

cmd_list() {
  local names="" name dir root unit
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
    if [[ "$repo" == "-" && -r /etc/runner-vm/env ]]; then
      repo=$(short_scope "$(sed -n 's/^GITHUB_URL=//p' /etc/runner-vm/env)")
      repo=${repo:--}
    fi
    [[ "$cpus" == "-" ]] && cpus=$VM_CPUS
    [[ "$mem"  == "-" ]] && mem="${VM_MEMORY_MB}M"
    [[ "$disk" == "-" ]] && disk="${VM_DISK_GB}G"

    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      state=running
      uptime=$(ps -o etime= -p "$pid" 2>/dev/null | tr -d ' ' || echo '-')
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
  echo "ssh into one with:  ssh -i ${SSH_KEY} -p <SSH> ubuntu@127.0.0.1"
  echo "consoles are under: <state dir>/vms/<NAME>/console.log"
  if [[ "$SERVICE_STATE" != "$STATE_DIR" && -d "$SERVICE_STATE" && ! -r "$SERVICE_STATE/vms" ]]; then
    echo
    warn "cannot read ${SERVICE_STATE}; rerun with sudo to see the VMs the service owns"
  fi
}

# systemd_unit prints the template unit. It lives here rather than in a file of
# its own so that a single copied script can install itself, and so the two
# cannot drift apart.
systemd_unit() {
  local credential=""
  # Only reference the credential file if there is one: LoadCredential fails
  # the unit when its source is missing.
  if [[ -f /etc/runner-vm/pat ]]; then
    credential="LoadCredential=pat:/etc/runner-vm/pat
Environment=GITHUB_TOKEN_FILE=%d/pat"
  elif [[ -f /etc/runner-vm/app.pem ]]; then
    credential="LoadCredential=app.pem:/etc/runner-vm/app.pem
Environment=GITHUB_APP_PRIVATE_KEY=%d/app.pem"
  fi

  cat <<UNIT
# Installed by runner-vm.sh install. Regenerate with: runner-vm.sh print-unit
#
# A template unit, so the instance name is the runner name:
#   systemctl enable --now runner-vm@build1

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

# systemd reads this as root before dropping to User=, so it can stay 0600
# root-owned and the credential is never readable by the service user.
EnvironmentFile=/etc/runner-vm/env
${credential}

ExecStart=${INSTALL_BIN} run --name %i

# Every start registers a new runner, because a VM keeps nothing, and that
# needs a credential that can mint registration tokens. Fail loudly rather than
# boot a VM that cannot register.
ExecStartPre=/bin/sh -c '[ -n "\$GITHUB_TOKEN" ] || [ -n "\$GITHUB_TOKEN_FILE" ] || [ -n "\$GITHUB_APP_ID" ] || { echo "set GITHUB_TOKEN or GITHUB_TOKEN_FILE (a PAT), or GITHUB_APP_ID with GITHUB_APP_PRIVATE_KEY: a pasted registration token expires an hour after it is issued and will not survive a reboot" >&2; exit 1; }'

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

# print_getting_started is what a fresh install leaves on the screen: the
# shortest path from "the script is here" to "a runner is registered", in the
# order the steps actually have to happen.
print_getting_started() {
  local url=${GITHUB_URL:-https://github.com/OWNER/REPO}
  cat <<GUIDE

  runner-vm is installed at ${INSTALL_BIN}

  It runs a self-hosted GitHub Actions runner inside a throwaway QEMU VM: one
  machine per runner, deleted when it stops, with its own Docker daemon and
  /dev/kvm so jobs can build images and boot VMs of their own.

  Boot your first runner
  ----------------------

  1. Check the host has KVM and nested virtualisation. It prints the fix for
     anything missing.

       sudo ${INSTALL_BIN} doctor

  2. Build the golden image. Once per host, a few minutes; every VM afterwards
     boots from a copy-on-write overlay on it in seconds.

       sudo ${INSTALL_BIN} build

  3. Give it a credential. A registration token from
     ${url}/settings/actions/runners/new
     works for one hour, which is enough to try it out:

       sudo ${INSTALL_BIN} run --url ${url} --token AAAA...

     For anything lasting, use a PAT or a GitHub App instead: a VM keeps no
     registration, so every boot needs a fresh registration token, and the
     script mints one per boot from either.

       sudoedit /etc/runner-vm/pat          # paste the PAT, save
       sudo ${INSTALL_BIN} run --url ${url} --github-token-file /etc/runner-vm/pat

     A fine-grained PAT needs Administration: Read and write on the repository,
     or the organisation's Self-hosted runners: Read and write.

  4. That runs in the foreground and the VM dies with it, which is the right
     way to check it works. To have runners start with the host instead:

       sudo ${INSTALL_BIN} install --service \\
         --url ${url} --github-token-file /etc/runner-vm/pat

     which adds a systemd unit, an unprivileged service user, builds the image
     and starts runner-vm@runner-1. Add more with:

       sudo systemctl enable --now runner-vm@runner-2

  Then
  ----

    ${INSTALL_BIN} list          what is running, with ssh ports and uptime
    ${INSTALL_BIN} --help        every flag
    sudo ${INSTALL_BIN} clean    stop everything, keep the image cache

  Jobs target it with "runs-on: self-hosted".

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

  # Check the credential before anything slow. Discovering a bad token after
  # six minutes of building an image is the wrong order to find out.
  resolve_token_files
  if [[ -n "$GITHUB_URL" && -n "$GITHUB_TOKEN" ]]; then
    log "checking the credential against ${GITHUB_URL}"
    mint_token "$GITHUB_URL" >/dev/null
    log "the credential can register runners"
  elif [[ -z "$GITHUB_TOKEN" && -z "$GITHUB_APP_ID" && -z "$RUNNER_TOKEN" ]]; then
    warn "no credential given; put one in /etc/runner-vm/env before starting a runner"
  fi

  if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --create-home --home-dir "$SERVICE_STATE" "$SERVICE_USER"
    log "created the ${SERVICE_USER} user"
  fi
  # /dev/kvm is rw for its group only, and the service user is not in it by
  # default. The group's name is read from the device rather than assumed.
  usermod -aG "$(stat -c %G /dev/kvm 2>/dev/null || echo kvm)" "$SERVICE_USER"
  install -d -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$SERVICE_STATE"

  install -d -m 0755 /etc/runner-vm
  if [[ -n "$GITHUB_URL" ]]; then
    printf 'GITHUB_URL=%s\n' "$GITHUB_URL" > /etc/runner-vm/env
    [[ -n "$RUNNER_LABELS" ]] && printf 'RUNNER_LABELS=%s\n' "$RUNNER_LABELS" >> /etc/runner-vm/env
    [[ "$EPHEMERAL" == "true" ]] && printf 'EPHEMERAL=true\n' >> /etc/runner-vm/env
    chmod 0600 /etc/runner-vm/env
    log "wrote /etc/runner-vm/env"
  elif [[ ! -f /etc/runner-vm/env ]]; then
    printf 'GITHUB_URL=\n' > /etc/runner-vm/env
    chmod 0600 /etc/runner-vm/env
    warn "no --url given; put GITHUB_URL in /etc/runner-vm/env before starting"
  fi

  # The credential is copied into a file of its own so it never appears in a
  # command line or in the unit.
  resolve_token_files
  if [[ -n "$GITHUB_TOKEN" ]]; then
    printf '%s' "$GITHUB_TOKEN" > /etc/runner-vm/pat
    chmod 0600 /etc/runner-vm/pat
    log "wrote /etc/runner-vm/pat"
  fi

  systemd_unit > /etc/systemd/system/runner-vm@.service
  chmod 0644 /etc/systemd/system/runner-vm@.service
  log "wrote /etc/systemd/system/runner-vm@.service"
  systemctl daemon-reload

  local instance=${RUNNER_NAME:-runner-1}

  if [[ "$INSTALL_BUILD" == "true" ]]; then
    log "building the golden image (a few minutes, once per host)"
    sudo -u "$SERVICE_USER" \
      env RUNNER_VM_HOME="$SERVICE_STATE" SCRIPT_URL="$SCRIPT_URL" \
      "$INSTALL_BIN" build \
      || die "the image build failed; fix it and rerun, or use --no-build"
  fi

  if [[ "$INSTALL_START" == "true" ]]; then
    systemctl enable --now "runner-vm@${instance}"
    log "started runner-vm@${instance}"
    log ""
    log "the runner appears at ${GITHUB_URL:-<your repo>}/settings/actions/runners in about a minute"
    log "watch it come up with:"
    log "  sudo journalctl -u runner-vm@${instance} -f"
  else
    log "installed. Start a runner with:"
    log "  sudo systemctl enable --now runner-vm@${instance}"
  fi
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
  [[ -d /etc/runner-vm ]] && echo "  - /etc/runner-vm, including any credentials in it"
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
  rm -rf /etc/runner-vm "$STATE_DIR"
  [[ "$service_state" != "$STATE_DIR" ]] && rm -rf "$service_state"
  have systemctl && systemctl daemon-reload 2>/dev/null || true

  # Last, because it is the file being executed.
  rm -f "$script"
  log "uninstalled"
}

# --------------------------------------------------------------------------
# Entry point
# --------------------------------------------------------------------------

# Empty when piped from curl, in which case entrypoint.sh is fetched rather
# than found next to the script.
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]:-/nonexistent}")" 2>/dev/null && pwd || echo /nonexistent)

main() {
  local command=run
  case "${1:-}" in
    run|build|doctor|clean|install|uninstall|list|ls|print-unit) command=$1; shift ;;
    -h|--help|help) print_help; exit 0 ;;
    --version) echo "runner-vm ${VERSION}"; exit 0 ;;
  esac

  # An .env from the container setup works as-is, and explicit flags still win.
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
      *)                die "unknown flag: $1 (try --help)" ;;
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

main "$@"
