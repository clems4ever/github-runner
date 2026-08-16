# github-runner

A containerised [self-hosted GitHub Actions runner](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners).

It follows the download / configure / run procedure GitHub shows on the
*Settings → Actions → Runners → New self-hosted runner* page: you paste the
registration token from that page into `.env` and nothing else. No personal
access token is involved.

The registration token expires an hour after GitHub issued it, but it is only
needed to register — from then on the runner authenticates with credentials of
its own. Those are kept in a small `runner-state` volume, so the container can
restart, be re-created, or come back after a reboot without a new token.

The image is built for `linux/amd64` and `linux/arm64` and published to
`ghcr.io/clems4ever/github-runner`.

## Quick start

```bash
cp .env.example .env
$EDITOR .env          # set GITHUB_URL and RUNNER_TOKEN
docker compose up -d
docker compose logs -f
```

The package inherits this repository's visibility, so while the repo is private
you need to authenticate before pulling:

```bash
echo "$GHCR_PAT" | docker login ghcr.io -u <your-username> --password-stdin
```

(a classic PAT with `read:packages`). Making the package public under
*Package settings → Change visibility* removes that step.

To build the image locally instead of pulling it:

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

The runner then shows up under *Settings → Actions → Runners*, and jobs can
target it with:

```yaml
jobs:
  build:
    runs-on: self-hosted
```

To run several runners in parallel:

```bash
docker compose up -d --scale runner=3
```

`RUNNER_NAME` must stay empty for that, so each container registers under its
own hostname. Each replica registers separately, so the token must still be
valid when they start.

Stop the runners:

```bash
docker compose down
```

The registrations survive that (see below); to also throw them away, use
`docker compose down -v` and delete the runners in
*Settings → Actions → Runners*.

## Configuration

All settings are read from `.env` (see `.env.example`).

| Variable | Default | Description |
| --- | --- | --- |
| `GITHUB_URL` | — | Repository or organisation URL, e.g. `https://github.com/clems4ever/runyard` |
| `RUNNER_TOKEN` | — | Registration token from *Settings → Actions → Runners → New self-hosted runner*. Only needed until the runner has registered |
| `RUNNER_NAME` | container hostname | Name shown in the runners list |
| `RUNNER_LABELS` | — | Extra comma-separated labels |
| `RUNNER_GROUP` | `Default` | Runner group |
| `EPHEMERAL` | `false` | Accept a single job, then exit and register again on restart — needs a valid token every time |
| `DISABLE_UPDATE` | `false` | Prevent the runner from auto-updating itself |
| `RUNNER_STATE_DIR` | `/home/runner/.runner-state` | Where the registration is stored inside the container |
| `IMAGE_TAG` | `latest` | Tag of `ghcr.io/clems4ever/github-runner` to run |
| `INSTALL_DOCKER_CLI` | `true` | Build arg (local builds): install the Docker CLI and compose plugin |

### Registration and restarts

`entrypoint.sh` registers the runner the first time it starts and copies the
files `config.sh` produced (`.runner`, `.credentials`, `.credentials_rsakey`)
into the `runner-state` volume, under a directory named after the runner. Every
later start restores them and skips registration, so `RUNNER_TOKEN` can be
emptied out of `.env` once the first start has succeeded.

Nothing de-registers the runner on shutdown — that would need a *removal* token,
which is exactly the kind of credential this setup avoids. A stopped runner
therefore shows up as offline in *Settings → Actions → Runners* until it comes
back; `config.sh --replace` makes a re-registration under the same name take
over the existing entry rather than pile up duplicates.

To move a runner to another repository or change its labels, register again:
`docker compose down -v`, put a fresh token in `.env`, `docker compose up -d`.

The runner is unprivileged (uid 1001), and the image ships an empty
`/home/runner/.runner-state` so that a named volume inherits its ownership.
Replacing that volume with a bind mount means `chown 1001:1001` on the host
directory; the entrypoint checks it can write there before it registers, so a
bad mount fails immediately instead of after burning a token.

### Ephemeral runners

With `EPHEMERAL=true` the runner takes exactly one job and exits;
`restart: unless-stopped` then brings the container back with a clean
workspace. That is nicer isolation — a long-lived runner accumulates state from
every job it has run, and workflows can see each other's leftovers — but GitHub
drops an ephemeral registration as soon as its job ends, so the container has to
register again on every restart. Without a PAT to mint tokens, that only works
while the pasted token is valid, i.e. for an hour. Hence the default of
`EPHEMERAL=false`; turn it on if you are happy to paste a token per hour of
jobs, or clean the workspace from the workflow itself.

