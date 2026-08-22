#!/usr/bin/env bash
# Installs runner-fleet on this host, and upgrades it in place.
#
#   curl -fsSL https://raw.githubusercontent.com/clems4ever/github-runner/main/install.sh | sudo bash
#
# Rerunning it is how you upgrade. That is deliberately uneventful: the daemon
# supervises nothing, so replacing the binary and restarting it leaves every
# runner on the host running, jobs included.
set -euo pipefail

REPO=${REPO:-clems4ever/github-runner}
VERSION=${VERSION:-latest}
INSTALL_BIN=${INSTALL_BIN:-/usr/local/bin/runner-fleet}
SERVICE_USER=${SERVICE_USER:-runner-fleet}
UNIT=/etc/systemd/system/runner-fleetd.service
ETC_DIR=/etc/runner-fleet
# Where the UI listens. An upgrade keeps whatever the last install chose: a
# rerun must not move the UI to a different port because nobody remembered to
# repeat the flag.
ADDR=${ADDR:-}

# Set these to install without being asked anything, which is what a
# configuration management tool wants.
FLEET_USER=${FLEET_USER:-}
FLEET_PASSWORD=${FLEET_PASSWORD:-}
ASSUME_YES=${ASSUME_YES:-false}

log()  { echo "[runner-fleet] $*"; }
warn() { echo "[runner-fleet] warning: $*" >&2; }
die()  { echo "[runner-fleet] error: $*" >&2; exit 1; }

[[ $(id -u) -eq 0 ]] || die "this has to run as root: pipe it into 'sudo bash'"

for tool in curl tar systemctl; do
  command -v "$tool" >/dev/null || die "$tool is required"
done

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

# ---------------------------------------------------------------------------
# Fetch and verify
# ---------------------------------------------------------------------------

