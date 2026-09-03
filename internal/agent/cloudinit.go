package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// UbuntuRelease is the guest the runners boot. It matches what GitHub's own
// hosted runners are on, which is what workflows are written against.
const UbuntuRelease = "noble"

// RunnerVersion is the actions runner baked into the golden image.
//
// This is not a preference, it is an expiry date. GitHub deprecates old runner
// versions server-side, and a deprecated runner is not merely warned at: it
// registers, connects, is told "this version cannot receive messages", and
// exits. On an ephemeral machine that means the runner stops, the machine
// powers off, and the host starts another one — two boots a minute, for ever,
// with the fleet showing a healthy runner throughout. That is what 2.330.0 did.
//
// So this is kept current, the runner is allowed to update itself when it is
// not (see GuestRunnerScript), and a weekly workflow opens a pull request when
// a newer one exists. Any one of those three would have been enough; the point
// of having all three is that the failure is silent and the clock is somebody
// else's.
const RunnerVersion = "2.336.0"

// basePackages is what a job can reasonably expect to find: the toolchain that
// workflows written for GitHub-hosted runners assume, plus Docker and QEMU so a
// job can run containers and, where the pool allows it, machines of its own.
var basePackages = []string{
	"apt-transport-https", "build-essential", "ca-certificates", "cloud-guest-utils",
	"cmake", "curl", "docker.io", "docker-compose-v2", "file", "gettext", "gh", "git",
	"git-lfs", "gnupg", "jq", "libbz2-dev", "libffi-dev", "liblzma-dev", "libncurses-dev",
	"libreadline-dev", "libsqlite3-dev", "libssl-dev", "libtool", "libxml2-dev",
	"libxmlsec1-dev", "make", "nodejs", "npm", "openssh-client", "pkg-config", "python3",
	"python3-dev", "python3-pip", "python3-venv", "qemu-system-x86", "qemu-utils",
	"rsync", "shellcheck", "software-properties-common", "sudo", "tar", "tk-dev", "unzip",
	"wget", "xz-utils", "zip", "zlib1g-dev",
}

// ImageSpec describes a golden image. Two pools wanting the same thing share
// one image; a pool that wants more gets its own, which is how per-repository
// images will work once pools can name their own package list.
type ImageSpec struct {
	Variant  string // the pool's image field: "default", or a name of its own
	Packages []string
	// Recipe is the pool's own provisioning, run as root in the build machine
	// after the packages are in and the base provisioning has finished. It is
	// for what apt cannot give: a toolchain at a version no archive carries, a
	// pinned linter, a warm build cache.
	Recipe string
}

// provision is the script an image is built with, as a variable so that a test
// can prove the image's name depends on it.
var provision = provisionScript

