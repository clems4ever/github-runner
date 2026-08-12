# github-runner

A containerised [self-hosted GitHub Actions runner](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners).

It follows the download / configure / run procedure GitHub shows on the
*Settings → Actions → Runners → New self-hosted runner* page, with one change:
instead of pasting the short-lived registration token from that page, the
container mints a fresh one from a personal access token every time it starts.
That token is only valid for an hour, so a container using it could never
restart on its own.

The image is built for `linux/amd64` and `linux/arm64` and published to
`ghcr.io/clems4ever/github-runner`.

## Quick start

```bash
cp .env.example .env
$EDITOR .env          # set GITHUB_URL and ACCESS_TOKEN
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
own hostname.

Stop and de-register:

```bash
docker compose down
```

## Configuration

All settings are read from `.env` (see `.env.example`).

| Variable | Default | Description |
| --- | --- | --- |
| `GITHUB_URL` | — | Repository or organisation URL, e.g. `https://github.com/clems4ever/runyard` |
| `ACCESS_TOKEN` | — | PAT used to mint registration tokens. Classic PAT: `repo` scope for a repository runner, `admin:org` for an organisation runner. Fine-grained PAT: *Administration* read & write |
| `RUNNER_TOKEN` | — | Alternative to `ACCESS_TOKEN`: a registration token from the runner page. Expires after one hour |
| `RUNNER_NAME` | container hostname | Name shown in the runners list |
| `RUNNER_LABELS` | — | Extra comma-separated labels |
| `RUNNER_GROUP` | `Default` | Runner group |
| `EPHEMERAL` | `true` | Accept a single job, then exit and let Compose start a fresh registration |
| `DISABLE_UPDATE` | `false` | Prevent the runner from auto-updating itself |
| `GITHUB_API_URL` | `https://api.github.com` | Override for GitHub Enterprise Server |
| `IMAGE_TAG` | `latest` | Tag of `ghcr.io/clems4ever/github-runner` to run |
| `INSTALL_DOCKER_CLI` | `true` | Build arg (local builds): install the Docker CLI and compose plugin |

### Ephemeral runners

With `EPHEMERAL=true` (the default) the runner takes exactly one job and exits;
`restart: unless-stopped` then brings the container back with a clean
workspace. This is the recommended setup — a long-lived runner accumulates
state from every job it has run, and workflows can see each other's leftovers.

### Docker inside jobs

The published image ships the Docker CLI but no daemon. To let jobs use the
host's daemon, uncomment the `docker.sock` mount and `group_add` entries in
`docker-compose.yml` and set `DOCKER_GID` to the host's docker group id. Note
that access to the host's Docker socket is equivalent to root on the host.

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
runner. Combine this with `EPHEMERAL=true` and a host you are willing to treat
as untrusted.

## How it works

- `Dockerfile` — Ubuntu 24.04, downloads the runner tarball, verifies its
  SHA-256, extracts it, runs `bin/installdependencies.sh`, and drops to an
  unprivileged `runner` user (the runner refuses to run as root).
- `entrypoint.sh` — requests a registration token from the GitHub API, runs
  `config.sh --unattended --replace`, starts `run.sh`, and on `SIGTERM` stops
  the runner and calls `config.sh remove` so no offline runner is left behind.