if [[ "$VERSION" == "latest" ]]; then
  log "finding the latest release"
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [[ -n "$VERSION" ]] || die "could not find a release; set VERSION=vX.Y.Z"
fi
NUMBER=${VERSION#v}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

ARCHIVE="runner-fleet_${NUMBER}_linux_${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"

log "downloading ${ARCHIVE}"
curl -fsSL -o "${WORK}/${ARCHIVE}" "${BASE}/${ARCHIVE}" \
  || die "could not download ${BASE}/${ARCHIVE}"

# Verified before anything is unpacked: this binary ends up running as root and
# holding a token that administers repositories.
if curl -fsSL -o "${WORK}/checksums.txt" "${BASE}/checksums.txt"; then
  ( cd "$WORK" && grep " ${ARCHIVE}\$" checksums.txt | sha256sum -c --status - ) \
    || die "the checksum does not match; the download is not what the release published"
  log "checksum verified"
else
  warn "the release publishes no checksums; continuing unverified"
fi

tar -xzf "${WORK}/${ARCHIVE}" -C "$WORK"
[[ -f "${WORK}/runner-fleet" ]] || die "the archive does not contain the binary"

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------

UPGRADE=false
[[ -x "$INSTALL_BIN" ]] && UPGRADE=true

if [[ -z "$ADDR" && -f "$UNIT" ]]; then
  ADDR=$(sed -n 's/.*--addr \([^ ]*\).*/\1/p' "$UNIT" | head -1)
  [[ -n "$ADDR" ]] && log "keeping the address this host already uses: ${ADDR}"
fi
ADDR=${ADDR:-127.0.0.1:8080}

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
  log "created the ${SERVICE_USER} user"
fi

# The runners need /dev/kvm, and its group varies by distribution, so it is
# read from the device rather than assumed.
if [[ -e /dev/kvm ]]; then
  KVM_GROUP=$(stat -c %G /dev/kvm 2>/dev/null || true)
  if [[ -n "$KVM_GROUP" && "$KVM_GROUP" != "UNKNOWN" ]]; then
    usermod -aG "$KVM_GROUP" "$SERVICE_USER" 2>/dev/null \
      || warn "could not add ${SERVICE_USER} to ${KVM_GROUP}; VM runners will not start"
  fi
else
  warn "/dev/kvm is missing, so this host cannot run VM pools (container pools are unaffected)"
fi

install -d -m 0700 "$ETC_DIR" /var/lib/runner-fleet
chown -R "${SERVICE_USER}:${SERVICE_USER}" /var/lib/runner-fleet

# Written beside the destination and renamed into place: the destination may be
# the binary currently running, and a rename leaves the running process on the
# inode it started from — which is exactly why an upgrade does not disturb the
# runners already going.
install -m 0755 "${WORK}/runner-fleet" "${INSTALL_BIN}.new"
mv -f "${INSTALL_BIN}.new" "$INSTALL_BIN"
log "installed ${INSTALL_BIN} (${VERSION})"

if [[ -f "${WORK}/packaging/runner-fleetd.service" ]]; then
  sed "s#--addr 127.0.0.1:8080#--addr ${ADDR}#" "${WORK}/packaging/runner-fleetd.service" > "$UNIT"
else
  cat > "$UNIT" <<UNITFILE
[Unit]
Description=runner-fleet: self-hosted GitHub Actions runners
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=${INSTALL_BIN} serve --addr ${ADDR}
Restart=always
RestartSec=5
KillMode=control-group
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
UNITFILE
fi
chmod 0644 "$UNIT"
systemctl daemon-reload

# ---------------------------------------------------------------------------
# The web UI's password
# ---------------------------------------------------------------------------

# Only asked for on a first install: an upgrade must not stop to ask anything.
#
# Nothing here may read standard input. This script is meant to be piped into
# bash, and when it is, bash reads it from standard input as it goes — so a
# command that consumes stdin eats the rest of the script, and bash then
# reports a syntax error in the middle of nowhere. An earlier version probed
# for an existing password by running "passwd" with an empty one, which reads
# the password from stdin, which ate everything below this line.
NEEDS_PASSWORD=true
if [[ "$UPGRADE" == "true" && -f "${ETC_DIR}/fleet.db" ]]; then
  NEEDS_PASSWORD=false
fi

if [[ -n "$FLEET_PASSWORD" ]]; then
  "$INSTALL_BIN" passwd --user "${FLEET_USER:-admin}" --password "$FLEET_PASSWORD" < /dev/null
elif [[ "$NEEDS_PASSWORD" == "true" ]]; then
  # The daemon serves nothing until a password is set, so this is not optional
  # — but it can be done later by hand if there is no terminal here.
  if [[ -t 0 ]] || [[ -r /dev/tty ]]; then
    echo
    echo "The web UI is protected by a user name and a password."
    echo "It listens on ${ADDR} and nothing else on this host should reach it."
    echo
    read -r -p "User name [admin]: " USER_INPUT < /dev/tty || USER_INPUT=""
    USER_INPUT=${USER_INPUT:-admin}
    while :; do
      read -r -s -p "Password (at least 8 characters): " PASS_INPUT < /dev/tty; echo
      read -r -s -p "Again: " PASS_CONFIRM < /dev/tty; echo
      if [[ "$PASS_INPUT" != "$PASS_CONFIRM" ]]; then
        echo "They do not match."
        continue
      fi
      if [[ ${#PASS_INPUT} -lt 8 ]]; then
        echo "Too short."
        continue
      fi
      break
    done
    # Through stdin, so it is never in the process list nor in the history.
    printf '%s\n' "$PASS_INPUT" | "$INSTALL_BIN" passwd --user "$USER_INPUT"
  else
    warn "no terminal to ask for a password on. Set one before the UI will let anyone in:"
    warn "  sudo ${INSTALL_BIN} passwd --user admin"
  fi
fi

# ---------------------------------------------------------------------------
# Start
# ---------------------------------------------------------------------------

systemctl enable runner-fleetd >/dev/null 2>&1 < /dev/null || true
if [[ "$UPGRADE" == "true" ]]; then
  # Restarting the daemon does not touch the runners: they are units and
  # containers of their own, and this is the whole reason for that design.
  systemctl restart runner-fleetd
  log "upgraded and restarted; the runners were not disturbed"
else
  systemctl start runner-fleetd
fi

sleep 1
if ! systemctl is-active --quiet runner-fleetd; then
  die "the daemon did not start; see: journalctl -u runner-fleetd -n 50"
fi

cat <<DONE

runner-fleet ${VERSION} is running.

  UI       http://${ADDR}
  Logs     journalctl -u runner-fleetd -f
  Upgrade  rerun this script; the runners keep going

If the UI is on a remote host, reach it over an ssh tunnel rather than opening
the port: it holds a credential that administers your repositories.

  ssh -N -L ${ADDR##*:}:${ADDR} $(hostname -s)

DONE