// Name is the file name for an image, and is a hash of everything that goes
// into it: two images with the same name are the same image, and changing any
// part of how one is built produces a new name rather than silently reusing the
// old image.
//
// "Everything" has to include the script, and once did not. The list of
// packages and the runner version were hashed and the provisioning was not, so
// a release that changed what the build *does* — giving the job passwordless
// sudo — produced the same name as the release before it, found that image
// already on disk, and reused it. The change shipped, was installed, and did
// nothing at all; the only symptom was jobs still failing on the thing that had
// just been fixed.
func (s ImageSpec) Name() string {
	h := sha256.New()
	h.Write([]byte(UbuntuRelease))
	h.Write([]byte(RunnerVersion))
	h.Write([]byte(provision()))
	h.Write([]byte(buildScript()))
	// The pool's own provisioning is part of the image's identity for exactly
	// the reason the script above is: editing it has to produce a different
	// image, or the edit ships and the host goes on booting the old one.
	h.Write([]byte(s.Recipe))
	for _, pkg := range s.EffectivePackages() {
		h.Write([]byte(pkg))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("runner-%s-%s-%s.qcow2", UbuntuRelease, s.Variant, hex.EncodeToString(h.Sum(nil))[:12])
}

// EffectivePackages is the sorted, deduplicated package list.
func (s ImageSpec) EffectivePackages() []string {
	seen := map[string]bool{}
	var out []string
	for _, pkg := range append(append([]string{}, basePackages...), s.Packages...) {
		if pkg == "" || seen[pkg] {
			continue
		}
		seen[pkg] = true
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

// Path is where the image lives.
func (s ImageSpec) Path(imagesDir string) string { return filepath.Join(imagesDir, s.Name()) }

// buildUserData is the cloud-init that turns an Ubuntu cloud image into the
// golden image: install everything, install the runner, then power off.
func buildUserData(spec ImageSpec, publicKey string) string {
	var packages strings.Builder
	for _, pkg := range spec.EffectivePackages() {
		fmt.Fprintf(&packages, "  - %s\n", pkg)
	}

	return fmt.Sprintf(`#cloud-config
hostname: runner-fleet-build

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s

package_update: true
package_upgrade: true
packages:
%s
write_files:
  - path: /usr/local/bin/provision.sh
    permissions: '0755'
    owner: 'root:root'
    content: |
%s
%s
  - path: /usr/local/bin/build.sh
    permissions: '0755'
    owner: 'root:root'
    content: |
%s

runcmd:
  - [ /usr/local/bin/build.sh ]
`, publicKey, packages.String(), indent(provisionScript(), "      "),
		recipeFile(spec.Recipe), indent(buildScript(), "      "))
}

// recipeFile is the pool's own provisioning as a write_files entry, or nothing
// at all when the pool has none — which is the case that has to keep producing
// exactly the document it produced before this existed.
func recipeFile(recipe string) string {
	if recipe == "" {
		return ""
	}
	return fmt.Sprintf(`
  - path: %s
    permissions: '0755'
    owner: 'root:root'
    content: |
%s
`, recipePath, indent(recipe, "      "))
}

// recipePath is where a pool's recipe lands inside the build machine.
const recipePath = "/usr/local/bin/recipe.sh"

// Console markers. The host cannot look inside the disk a build is writing —
// it is a qcow2 nobody has mounted — so the serial console is the whole of
// what it knows. A build that does not say it finished did not finish.
const (
	ImageReadyMarker  = "runner-fleet: image ready"
	ImageFailedMarker = "runner-fleet: image build failed"
)

// buildScript is what cloud-init actually runs: the base provisioning, then
// the pool's recipe if it has one, and a power-off either way.
//
// The power-off is in a trap rather than at the end for one reason. Powering
// off is how the host learns a build is over, and before this a script that
// exited non-zero never reached it — the machine sat at a login prompt and the
// host waited on a build that was already dead, for as long as the stale-lock
// timer takes. That was survivable while the script was ours and changed twice
// a year. A recipe is somebody else's shell, edited in a text box.
func buildScript() string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

finish() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo '%[2]s' > /dev/console
    echo "the image build failed with status $status" > /dev/console
  fi
  systemctl poweroff --no-block
}
trap finish EXIT

/usr/local/bin/provision.sh

if [ -x %[3]s ]; then
  echo "running this pool's recipe" > /dev/console
  %[3]s
fi

# Written last and read by nothing inside the machine: it is here so that a
# disk can be told apart from one a half-finished build left behind.
touch /var/lib/runner-fleet-image-ready
echo '%[1]s' > /dev/console
`, ImageReadyMarker, ImageFailedMarker, recipePath)
}

// provisionScript is the base provisioning every image gets, run inside the
// build machine. What comes after it — the pool's own recipe, the readiness
// marker and the power-off — is buildScript's.
func provisionScript() string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

# The runner refuses to run as root, so it gets a user of its own.
id runner >/dev/null 2>&1 || useradd -m -s /bin/bash runner

# And passwordless sudo, because a workflow written for a GitHub-hosted runner
# assumes it. Installing a package, writing outside the workspace, starting a
# service: all of it is "sudo apt-get install ..." in somebody's yaml, and
# without this every one of those jobs fails with "runner is not in the sudoers
# file" — which is not a sentence anyone reads as "your runner is misconfigured".
#
# The machine is the boundary here, not the user. A job already has a kernel of
# its own and a disk that is destroyed afterwards, so root inside it buys an
# attacker nothing they did not already have. That is the whole reason a VM pool
# exists.
echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner
chmod 0440 /etc/sudoers.d/runner
# A malformed sudoers file locks sudo for everybody, so it is checked here where
# the failure stops the image build rather than in every job that needs root.
visudo -c -f /etc/sudoers.d/runner

# Docker for jobs: a daemon inside the machine, so nothing is shared with the
# host the way a mounted socket would be.
usermod -aG docker runner
systemctl enable docker

# /dev/kvm inside the guest is rw for the kvm group only, so a job can only use
# nested virtualisation if the runner is a member. Whether the device is there
# at all is the pool's choice, made on the host.
getent group kvm >/dev/null || groupadd -r kvm
usermod -aG kvm runner

install -d -o runner -g runner /home/runner/actions-runner /home/runner/_work
cd /home/runner/actions-runner

arch=x64
[ "$(uname -m)" = "aarch64" ] && arch=arm64
curl -fsSL -o runner.tar.gz \
  "https://github.com/actions/runner/releases/download/v%[1]s/actions-runner-linux-${arch}-%[1]s.tar.gz"
tar xzf runner.tar.gz
rm -f runner.tar.gz
chown -R runner:runner /home/runner
./bin/installdependencies.sh

# Unattended upgrades would fight the job for the package lock and reboot the
# machine underneath it. The machine is replaced rather than patched.
systemctl disable --now unattended-upgrades.service 2>/dev/null || true

# A machine boots for one job and is destroyed, and every second of that boot is
# paid by every job on the host. These are the services a cloud image starts by
# default that a runner has no use for: hardware managers for hardware that does
# not exist, a snap daemon for packages nobody installs, a boot splash on a
# machine with no screen, and timers whose whole purpose is to do work later on
# a machine that will not exist later.
#
# Disabled rather than masked, so a job that genuinely wants one can start it.
for unit in \
  snapd.service snapd.seeded.service snapd.socket snapd.apparmor.service \
  ModemManager.service udisks2.service multipathd.service multipathd.socket \
  lvm2-monitor.service ubuntu-fan.service plymouth-quit-wait.service \
  apt-daily.timer apt-daily-upgrade.timer motd-news.timer fwupd-refresh.timer \
  e2scrub_all.timer man-db.timer dpkg-db-backup.timer update-notifier-download.timer
do
  systemctl disable "$unit" 2>/dev/null || true
done
apt-get clean
`, RunnerVersion)
}

