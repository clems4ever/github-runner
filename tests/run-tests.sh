#!/usr/bin/env bash
# Tests for runner-vm.sh.
#
# These cover the parts that can be checked without a hypervisor: URL and
# credential handling, the generated cloud-init and systemd files, image
# naming, and the listing and cleanup commands against fixtures. Booting a
# guest needs /dev/kvm, which CI runners do not have, so that stays a manual
# check — see the testing notes in README.md.
#
# Usage: tests/run-tests.sh [name-filter]

# Most assignments here set globals that the sourced script reads — GITHUB_URL,
# STATE_DIR, RUNNER_TOKEN and so on — which shellcheck cannot see from here.
# The directive has to come before the first command to apply to the whole file.
# shellcheck disable=SC2034

set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1
# Absolute, and never a bare name: "source runner-vm.sh" searches PATH when the
# argument has no slash, so on a host where the script is installed the tests
# would silently exercise /usr/local/bin/runner-vm.sh instead of this checkout.
SCRIPT=$PWD/runner-vm.sh

# shellcheck source=../runner-vm.sh
source "$SCRIPT"
# The script sets -e for its own execution; tests must survive a failing case.
set +e

FILTER=${1:-}
PASS=0
FAIL=0
CURRENT=""

# ---------------------------------------------------------------------------
# Harness
# ---------------------------------------------------------------------------

test_case() {
  CURRENT=$1
  if [[ -n "$FILTER" && "$CURRENT" != *"$FILTER"* ]]; then
    CURRENT=""
    return 1
  fi
  return 0
}

ok() {
  PASS=$((PASS + 1))
  printf '  \033[32mok\033[0m   %s: %s\n' "$CURRENT" "$1"
}

