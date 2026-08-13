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