// Registration is what a machine is given to become a runner, and there are
// two kinds.
//
// A JIT is the whole configuration, minted on the host before the machine
// booted. The guest unpacks it and starts: there is no registration step to
// fail, nothing that could administer a repository is ever inside the guest,
// and a machine that never boots leaves no entry behind. It is what an
// ephemeral pool uses, and it is the only thing GitHub will mint one of — a
// just-in-time runner takes one job and is spent.
//
// A Token is the older two-step: a registration token the guest trades for
// credentials of its own by running config.sh. It is what a pool of long-lived
// runners has to use, because there is no such thing as a long-lived
// just-in-time runner.
type Registration struct {
	JIT   string
	Token string
}

// runUserData is the cloud-init for one runner's machine: what it registers
// with, a script that runs the runner, and a unit to keep it under systemd's
// eye inside the guest.
func runUserData(c Config, reg Registration, publicKey string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
preserve_hostname: false

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s

write_files:
  # 0600 and root-owned. Both kinds of registration are short-lived and worth
  # little once spent, but neither is any of the job's business.
  - path: /etc/runner-fleet/runner.env
    permissions: '0600'
    owner: 'root:root'
    content: |
      GITHUB_URL=%s
      RUNNER_TOKEN=%s
      RUNNER_JITCONFIG=%s
      RUNNER_NAME=%s
      RUNNER_LABELS=%s
      RUNNER_GROUP=%s
      EPHEMERAL=%t

  - path: /usr/local/bin/run-runner.sh
    permissions: '0755'
    owner: 'root:root'
    content: |
%s

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
      EnvironmentFile=/etc/runner-fleet/runner.env
      ExecStart=/usr/local/bin/run-runner.sh
      # The runner has to finish the job it is on before it stops. This is the
      # inside half of the promise the host makes when it drains a runner.
      KillSignal=SIGTERM
      TimeoutStopSec=3h
      Restart=no
      StandardOutput=journal+console
      StandardError=journal+console
      # When the runner stops for any reason the machine has nothing left to
      # do. Powering off is what tells the agent on the host to clean up.
      #
      # The "+" prefix runs this with full privileges: it would otherwise
      # inherit User=runner, and polkit refuses a non-interactive poweroff from
      # an unprivileged user, leaving the machine at a login prompt for ever.
      ExecStopPost=+/usr/bin/systemctl poweroff --no-block

      [Install]
      WantedBy=multi-user.target

runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, start, --no-block, github-runner.service ]
`,
		c.Runner, publicKey,
		quote(c.URL), quote(reg.Token), quote(reg.JIT), quote(c.Runner),
		quote(strings.Join(c.Labels, ",")), quote(c.Group), c.Ephemeral,
		indent(GuestRunnerScript, "      "))
}

// GuestRunnerScript registers the runner inside the guest and runs it. It is
// exported so a test can assert on the arguments it builds, which are the
// difference between an ephemeral runner and a long-lived one.
const GuestRunnerScript = `#!/usr/bin/env bash
set -euo pipefail

