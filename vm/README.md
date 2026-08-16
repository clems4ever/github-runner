# Runner in a VM

`runner-vm.sh` runs the same runner inside a throwaway QEMU virtual machine. It
is a wrapper, not a second implementation: the VM does what
[the `Dockerfile`](../docker/Dockerfile) does — download the runner package, verify it, extract it — and then runs
[`entrypoint.sh`](../entrypoint.sh) exactly as the container does.

The VM belongs to the process that started it. Stop the script and the machine
is powered off and its disk deleted, so nothing a job did survives into the
next one.

What a job gets that the container cannot give it safely:

| | container | VM |
| --- | --- | --- |
| Docker | the host's daemon through `docker.sock`, which is root on the host | a daemon of its own |
| `/dev/kvm` | the host's, shared | the VM's own, through nested virtualisation |
| kernel | the host's | its own, free to break |
| cleanup | whatever the job left in the volume | the disk is deleted |

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/clems4ever/github-runner/main/vm/runner-vm.sh \
  | sudo bash -s -- install
```

That puts `runner-vm.sh` in `/usr/local/bin` and prints how to boot a first
runner. It touches nothing else — no user, no unit, no configuration — so it is
safe to run on a host before deciding anything.

Installing from a branch means telling it where it came from, or it would fetch
`main` instead of the version being piped:

```bash
URL=https://raw.githubusercontent.com/clems4ever/github-runner/feat/qemu-runner-vm/vm/runner-vm.sh
curl -fsSL "$URL" | sudo SCRIPT_URL="$URL" bash -s -- install
```

Or just download it and run it from anywhere — it is a single self-contained
script with no repository around it:

```bash
curl -fsSLO https://raw.githubusercontent.com/clems4ever/github-runner/main/vm/runner-vm.sh
chmod +x runner-vm.sh
```

## Requirements

```bash
sudo apt-get install -y qemu-system-x86 qemu-utils cloud-image-utils
sudo runner-vm.sh doctor
```

`doctor` checks KVM, nested virtualisation and the tools, and prints the fix
for anything missing. Nested virtualisation is the one that usually needs
turning on:

```bash
sudo modprobe -r kvm_amd && sudo modprobe kvm_amd nested=1   # kvm_intel on Intel
echo 'options kvm_amd nested=1' | sudo tee /etc/modprobe.d/kvm_amd.conf
```

## Use

```bash
./runner-vm.sh --url https://github.com/OWNER/REPO --token AAAA...
```

The token is the one from *Settings → Actions → Runners → New self-hosted
runner*, the same as `.env`. An existing `.env` from the container setup is
read automatically, so `./runner-vm.sh` on its own also works.

## Tokens, and why a reboot needs no human

There are two different tokens, and it is worth keeping them apart:

- a **registration token** is what a runner registers with. It expires an hour
  after GitHub issues it, and a VM keeps nothing, so a new one is needed on
  every boot.
- a **long-lived credential** is what mints those. It is configured once.

Pasting a registration token with `--token` therefore works exactly once, for
an hour. Give the script a credential instead and it mints a registration token
per boot, so restarts — including a host reboot at 4am — need nobody:

```bash
GITHUB_TOKEN=ghp_... ./runner-vm.sh --url https://github.com/OWNER/REPO
```

The PAT needs the `repo` scope for a repository runner, or `admin:org` for an
organisation one. A fine-grained token wants **Administration: Read and write**
on the repository, or the organisation's **Self-hosted runners: Read and
write**.

Every setting is read from the environment, so nothing sensitive has to be
edited into a file or typed on a command line — where `ps` would show it to
every user on the machine, and the shell would keep it in the history:

```bash
read -rs GITHUB_TOKEN && export GITHUB_TOKEN     # typed, not echoed, not recorded
./runner-vm.sh --url https://github.com/OWNER/REPO
```

Or keep it in a file and never handle it again:

```bash
sudoedit /etc/runner-vm/pat                       # paste, save
./runner-vm.sh --url https://github.com/OWNER/REPO --github-token-file /etc/runner-vm/pat
pass show github/pat | ./runner-vm.sh --url ... --github-token-file -   # or from stdin
```

**A GitHub App is the better credential for a server.** A PAT belongs to a
person: it expires — a fine-grained one after a year at most — and it stops
working when they leave the organisation, which surfaces as runners silently
failing to come back after a reboot months later. An app belongs to the
organisation instead, and its private key does not expire:

```bash
./runner-vm.sh --url https://github.com/OWNER \
  --app-id 123456 --app-key /etc/runner-vm/app.pem