bad() {
  FAIL=$((FAIL + 1))
  printf '  \033[31mFAIL\033[0m %s: %s\n' "$CURRENT" "$1"
  [[ $# -gt 1 ]] && printf '       %s\n' "${@:2}"
}

is() {
  local what=$1 want=$2 got=$3
  if [[ "$want" == "$got" ]]; then ok "$what"; else bad "$what" "want: ${want}" "got:  ${got}"; fi
}

contains() {
  local what=$1 haystack=$2 needle=$3
  if [[ "$haystack" == *"$needle"* ]]; then ok "$what"; else bad "$what" "missing: ${needle}"; fi
}

lacks() {
  local what=$1 haystack=$2 needle=$3
  if [[ "$haystack" != *"$needle"* ]]; then ok "$what"; else bad "$what" "unexpectedly present: ${needle}"; fi
}

# Both run the subject in a subshell: the functions under test are sourced, and
# several of them end in die(), which would otherwise exit the test run itself.
succeeds() {
  local what=$1; shift
  if ( "$@" ) >/dev/null 2>&1; then ok "$what"; else bad "$what" "command failed: $*"; fi
}

fails() {
  local what=$1; shift
  if ( "$@" ) >/dev/null 2>&1; then bad "$what" "command unexpectedly succeeded: $*"; else ok "$what"; fi
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------------------

if test_case "api-urls"; then
  is "repository URL"            "https://api.github.com/repos/o/r"        "$(github_api_prefix https://github.com/o/r)"
  is "organisation URL"          "https://api.github.com/orgs/myorg"       "$(github_api_prefix https://github.com/myorg)"
  is "trailing slash"            "https://api.github.com/orgs/myorg"       "$(github_api_prefix https://github.com/myorg/)"
  is "enterprise server, org"    "https://ghe.example.com/api/v3/orgs/o"   "$(github_api_prefix https://ghe.example.com/o)"
  is "enterprise server, repo"   "https://ghe.example.com/api/v3/repos/o/r" "$(github_api_prefix https://ghe.example.com/o/r)"
  is "short scope from URL"      "o/r"                                     "$(short_scope https://github.com/o/r)"
fi

if test_case "cpu-vendor"; then
  # The real ones read /proc/cpuinfo; override to test what is derived from it.
  cpu_vendor() { echo intel; }
  is "intel module" "kvm_intel" "$(kvm_module)"
  is "intel flag"   "vmx"       "$(nested_flag)"
  cpu_vendor() { echo amd; }
  is "amd module"   "kvm_amd"   "$(kvm_module)"
  is "amd flag"     "svm"       "$(nested_flag)"
  # What a VM is actually given. "-cpu host" alone would leak the host's
  # virtualisation extensions into every guest on a host that has nested
  # enabled, so the off case masks the flag rather than saying nothing.
  cpu_vendor() { echo intel; }
  VM_NESTED=true;  is "nested exposes the flag"  "host,+vmx" "$(cpu_model)"
  VM_NESTED=false; is "off masks it"             "host,-vmx" "$(cpu_model)"
  cpu_vendor() { echo amd; }
  VM_NESTED=true;  is "the amd flag likewise"    "host,+svm" "$(cpu_model)"
  VM_NESTED=false; is "and is masked likewise"   "host,-svm" "$(cpu_model)"

  cpu_vendor() { echo unknown; }
  is "no flag to name, so none is named" "host" "$(cpu_model)"
  VM_NESTED=true
  is "even when asked for"               "host" "$(cpu_model)"
  VM_NESTED=false

  is "unknown module" ""        "$(kvm_module)"
  is "unknown flag"   ""        "$(nested_flag)"
  unset -f cpu_vendor
fi

if test_case "packages"; then
  pkgs=$(effective_packages)
  contains "ships docker"          "$pkgs" "docker.io"
  contains "ships a compiler"      "$pkgs" "build-essential"
  contains "ships python"          "$pkgs" "python3"
  contains "ships qemu, for nested virtualisation in jobs" "$pkgs" "qemu-system-x86"
  is "sorted and deduplicated"     "$(sort -u <<<"$pkgs")" "$pkgs"

  base_hash=$(packages_hash)
  is "hash is stable"              "$base_hash" "$(packages_hash)"
  EXTRA_PACKAGES="ffmpeg" ; extra_hash=$(packages_hash)
  if [[ "$base_hash" != "$extra_hash" ]]; then ok "extra packages change the hash"; else bad "extra packages change the hash"; fi
  contains "extra packages are included" "$(effective_packages)" "ffmpeg"
  EXTRA_PACKAGES=""

  golden=$(basename "$(golden_path)")
  contains "image name carries the runner version" "$golden" "runner${RUNNER_VERSION}"
  contains "image name carries the package hash"   "$golden" "$(packages_hash)"
  contains "image name carries the release"        "$golden" "$UBUNTU_RELEASE"
fi

if test_case "read-secret"; then
  printf 'github_pat_PLAIN\n' > "$WORK/tok"
  is "reads a file"              "github_pat_PLAIN" "$(read_secret "$WORK/tok")"
  printf '  github_pat_PADDED  \n' > "$WORK/tok2"
  is "trims whitespace"          "github_pat_PADDED" "$(read_secret "$WORK/tok2")"
  is "reads stdin"               "github_pat_STDIN" "$(printf 'github_pat_STDIN' | read_secret -)"
  : > "$WORK/empty"
  fails "rejects an empty file"  read_secret "$WORK/empty"
  fails "rejects a missing file" read_secret "$WORK/nope"
fi

if test_case "credential-precedence"; then
  printf 'from_file\n' > "$WORK/pat"
  GITHUB_TOKEN="" GITHUB_TOKEN_FILE="$WORK/pat" RUNNER_TOKEN="" RUNNER_TOKEN_FILE=""
  resolve_token_files
  is "file is read when the variable is empty" "from_file" "$GITHUB_TOKEN"

  GITHUB_TOKEN="from_env" GITHUB_TOKEN_FILE="$WORK/pat"
  resolve_token_files
  is "the environment wins over the file"      "from_env" "$GITHUB_TOKEN"

  GITHUB_TOKEN="" GITHUB_TOKEN_FILE="" RUNNER_TOKEN="" RUNNER_TOKEN_FILE="$WORK/pat"
  resolve_token_files
  is "registration token file is read"         "from_file" "$RUNNER_TOKEN"
  GITHUB_TOKEN=""; GITHUB_TOKEN_FILE=""; RUNNER_TOKEN=""; RUNNER_TOKEN_FILE=""
fi

if test_case "app-jwt"; then
  if command -v openssl >/dev/null; then
    openssl genrsa -out "$WORK/app.pem" 2048 2>/dev/null
    GITHUB_APP_ID=123456 GITHUB_APP_PRIVATE_KEY="$WORK/app.pem"
    jwt=$(app_jwt)
    is "three dot-separated parts" "3" "$(awk -F. '{print NF}' <<<"$jwt")"

    unpad() { local v=$1; while (( ${#v} % 4 )); do v="${v}="; done; printf '%s' "$v" | tr -- '-_' '+/' | openssl base64 -d -A; }
    contains "header names RS256"   "$(unpad "$(cut -d. -f1 <<<"$jwt")")" '"alg":"RS256"'
    contains "payload carries the app id" "$(unpad "$(cut -d. -f2 <<<"$jwt")")" '"iss":"123456"'
    is "base64url, no unsafe characters" "0" "$(tr -cd '+/=' <<<"$jwt" | wc -c)"

    # The signature is what GitHub actually checks.
    printf '%s.%s' "$(cut -d. -f1 <<<"$jwt")" "$(cut -d. -f2 <<<"$jwt")" > "$WORK/signed"
    unpad "$(cut -d. -f3 <<<"$jwt")" > "$WORK/sig"
    openssl rsa -in "$WORK/app.pem" -pubout -out "$WORK/pub.pem" 2>/dev/null
    succeeds "signature verifies against the public key" \
      openssl dgst -sha256 -verify "$WORK/pub.pem" -signature "$WORK/sig" "$WORK/signed"

    # Clock skew and GitHub's ten-minute ceiling.
    payload=$(unpad "$(cut -d. -f2 <<<"$jwt")")
    iat=$(sed -n 's/.*"iat":\([0-9]*\).*/\1/p' <<<"$payload")
    exp=$(sed -n 's/.*"exp":\([0-9]*\).*/\1/p' <<<"$payload")
    if (( exp - iat < 600 )); then ok "lifetime is inside GitHub's ten-minute limit"; else bad "lifetime is inside GitHub's ten-minute limit" "$((exp - iat))s"; fi
    if (( iat < $(date +%s) )); then ok "issued-at is backdated against clock skew"; else bad "issued-at is backdated against clock skew"; fi
    GITHUB_APP_ID=""; GITHUB_APP_PRIVATE_KEY=""
  else
    printf '  skip app-jwt: openssl not installed\n'
  fi
fi

if test_case "build-user-data"; then
  SSH_KEY="$WORK/id"; printf 'ssh-ed25519 AAAA test\n' > "$WORK/id.pub"
  ud=$(build_user_data)
  contains "is a cloud-config"                    "$ud" "#cloud-config"
  contains "installs the packages"                "$ud" "  - docker.io"
  contains "provisions through a script file"     "$ud" "/usr/local/bin/runner-vm-provision.sh"
  contains "runcmd invokes that script"           "$ud" "runcmd:"
  contains "the script declares bash"             "$ud" "#!/bin/bash"
  contains "verifies the runner tarball checksum" "$ud" "sha256sum -c -"
  contains "adds the runner to the kvm group"     "$ud" "usermod -aG kvm runner"
  contains "adds the runner to the docker group"  "$ud" "usermod -aG docker runner"
  contains "installs the GitHub CLI"              "$ud" "cli.github.com"
  contains "generates a UTF-8 locale"             "$ud" "locale-gen en_US.UTF-8"
  contains "reports success on the console"       "$ud" "$BUILD_OK_SENTINEL"

  if command -v python3 >/dev/null; then
    printf '%s' "$ud" > "$WORK/build.yaml"
    succeeds "is valid YAML" python3 -c "import yaml,sys; yaml.safe_load(open('$WORK/build.yaml'))"
  fi
fi

if test_case "run-user-data"; then
  SSH_KEY="$WORK/id"; printf 'ssh-ed25519 AAAA test\n' > "$WORK/id.pub"
  VM_NAME=vm-under-test
  GITHUB_URL=https://github.com/o/r RUNNER_TOKEN='tok"with\quotes' RUNNER_NAME=vm-under-test
  RUNNER_LABELS=a,b RUNNER_GROUP=Default EPHEMERAL=false DISABLE_UPDATE=false
  ud=$(run_user_data)

  contains "writes the runner environment"   "$ud" "/etc/runner-vm/runner.env"
  contains "ships the guest runner script"   "$ud" "/usr/local/bin/runner-vm-runner.sh"
  contains "defines the runner service"      "$ud" "github-runner.service"
  contains "runs the runner unprivileged"    "$ud" "User=runner"
  contains "powers off with full privileges" "$ud" "ExecStopPost=+/usr/bin/systemctl poweroff"
  contains "console carries the runner's output" "$ud" "StandardOutput=journal+console"
  # A quote or a backslash in a value must not be able to end the assignment
  # and turn the rest of the line into something else.
  contains "escapes quotes in the env file"  "$ud" 'RUNNER_TOKEN="tok\"with\\quotes"'
  contains "keeps the credential root-only"  "$ud" "permissions: '0600'"

  if command -v python3 >/dev/null; then
    printf '%s' "$ud" > "$WORK/run.yaml"
    succeeds "is valid YAML" python3 -c "import yaml; yaml.safe_load(open('$WORK/run.yaml'))"
    # The runner script is embedded as an indented block; it has to come back
    # out byte for byte, and still be valid bash, or the guest runs something
    # mangled and the failure only shows up on the VM's console.
    guest_runner_script > "$WORK/guest.sh"
    python3 - "$WORK/run.yaml" "$WORK/guest.sh" <<'PY' && ok "the guest script round-trips unchanged" || bad "the guest script round-trips unchanged"
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
embedded = [f for f in doc['write_files'] if f['path'].endswith('runner-vm-runner.sh')][0]['content']
sys.exit(0 if embedded.rstrip('\n') == open(sys.argv[2]).read().rstrip('\n') else 1)
PY
  fi
fi

if test_case "guest-script"; then
  guest=$(guest_runner_script)
  succeeds "is valid bash"                       bash -n <(printf '%s' "$guest")
  contains "registers unattended"                "$guest" "--unattended"
  contains "takes over a stale entry of the same name" "$guest" "--replace"
  contains "passes the token to config.sh"       "$guest" '--token "$RUNNER_TOKEN"'
  contains "execs run.sh, so it receives SIGTERM" "$guest" "exec ./run.sh"
  contains "requires a URL"                      "$guest" 'GITHUB_URL:?'
  contains "requires a token"                    "$guest" 'RUNNER_TOKEN:?'
  # The VM is thrown away, so there is nothing to restore and no state to keep.
  lacks    "carries no saved-registration logic" "$guest" "RUNNER_STATE_DIR"

  EPHEMERAL=true
  contains "ephemeral runners are configured as such" "$guest" "--ephemeral"
  EPHEMERAL=false
fi

if test_case "meta-data"; then
  # The instance id has to differ on every boot, or cloud-init treats the golden
  # image's own build as the current instance and skips the modules that
  # register the runner. It is generated inline in cmd_run, so this checks the
  # shape rather than calling a function.
  contains "instance id is unique per boot" \
    "$(grep 'instance-id:' "$SCRIPT")" 'instance-id: ${VM_NAME}-$(date +%s)'
fi

if test_case "systemd-unit"; then
  unit=$(systemd_unit)
  contains "is a template unit"                     "$unit" "ExecStart=${INSTALL_BIN} run --name %i"
  contains "signals the script, not qemu"           "$unit" "KillMode=mixed"
  contains "waits for the network"                  "$unit" "After=network-online.target"
  # Requiring it fails on every host: udev does not tag misc devices for
  # systemd, so dev-kvm.device never activates. The comment explaining that is
  # expected to mention it; the directive must not exist.
  lacks    "does not require the kvm device unit"   "$unit" "Requires=dev-kvm.device"
  contains "restarts, which recycles ephemeral VMs" "$unit" "Restart=always"
  contains "allows a job to finish before stopping" "$unit" "TimeoutStopSec=3660"
  contains "refuses to start with no credential"    "$unit" "ExecStartPre="
  contains "runs as the service user"               "$unit" "User=${SERVICE_USER}"

  # One unit serves every runner on the host, so nothing in it may name a
  # repository or a credential file: %i has to be what picks them.
  contains "reads the shared configuration"         "$unit" "EnvironmentFile=-${ETC_DIR}/env"
  contains "then the runner's own, which wins"      "$unit" "EnvironmentFile=-${ETC_DIR}/env.%i"
  contains "loads the credentials directory"        "$unit" "LoadCredential=${CRED_ID}:${CRED_DIR}"
  lacks    "names no repository"                    "$unit" "GITHUB_URL="
  lacks    "names no credential file"               "$unit" "GITHUB_TOKEN_FILE="

  if command -v systemd-analyze >/dev/null; then
    printf '%s' "$unit" > "$WORK/runner-vm@.service"
    out=$(systemd-analyze verify "$WORK/runner-vm@.service" 2>&1)
    # The only expected complaint is the binary not being installed here.
    if [[ -z "$(grep -v 'not executable' <<<"$out" | grep -v '^$')" ]]; then
      ok "systemd accepts the unit"
    else
      bad "systemd accepts the unit" "$out"
    fi
  fi
fi

if test_case "ports"; then
  STATE_DIR="$WORK/state"; mkdir -p "$STATE_DIR/vms/a" "$STATE_DIR/vms/b"
  VM_DIR="$STATE_DIR/vms/a"; port_a=$(claim_port)
  VM_DIR="$STATE_DIR/vms/b"; port_b=$(claim_port)
  if [[ "$port_a" != "$port_b" ]]; then ok "two VMs never claim the same port"; else bad "two VMs never claim the same port" "both got ${port_a}"; fi
  is "the claim is recorded for the next VM" "$port_a" "$(cat "$STATE_DIR/vms/a/ssh_port")"
fi

if test_case "pid-alive"; then
  # pid 1 belongs to root, so unless these tests run as root "kill -0 1" fails
  # with EPERM — which is the whole reason pid_alive does not use it. That is
  # what made an unprivileged "list" report the service's VMs as stopped.
  succeeds "sees a process it cannot signal" pid_alive 1
  fails    "sees through a dead pid"         pid_alive 999999999
  fails    "sees through an empty pid"       pid_alive ""
fi

if test_case "image-size"; then
  if have qemu-img; then
    qemu-img create -q -f qcow2 "$WORK/backing.qcow2" 30G
    is "reads the virtual size in bytes" "32212254720" "$(image_virtual_bytes "$WORK/backing.qcow2")"

    # qemu-img creates this overlay without complaint, and a guest cannot boot
    # from it: the truncated disk loses the backup GPT and invalidates the
    # primary one, so the kernel finds no partitions at all.
    qemu-img create -q -f qcow2 -F qcow2 -b "$WORK/backing.qcow2" "$WORK/small.qcow2" 20G
    is "a truncating overlay is smaller than its backing image" "21474836480" \
      "$(image_virtual_bytes "$WORK/small.qcow2")"
    is "nothing to read from a missing image" "" "$(image_virtual_bytes "$WORK/absent.qcow2")"
  else
    ok "skipped: no qemu-img on this host"
  fi
fi

if test_case "list"; then
  STATE_DIR="$WORK/list-state"
  SERVICE_STATE="$WORK/list-service"
  # Never the host's own: a developer machine with runners installed on it
  # would otherwise feed its real configuration into these rows.
  ETC_DIR="$WORK/list-etc"; CRED_DIR="$ETC_DIR/creds"; mkdir -p "$ETC_DIR"
  mkdir -p "$STATE_DIR/vms/hand-1" "$SERVICE_STATE/vms/runner-1"
  cat > "$SERVICE_STATE/vms/runner-1/meta" <<'META'
NAME=runner-1
URL=https://github.com/clems4ever/runyard
EPHEMERAL=false
CPUS=4
MEMORY_MB=8192
DISK_GB=60
NESTED=true
META
  echo 2222 > "$SERVICE_STATE/vms/runner-1/ssh_port"
  cat > "$STATE_DIR/vms/hand-1/meta" <<'META'
NAME=hand-1
URL=https://github.com/o/other
EPHEMERAL=true
CPUS=2
MEMORY_MB=4096
DISK_GB=40
NESTED=false
META

  out=$(cmd_list)
  contains "finds the VM the service owns"     "$out" "runner-1"
  contains "finds a hand-started VM elsewhere" "$out" "hand-1"
  contains "shows the repository"              "$out" "clems4ever/runyard"
  contains "shows the core count"              "$out" "4"
  contains "shows the memory"                  "$out" "8192M"
  contains "shows the disk"                    "$out" "60G"
  contains "shows the ssh port"                "$out" "2222"
  contains "marks ephemeral VMs"               "$out" "o/other*"
  # Which VMs can run VMs of their own is the difference between a runner that
  # can build images and one that cannot, so it belongs in the table.
  contains "says which VMs have nested virtualisation" "$out" "60G     yes"
  contains "and which do not"                          "$out" "40G     no"
  contains "reports a VM with no process as stopped" "$out" "runner-1         stopped"
  # Both state directories are in play, so no single key path applies.
  contains "names the state directories"       "$out" "$SERVICE_STATE"

  # A VM whose process is alive, using this shell as a stand-in for QEMU.
  echo $$ > "$SERVICE_STATE/vms/runner-1/qemu.pid"
  out=$(cmd_list)
  contains "reports a live VM as running" "$out" "runner-1         running"
  lacks    "fills in the uptime"          "$(sed -n 's/^runner-1 *running *[^ ]* *[^ ]* *[^ ]* *[^ ]* *[^ ]* *\([^ ]*\).*/\1/p' <<<"$out")" "-"

  # One state directory in play, so the key to use is unambiguous.
  STATE_DIR=$SERVICE_STATE
  contains "shows the key for the VMs it found" "$(cmd_list)" "${SERVICE_STATE}/ssh/id_ed25519"

  STATE_DIR="$WORK/empty-state"; SERVICE_STATE="$WORK/empty-service"; mkdir -p "$STATE_DIR"
  contains "says so when there is nothing" "$(cmd_list)" "no VMs and no services"
fi

# A runner whose VM has never booted has no meta file to read, so its row comes
# from the service configuration. With several repositories on one host that
# has to be read per runner: reading the shared file alone showed every row as
# whichever repository was installed first.
if test_case "list-per-runner"; then
  STATE_DIR="$WORK/multi-state"; SERVICE_STATE="$WORK/multi-service"
  ETC_DIR="$WORK/multi-etc"; CRED_DIR="$ETC_DIR/creds"
  mkdir -p "$ETC_DIR" "$SERVICE_STATE/vms/web" "$SERVICE_STATE/vms/spare"
  printf 'GITHUB_URL=https://github.com/o/shared\nVM_CPUS=2\n'   > "$ETC_DIR/env"
  printf 'GITHUB_URL=https://github.com/o/web\nVM_CPUS=8\nVM_NESTED=true\n' > "$ETC_DIR/env.web"

  out=$(cmd_list)
  contains "shows each runner its own repository" "$out" "o/web"
  contains "and the shared one for the rest"      "$out" "o/shared"
  contains "with the size that runner will get"   "$out" "8"
  contains "and whether it will get nested"      "$out" "yes"
  contains "which the others will not"           "$out" "no"

  is "a runner's own setting wins"          "https://github.com/o/web"    "$(configured_field web GITHUB_URL)"
  is "and falls back to the shared file"    "2"                           "$(configured_field spare VM_CPUS)"
  is "as does a runner with no file at all" "https://github.com/o/shared" "$(configured_field spare GITHUB_URL)"
  is "nothing for a key nobody sets"        ""                            "$(configured_field web RUNNER_LABELS)"
fi

# Whether a job is on a runner is only knowable from GitHub: the host can see
# that a VM is running, not that anything is happening inside it.
if test_case "runner-jobs"; then
  if have jq; then
    # Counted in a file, not a variable: cmd_list is read through a command
    # substitution, and a subshell cannot hand a counter back.
    api_call() {
      echo call >> "$WORK/api-calls"
      cat <<'J'
{"total_count":3,"runners":[
 {"id":1,"name":"web-1","status":"online","busy":true},
 {"id":2,"name":"web-2","status":"online","busy":false},
 {"id":3,"name":"web-3","status":"offline","busy":false}]}
J
    }
    : > "$WORK/api-calls"
    states=$(runner_states https://github.com/o/web)
    contains "a runner on a job is busy"        "$states" "web-1	busy"
    contains "one waiting for work is idle"     "$states" "web-2	idle"
    # Offline is not idle: the VM is gone or has not registered, and calling
    # that idle would read as "safe to remove" for the one case it is not.
    contains "one GitHub cannot see is offline" "$states" "web-3	offline"

    STATE_DIR="$WORK/jobs-state"; SERVICE_STATE="$WORK/jobs-service"
    ETC_DIR="$WORK/jobs-etc"; CRED_DIR="$ETC_DIR/creds"
    mkdir -p "$CRED_DIR" "$STATE_DIR"
    printf 'tok' > "$CRED_DIR/pat"
    for n in web-1 web-2 web-3; do
      mkdir -p "$SERVICE_STATE/vms/$n"
      printf 'NAME=%s\nURL=https://github.com/o/web\nCPUS=2\nMEMORY_MB=4096\nDISK_GB=40\nNESTED=false\n' \
        "$n" > "$SERVICE_STATE/vms/$n/meta"
    done

    : > "$WORK/api-calls"
    SHOW_JOBS=true
    out=$(cmd_list)
    SHOW_JOBS=false
    contains "the column is only there when asked for" "$out" "JOB"
    contains "and carries what each runner is doing"   "$out" "web-1            stopped   -         busy"
    # Three replicas on one repository are one question, not three.
    is "one call serves every runner in a scope" "1" "$(grep -c . "$WORK/api-calls")"

    lacks "no column without the flag" "$(cmd_list)" "JOB"

    unset -f api_call
    ETC_DIR=${RUNNER_VM_ETC:-/etc/runner-vm}; CRED_DIR="${ETC_DIR}/creds"
  fi
fi

if test_case "clean"; then
  STATE_DIR="$WORK/clean-state"
  mkdir -p "$STATE_DIR/vms/gone" "$STATE_DIR/images"
  echo 999999999 > "$STATE_DIR/vms/gone/qemu.pid"   # a VM whose process died
  echo golden > "$STATE_DIR/images/golden-test.qcow2"
  GITHUB_URL="" ASSUME_YES=true CLEAN_ALL=false

  out=$(cmd_clean </dev/null 2>&1)
  contains "removes the leftover VM" "$out" "removed the VM gone"
  if [[ ! -d "$STATE_DIR/vms/gone" ]]; then ok "the VM directory is gone"; else bad "the VM directory is gone"; fi
  if [[ -f "$STATE_DIR/images/golden-test.qcow2" ]]; then ok "the image cache is kept"; else bad "the image cache is kept"; fi

  CLEAN_ALL=true
  cmd_clean </dev/null >/dev/null 2>&1
  if [[ ! -d "$STATE_DIR" ]]; then ok "--all removes the state directory"; else bad "--all removes the state directory"; fi

  # Without a terminal to confirm at, it must refuse rather than assume.
  STATE_DIR="$WORK/clean-state2"; mkdir -p "$STATE_DIR/vms"
  ASSUME_YES=false CLEAN_ALL=false
  fails "refuses unattended without --yes" bash -c "source $SCRIPT; STATE_DIR='$STATE_DIR' ASSUME_YES=false cmd_clean </dev/null"
fi

if test_case "clean-one"; then
  STATE_DIR="$WORK/one-state"
  mkdir -p "$STATE_DIR/vms/keep" "$STATE_DIR/vms/drop" "$STATE_DIR/images"
  echo golden > "$STATE_DIR/images/golden-test.qcow2"
  GITHUB_URL="" ASSUME_YES=true CLEAN_ALL=false

  CLEAN_TARGETS=(drop)
  cmd_clean </dev/null >/dev/null 2>&1

  if [[ ! -d "$STATE_DIR/vms/drop" ]]; then ok "the named VM is removed"; else bad "the named VM is removed"; fi
  if [[ -d "$STATE_DIR/vms/keep" ]]; then ok "the others are left alone"; else bad "the others are left alone"; fi
  if [[ -f "$STATE_DIR/images/golden-test.qcow2" ]]; then ok "the images are left alone"; else bad "the images are left alone"; fi

  # Naming something that does not exist is a mistake worth reporting, not a
  # silent no-op that looks like success.
  CLEAN_TARGETS=(nosuchvm)
  fails "refuses a name that matches no VM" \
    bash -c "source $SCRIPT; STATE_DIR='$STATE_DIR' ASSUME_YES=true CLEAN_TARGETS=(nosuchvm) cmd_clean </dev/null"

  fails "refuses --all together with a name" \
    bash -c "source $SCRIPT; STATE_DIR='$STATE_DIR' ASSUME_YES=true CLEAN_ALL=true CLEAN_TARGETS=(keep) cmd_clean </dev/null"

  CLEAN_TARGETS=()
fi

if test_case "help"; then
  out=$(print_help)
  contains "lists the commands" "$out" "Commands:"
  contains "documents install"  "$out" "install"
  contains "documents the token flags" "$out" "--github-token-file"
fi

if test_case "service-config"; then
  # The unit runs "run --name %i" and takes everything else from this file, so
  # a setting install accepts but does not record is one it silently ignored —
  # which is what happened to --cpus.
  GITHUB_URL=https://github.com/o/r
  VM_CPUS=8 VM_MEMORY_MB=16384 VM_DISK_GB=100 VM_NESTED=false
  RUNNER_GROUP=mygroup EPHEMERAL=true RUNNER_LABELS=big GITHUB_APP_ID=42
  env_out=$(service_env_file)

  is "records the cores asked for"  "VM_CPUS=8"           "$(grep '^VM_CPUS=' <<<"$env_out")"
  is "records the memory"           "VM_MEMORY_MB=16384"  "$(grep '^VM_MEMORY_MB=' <<<"$env_out")"
  is "records the disk"             "VM_DISK_GB=100"      "$(grep '^VM_DISK_GB=' <<<"$env_out")"
  is "records nested virtualisation" "VM_NESTED=false"    "$(grep '^VM_NESTED=' <<<"$env_out")"
  is "records the runner group"     "RUNNER_GROUP=mygroup" "$(grep '^RUNNER_GROUP=' <<<"$env_out")"
  is "records ephemeral"            "EPHEMERAL=true"      "$(grep '^EPHEMERAL=' <<<"$env_out")"
  is "records the labels"           "RUNNER_LABELS=big"   "$(grep '^RUNNER_LABELS=' <<<"$env_out")"
  is "records the app id"           "GITHUB_APP_ID=42"    "$(grep '^GITHUB_APP_ID=' <<<"$env_out")"
  is "records the repository"       "GITHUB_URL=https://github.com/o/r" "$(grep '^GITHUB_URL=' <<<"$env_out")"

  # The credential is never in there: it lives in its own root-only file.
  lacks "keeps the credential out of it" "$env_out" "GITHUB_TOKEN="

  # Empty optional values are left out rather than written blank, which
  # systemd would read as a deliberate empty string.
  RUNNER_LABELS="" GITHUB_APP_ID=""
  lacks "omits labels when there are none" "$(service_env_file)" "RUNNER_LABELS="

  # This file is read after the shared one, so a blank value would not fall
  # back to the shared setting: it would override it with nothing.
  GITHUB_URL=""
  lacks "omits the repository when there is none" "$(service_env_file)" "GITHUB_URL="
  contains "says which runner it belongs to" "$(service_env_file web)" "runner-vm@web"
  GITHUB_URL=https://github.com/o/r

  VM_CPUS=2 VM_MEMORY_MB=4096 VM_DISK_GB=40 VM_NESTED=false RUNNER_GROUP=Default EPHEMERAL=false
fi

# The unit hands systemd the whole credentials directory, because a template
# unit cannot name a file that might not be there. Which of them a runner uses
# is decided here instead.
if test_case "service-credential"; then
  creds="$WORK/creds"; mkdir -p "$creds"
  printf 'shared'     > "${creds}/${CRED_ID}_pat"
  printf 'web-only'   > "${creds}/${CRED_ID}_pat.web"
  printf 'shared-key' > "${creds}/${CRED_ID}_app.pem"
  printf 'web-key'    > "${creds}/${CRED_ID}_app.web.pem"

  CREDENTIALS_DIRECTORY=$creds RUNNER_NAME=web
  is "prefers the runner's own PAT"     "${creds}/${CRED_ID}_pat.web"     "$(service_credential pat)"
  # The name goes before the suffix, not after it: app.web.pem, not app.pem.web.
  is "prefers the runner's own app key" "${creds}/${CRED_ID}_app.web.pem" "$(service_credential app.pem)"

  RUNNER_NAME=api
  is "falls back to the shared PAT"     "${creds}/${CRED_ID}_pat"         "$(service_credential pat)"
  is "falls back to the shared app key" "${creds}/${CRED_ID}_app.pem"     "$(service_credential app.pem)"

  # Run by hand there is no credentials directory, and nothing may be invented.
  CREDENTIALS_DIRECTORY=""
  is "nothing outside the service"      ""                                "$(service_credential pat)"

  CREDENTIALS_DIRECTORY=$creds RUNNER_NAME=web
  GITHUB_TOKEN="" GITHUB_TOKEN_FILE="" RUNNER_TOKEN="" RUNNER_TOKEN_FILE="" GITHUB_APP_PRIVATE_KEY=""
  resolve_token_files
  is "the runner's credential is read"  "web-only"                        "$GITHUB_TOKEN"
  is "so is its app key"                "${creds}/${CRED_ID}_app.web.pem" "$GITHUB_APP_PRIVATE_KEY"

  # An older unit names the file itself, and a flag or the environment must
  # keep working, so neither may be overwritten by what systemd loaded.
  GITHUB_TOKEN="" GITHUB_TOKEN_FILE="$WORK/pat" GITHUB_APP_PRIVATE_KEY=""
  resolve_token_files
  is "a named file still wins"          "from_file"                       "$GITHUB_TOKEN"

  CREDENTIALS_DIRECTORY="" RUNNER_NAME=""
  GITHUB_TOKEN="" GITHUB_TOKEN_FILE="" RUNNER_TOKEN="" RUNNER_TOKEN_FILE="" GITHUB_APP_PRIVATE_KEY=""
fi

# What install files where. A second repository must not disturb the first, and
# one PAT that covers both should not end up copied into two files to rotate.
if test_case "credential-store"; then
  ETC_DIR="$WORK/store"; CRED_DIR="${ETC_DIR}/creds"
  mkdir -p "$CRED_DIR"
  GITHUB_APP_PRIVATE_KEY=""

  # The layout before per-runner credentials existed: one file in ETC_DIR.
  # Install has to find it before it moves it, or upgrading a host would ask
  # for a credential it already has.
  printf 'legacy' > "${ETC_DIR}/pat"
  is "an older flat credential is found" "${ETC_DIR}/pat" "$(stored_credential runner-1)"
  migrate_flat_credentials >/dev/null
  is "moves an older flat credential"  "legacy" "$(cat "${CRED_DIR}/pat")"
  succeeds "and leaves nothing behind" test ! -e "${ETC_DIR}/pat"
  is "then it is found in its new home" "${CRED_DIR}/pat" "$(stored_credential runner-1)"

  rm -f "${CRED_DIR}/pat"
  store_credentials tok1 web >/dev/null
  is "the first credential is the shared one" "tok1" "$(cat "${CRED_DIR}/pat")"
  is "root-only"                              "600"  "$(stat -c %a "${CRED_DIR}/pat")"
  succeeds "with no per-runner copy"          test ! -e "${CRED_DIR}/pat.web"

  store_credentials tok1 api >/dev/null
  succeeds "the same token is not copied again" test ! -e "${CRED_DIR}/pat.api"
  is "stored_credential falls back to it"     "${CRED_DIR}/pat" "$(stored_credential api)"

  store_credentials tok2 api >/dev/null
  is "a different token goes under the runner" "tok2" "$(cat "${CRED_DIR}/pat.api")"
  is "and the shared one is untouched"         "tok1" "$(cat "${CRED_DIR}/pat")"
  is "which is what that runner then uses"     "${CRED_DIR}/pat.api" "$(stored_credential api)"
  is "while the other still shares"            "${CRED_DIR}/pat"     "$(stored_credential web)"

  # Going back to the shared token removes the copy rather than leaving a
  # second file holding the same secret.
  store_credentials tok1 api >/dev/null
  succeeds "a token back in step is dropped"   test ! -e "${CRED_DIR}/pat.api"

  is "nothing to find on a bare host" "" "$(CRED_DIR=$WORK/none stored_credential web)"

  # Replicas share a repository, so a credential of their own has to reach
  # every one of them: a file for the first alone would leave the rest falling
  # back to the host-wide PAT, which belongs to a different repository.
  store_credentials tok3 web-1 web-2 web-3 >/dev/null
  is "each replica gets the repository's own token" "tok3" "$(cat "${CRED_DIR}/pat.web-2")"
  is "and they all agree"                           "tok3" "$(cat "${CRED_DIR}/pat.web-3")"
  is "which is what each of them resolves to"       "${CRED_DIR}/pat.web-3" "$(stored_credential web-3)"

  # Rotating is the same command with the new token, so every replica has to be
  # rewritten, not just the first.
  store_credentials tok4 web-1 web-2 web-3 >/dev/null
  is "rotating reaches every replica"               "tok4" "$(cat "${CRED_DIR}/pat.web-3")"

  ETC_DIR=${RUNNER_VM_ETC:-/etc/runner-vm}; CRED_DIR="${ETC_DIR}/creds"
fi

# How many runners install sets up for one repository, and what they are named.
if test_case "replicas"; then
  is "one runner keeps the plain name" "web"             "$(replica_names web 1)"
  is "several are numbered"            "web-1 web-2 web-3" "$(replica_names web 3 | tr '\n' ' ' | sed 's/ $//')"
  is "zero is treated as one"          "web"             "$(replica_names web 0)"

  ETC_DIR="$WORK/replicas"; mkdir -p "$ETC_DIR"
  : > "$ETC_DIR/env.web-1"; : > "$ETC_DIR/env.web-2"; : > "$ETC_DIR/env.web-3"
  : > "$ETC_DIR/env.web-other"   # not a replica: the suffix is not a number
  : > "$ETC_DIR/env.api-1"       # another repository's replicas

  is "nothing stale when the count is unchanged" "" "$(stale_replicas web 3)"
  is "scaling down names what is left over"      "web-3" "$(stale_replicas web 2)"
  is "and all of them when going back to one"    "web-1 web-2 web-3" \
     "$(stale_replicas web 1 | tr '\n' ' ' | sed 's/ $//')"
  is "a name that is not a replica is left out"  "" "$(stale_replicas web 3 | grep web-other)"
  is "and so is another repository's"            "" "$(stale_replicas web 3 | grep api)"
  is "nothing for a runner that has none"        "" "$(stale_replicas lonely 1)"

  ETC_DIR=${RUNNER_VM_ETC:-/etc/runner-vm}; CRED_DIR="${ETC_DIR}/creds"
fi

if test_case "install-guards"; then
  fails "install refuses to run as a normal user" \
    bash -c "source $SCRIPT; id() { echo 1000; }; cmd_install"
fi

# ---------------------------------------------------------------------------

echo
if [[ $FAIL -eq 0 ]]; then
  printf '\033[32m%d passed\033[0m\n' "$PASS"
else
  printf '\033[31m%d failed\033[0m, %d passed\n' "$FAIL" "$PASS"
fi
exit $(( FAIL > 0 ? 1 : 0 ))