### Docker inside jobs

The published image ships the Docker CLI but no daemon. To let jobs use the
host's daemon, uncomment the `docker.sock` mount and `group_add` entries in
`docker-compose.yml` and set `DOCKER_GID` to the host's docker group id. Note
that access to the host's Docker socket is equivalent to root on the host.

### VMs inside jobs

Jobs that boot a VM — the Android emulator, QEMU, libvirt, anything wanting
`-accel kvm` — need the host's `/dev/kvm`. Passing that one device through is
enough; the container does not need `privileged: true`.

Check the host can do it at all:

```bash
ls -l /dev/kvm                    # crw-rw---- 1 root kvm ...
stat -c %g /dev/kvm               # gid to put in KVM_GID
```

No such file means the kernel modules are not loaded (`modprobe kvm_intel` /
`kvm_amd`), virtualisation is off in the firmware, or — if the host is itself a
VM — nested virtualisation is not enabled for it.

Then uncomment the `devices` entry and the `KVM_GID` line of `group_add` in
`docker-compose.yml`, set `KVM_GID` in `.env` to the gid printed above, and
recreate the container:

```bash
docker compose up -d --force-recreate
```

The gid matters because `/dev/kvm` is `rw` for its group only, and the runner
is an unprivileged user that is not in the host's `kvm` group by default.
`group_add` takes numeric gids, and the gid of `kvm` varies by distribution and
by install order (often 104 on Debian/Ubuntu, 36 on Fedora), so read it from
the host rather than copying a number.

Verify from a workflow:

```yaml
- run: |
    ls -l /dev/kvm
    sudo apt-get update && sudo apt-get install -y cpu-checker
    kvm-ok
```