```

Create it under *Organisation settings → Developer settings → GitHub Apps*,
give it the **Self-hosted runners: Read and write** organisation permission (or
**Administration: Read and write** for a repository runner), install it on the
organisation, and download a private key. The installation is discovered
automatically; `GITHUB_APP_INSTALLATION_ID` overrides that if the app is
installed in more than one place.

Either way the credential stays on the host. What reaches the VM is only the
registration token minted from it, which is good for an hour and nothing else.

The first run builds a golden image — a cloud image with Docker, QEMU and the
runner already installed — which takes a few minutes. Later runs boot a
copy-on-write overlay on top of it in seconds. Rebuild it after bumping
`RUNNER_VERSION` with `./runner-vm.sh build --force`.

Run several by starting the script several times; each VM gets its own name,
disk and ssh port.

## Flags

Every flag has an environment variable equivalent, so `.env` keeps working.

| Flag | Variable | Default | Description |
| --- | --- | --- | --- |
| `--url` | `GITHUB_URL` | — | Repository or organisation URL |
| `--token` | `RUNNER_TOKEN` | — | Registration token |
| `--github-token` | `GITHUB_TOKEN` | — | PAT, to mint a registration token per boot |
| `--name` | `RUNNER_NAME` | `vm-<host>-<pid>` | Runner name |
| `--labels` | `RUNNER_LABELS` | — | Extra comma-separated labels |
| `--group` | `RUNNER_GROUP` | `Default` | Runner group |
| `--cpus` | `VM_CPUS` | `2` | vCPUs |
| `--memory` | `VM_MEMORY_MB` | `4096` | Memory in MiB |
| `--disk` | `VM_DISK_GB` | `40` | Disk in GiB |
| `--ephemeral` | `EPHEMERAL` | `false` | Take one job, then stop — the VM powers off and is deleted |
| `--no-nested` | `VM_NESTED` | nested on | Do not expose `vmx`/`svm` to the VM |
| `--env-file` | | `.env` | Read settings from this file when it exists |

`RUNNER_VM_HOME` moves the cached images and VM disks, which default to
`~/.local/share/runner-vm`.

## Verifying it from a workflow

```yaml
jobs:
  build:
    runs-on: self-hosted
    steps:
      - run: docker run --rm hello-world     # the VM's own daemon
      - run: |
          sudo apt-get update && sudo apt-get install -y cpu-checker
          kvm-ok                             # nested virtualisation
```

## How it works

- `runner-vm.sh build` boots the Ubuntu cloud image once with a cloud-init
  seed that installs Docker, QEMU and the runner package, then powers off. The
  result is the golden image.
- `runner-vm.sh run` creates a qcow2 overlay on that image and a second seed
  carrying the registration details and `entrypoint.sh`, and boots it with
  `-cpu host,+svm` (or `+vmx`) so the guest can run VMs of its own.
- Inside, a systemd unit runs `entrypoint.sh` as the unprivileged `runner`
  user, with its output on the serial console — which is what the script
  streams to the terminal.
- When the runner stops, the unit powers the machine off; when the machine is
  gone, the script deletes the disk. Interrupting the script does the same
  thing from the other direction: it asks the guest to power off, waits, and
  then deletes the disk.

## Ephemeral runners, without the token churn

`--ephemeral` is far more useful here than it is for the container. The runner
takes one job and stops, the VM powers off, and the script deletes it — a
genuinely clean machine per job rather than a clean workspace. Combined with
`--github-token` there is no token to paste, so a loop is enough to keep a
clean runner available:

```bash
while :; do GITHUB_TOKEN=ghp_... ./runner-vm.sh --url https://github.com/OWNER/REPO --ephemeral; done
```

## Starting with the host, under systemd

`install` puts the script, a systemd template unit and an unprivileged service
user in place, so runners come back after a reboot:

```bash
read -rs PAT                                  # typed, not echoed, not recorded
printf '%s' "$PAT" | sudo tee /root/pat >/dev/null && sudo chmod 0600 /root/pat

sudo ./runner-vm.sh install \
  --url https://github.com/OWNER/REPO \
  --github-token-file /root/pat
```

That writes `/usr/local/bin/runner-vm.sh`, `/etc/runner-vm/{env,pat}` (both
`0600`), `/etc/systemd/system/runner-vm@.service`, and creates the `runner-vm`
user in the `kvm` group. Then:

```bash
sudo -u runner-vm RUNNER_VM_HOME=/var/lib/runner-vm /usr/local/bin/runner-vm.sh build
sudo systemctl enable --now runner-vm@runner-1
```

Build the image first, as above, or the service's first start spends a few
minutes doing it and looks hung. More runners are more instances:
`runner-vm@runner-2`, and so on — each gets its own name, disk and ssh port.

Managing them afterwards:

```bash
runner-vm.sh list             # VMs on this host, their state, ssh port and services
sudo runner-vm.sh clean       # stop the services and VMs, keep the image cache
sudo runner-vm.sh uninstall   # remove all of it, including the credentials
runner-vm.sh print-unit       # the unit install writes, to review or adapt
```

The unit is generated by the script rather than shipped as a file, so a single
copied `runner-vm.sh` can install itself and the two cannot drift apart.
`install` wires the credential up according to what it finds: given a token it
writes `/etc/runner-vm/pat` and adds the matching `LoadCredential`, so systemd
hands the service a private copy and the file on disk stays root-owned.

Two settings in the unit are worth knowing about:

- `KillMode=mixed`. QEMU daemonises itself but stays in the service's cgroup,
  so the default would signal it directly — the equivalent of pulling the VM's
  power cord. `mixed` sends `SIGTERM` to the script alone, which then asks the
  guest to shut down.
- `SHUTDOWN_TIMEOUT=3600` with `TimeoutStopSec=3660`. Stopping is deliberately
  not instant: the runner inside the VM finishes the job it is on first. The
  two have to stay in step.

`Restart=always` also gives ephemeral runners for free: the VM powers off after
its job, systemd starts a new one. Set `EPHEMERAL=true` in
`/etc/runner-vm/env` and drop the `while` loop above.

**A minting credential is required here** — a PAT or an app, not a pasted
token: a VM keeps no registration, so every start registers anew, and a pasted
registration token expires an hour after it is issued. The unit refuses to
start without one rather than boot a VM that cannot register.

## Caveats

- Boot costs a few seconds and the VM holds its memory for as long as it runs,
  so this is heavier than a container. That is the trade for the isolation.
- Nested virtualisation only accelerates guests of the host's architecture, and
  an L2 guest is slower than an L1 one.
- With `--github-token` the runner is deleted from *Settings → Actions →
  Runners* on the way out. Without one there is no credential to do that with,
  so the entry stays there as offline until a runner of the same name takes it
  over, exactly as it does for the container.