# Registers this machine as a self-hosted runner and runs it. The runner
# refuses to run as root, so the unit starts this as "runner".

: "${GITHUB_URL:?GITHUB_URL is required}"
RUNNER_NAME=${RUNNER_NAME:-$(hostname)}
RUNNER_GROUP=${RUNNER_GROUP:-Default}
EPHEMERAL=${EPHEMERAL:-false}

cd /home/runner/actions-runner

# A just-in-time configuration is everything config.sh would have written,
# minted on the host before this machine booted. There is nothing to register:
# unpack it and run. exec, so the runner receives the unit's SIGTERM directly
# and can finish the job it is on before stopping.
if [[ -n "${RUNNER_JITCONFIG:-}" ]]; then
  echo "starting runner '${RUNNER_NAME}' on ${GITHUB_URL} from a just-in-time configuration"
  exec ./run.sh --jitconfig "$RUNNER_JITCONFIG"
fi

# The older path, for a pool of long-lived runners: GitHub only mints a
# just-in-time configuration for a runner that takes one job, so a runner meant
# to outlive its job still has to trade a registration token for credentials.
: "${RUNNER_TOKEN:?either RUNNER_JITCONFIG or RUNNER_TOKEN is required}"

args=(
  --url "$GITHUB_URL"
  --name "$RUNNER_NAME"
  --runnergroup "$RUNNER_GROUP"
  --work /home/runner/_work
  --unattended
  # Updates are deliberately NOT disabled here, unlike in a container.
  #
  # A container's runner is updated by pulling a newer image, and the default
  # tag is a rolling one, so it is never far behind. A machine's runner is baked
  # into a golden image built once on this host, which can be months old — and a
  # runner GitHub considers deprecated cannot receive jobs at all. Letting it
  # update itself costs a download on a stale image and nothing on a fresh one,
  # which is a better trade than a fleet that quietly stops accepting work.
  # Take over an entry of the same name left by an earlier machine: this
  # runner's name is its identity in the fleet, and it is reused on purpose.
  --replace
)
[[ -n "${RUNNER_LABELS:-}" ]] && args+=(--labels "$RUNNER_LABELS")
[[ "$EPHEMERAL" == "true" ]] && args+=(--ephemeral)

echo "registering runner '${RUNNER_NAME}' on ${GITHUB_URL}"
if ! ./config.sh "${args[@]}" --token "$RUNNER_TOKEN"; then
  echo "registration failed: a registration token expires an hour after it is issued" >&2
  exit 1
fi

# exec, so the runner receives the unit's SIGTERM directly and can finish the
# job it is on before stopping.
exec ./run.sh
`

// metaData is the second half of a cloud-init seed.
func metaData(name, instanceID string) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, name)
}

func indent(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// quote renders a value for an environment file the way systemd reads one.
// Double quotes rather than single: systemd processes escapes inside doubles,
// and a token has no business breaking the file.
func quote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", "").Replace(value) + `"`
}