The image ships no QEMU, libvirt or emulator packages: jobs install what they
need (the runner user has passwordless `sudo`), or an action such as
[`reactivecircus/android-emulator-runner`](https://github.com/ReactiveCircus/android-emulator-runner)
brings its own.

KVM only accelerates guests of the host's own architecture — an arm64 host
runs arm64 guests at native speed and x86 guests through slow emulation.

Access to `/dev/kvm` is far narrower than the Docker socket: it does not grant
root on the host. It is still a kernel interface reachable by whatever a
workflow runs, and a job can exhaust the host's memory and CPU with it, so keep
it to repositories you trust.

### Upgrading the runner

Bump `ARG RUNNER_VERSION` and the checksums in the `Dockerfile`
(`RUNNER_SHA256_AMD64` / `RUNNER_SHA256_ARM64`, published in the
[release notes](https://github.com/actions/runner/releases)) and push to
`main`. The workflow republishes `latest` and tags the image with the new
runner version; `docker compose pull && docker compose up -d` picks it up.

## Running in a VM instead of a container

`runner-vm.sh` runs the same runner inside a throwaway QEMU virtual machine. It
is a wrapper, not a second implementation: the VM does what the `Dockerfile`
does — download the runner package, verify it, extract it — and then runs
`entrypoint.sh` exactly as the container does.

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

### Requirements

```bash
sudo apt-get install -y qemu-system-x86 qemu-utils cloud-image-utils
./runner-vm.sh doctor
```

`doctor` checks KVM, nested virtualisation and the tools, and prints the fix
for anything missing. Nested virtualisation is the one that usually needs
turning on:

```bash
sudo modprobe -r kvm_amd && sudo modprobe kvm_amd nested=1   # kvm_intel on Intel
echo 'options kvm_amd nested=1' | sudo tee /etc/modprobe.d/kvm_amd.conf
```

### Use

```bash
./runner-vm.sh --url https://github.com/OWNER/REPO --token AAAA...
```

The token is the one from *Settings → Actions → Runners → New self-hosted
runner*, the same as `.env`. An existing `.env` from the container setup is
read automatically, so `./runner-vm.sh` on its own also works.

### Tokens, and why a reboot needs no human

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

### Flags

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

### Verifying it from a workflow

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

### How it works

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

### Ephemeral runners, without the token churn

`--ephemeral` is far more useful here than it is for the container. The runner
takes one job and stops, the VM powers off, and the script deletes it — a
genuinely clean machine per job rather than a clean workspace. Combined with
`--github-token` there is no token to paste, so a loop is enough to keep a
clean runner available:

```bash
while :; do GITHUB_TOKEN=ghp_... ./runner-vm.sh --url https://github.com/OWNER/REPO --ephemeral; done
```

### Starting with the host, under systemd

`systemd/runner-vm@.service` is a template unit, so the instance name is the
runner name:

```bash
sudo install -m 0755 runner-vm.sh /usr/local/bin/runner-vm.sh
sudo install -m 0644 systemd/runner-vm@.service /etc/systemd/system/
sudo useradd --system --create-home --home-dir /var/lib/runner-vm runner-vm
sudo usermod -aG kvm runner-vm

sudo install -d -m 0755 /etc/runner-vm
printf 'GITHUB_URL=https://github.com/OWNER/REPO\nGITHUB_TOKEN=ghp_...\n' \
  | sudo tee /etc/runner-vm/env >/dev/null
sudo chmod 0600 /etc/runner-vm/env

sudo -u runner-vm RUNNER_VM_HOME=/var/lib/runner-vm /usr/local/bin/runner-vm.sh build
sudo systemctl daemon-reload
sudo systemctl enable --now runner-vm@build1
```

Build the golden image first, as above, or the service's first start spends a
few minutes doing it. Run more runners by enabling more instances:
`runner-vm@build2`, and so on.

**A minting credential is required here** — a PAT or an app, not a pasted
token, for the reasons above. The unit refuses to start without one rather than
boot a VM that cannot register. systemd reads `/etc/runner-vm/env` as root
before dropping to the service user, so it stays `0600` root-owned and nothing
a job runs can read it; the VM only ever receives the hour-long registration
token minted from it.

To keep the credential out of the environment file as well, put it in a file of
its own and uncomment the matching `LoadCredential` pair in the unit. systemd
hands the service a private copy, so the file on disk stays `0600` root-owned
and the service user never reads it directly:

```
LoadCredential=pat:/etc/runner-vm/pat
Environment=GITHUB_TOKEN_FILE=%d/pat
```

and for a GitHub App, with `GITHUB_APP_ID=123456` in `/etc/runner-vm/env`:

```
LoadCredential=app.pem:/etc/runner-vm/app.pem
Environment=GITHUB_APP_PRIVATE_KEY=%d/app.pem
```

Two settings in the unit are worth knowing about:

- `KillMode=mixed`. QEMU daemonises itself but stays in the service's cgroup,
  so the default would signal it directly — the equivalent of pulling the VM's
  power cord. `mixed` sends `SIGTERM` to the script alone, which then asks the
  guest to shut down.
- `SHUTDOWN_TIMEOUT=3600` with `TimeoutStopSec=3660`. Stopping is deliberately
  not instant: the runner inside the VM finishes the job it is on first. The
  two have to stay in step — the first is how long the script waits before
  killing QEMU, the second how long systemd waits before killing the script.

`Restart=always` also gives ephemeral runners for free: the VM powers off after
its job, systemd starts a new one. Set `EPHEMERAL=true` in
`/etc/runner-vm/env` and drop the `while` loop above.

### Caveats

- Boot costs a few seconds and the VM holds its memory for as long as it runs,
  so this is heavier than a container. That is the trade for the isolation.
- Nested virtualisation only accelerates guests of the host's architecture, and
  an L2 guest is slower than an L1 one.
- With `--github-token` the runner is deleted from *Settings → Actions →
  Runners* on the way out. Without one there is no credential to do that with,
  so the entry stays there as offline until a runner of the same name takes it
  over, exactly as it does for the container.

## Publishing

`.github/workflows/build.yml` builds the image with buildx (QEMU for arm64) and
pushes to GHCR on every push to `main`, using the workflow's built-in
`GITHUB_TOKEN` — no secret to configure. Pull requests build without pushing.

Tags produced:

| Tag | When |
| --- | --- |
| `latest` | push to `main` |
| runner version, e.g. `2.336.0` | push to `main`, read from the `Dockerfile` |
| `sha-<commit>` | every push |
| `vX.Y.Z` | pushing a `v*` git tag |

## Security note

GitHub recommends self-hosted runners only for **private** repositories: on a
public repository, a pull request from a fork can run arbitrary code on the
runner. Treat the host as untrusted, and prefer `EPHEMERAL=true` where the
token churn it implies is acceptable.

The only credential in `.env` is a registration token: it can enrol a runner
for an hour and does nothing else, so a leak is far less damaging than that of
a PAT with repository or organisation administration rights.

## How it works

- `Dockerfile` — Ubuntu 24.04, downloads the runner tarball, verifies its
  SHA-256, extracts it, runs `bin/installdependencies.sh`, and drops to an
  unprivileged `runner` user (the runner refuses to run as root).
- `entrypoint.sh` — restores a saved registration if there is one, otherwise
  runs `config.sh --unattended --replace` with `RUNNER_TOKEN` and saves what it
  produced, then starts `run.sh` and stops it on `SIGTERM`.
