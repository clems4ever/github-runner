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

runcmd:
  - [ /usr/local/bin/provision.sh ]
`, publicKey, packages.String(), indent(provisionScript(), "      "))
}

// provisionScript runs inside the build VM. It ends by powering the machine
// off, which is how the builder knows it finished rather than hung.
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
apt-get clean

touch /var/lib/runner-fleet-image-ready
# Powering off is the signal that the build finished.
systemctl poweroff --no-block
`, RunnerVersion)
}

// runUserData is the cloud-init for one runner's machine: the registration
// details, a script that registers and runs the runner, and a unit to keep it
// under systemd's eye inside the guest.
func runUserData(c Config, registrationToken, publicKey string) string {
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
  # 0600 and root-owned. The registration token is short-lived, but a job has
  # no reason to be able to read it.
  - path: /etc/runner-fleet/runner.env
    permissions: '0600'
    owner: 'root:root'
    content: |
      GITHUB_URL=%s
      RUNNER_TOKEN=%s
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
		quote(c.URL), quote(registrationToken), quote(c.Runner),
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
: "${RUNNER_TOKEN:?RUNNER_TOKEN is required}"
RUNNER_NAME=${RUNNER_NAME:-$(hostname)}
RUNNER_GROUP=${RUNNER_GROUP:-Default}
EPHEMERAL=${EPHEMERAL:-false}

cd /home/runner/actions-runner

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
