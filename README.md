# github-runner

A containerised [self-hosted GitHub Actions runner](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners).

It follows the download / configure / run procedure GitHub shows on the
*Settings → Actions → Runners → New self-hosted runner* page, with one change:
instead of pasting the short-lived registration token from that page, the
container mints a fresh one from a personal access token every time it starts.
That token is only valid for an hour, so a container using it could never
restart on its own.

## Quick start

```bash
cp .env.example .env
$EDITOR .env          # set GITHUB_URL and ACCESS_TOKEN
docker compose up -d --build
docker compose logs -f
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
| `RUNNER_VERSION` | `2.336.0` | Build arg: runner release to install |
| `RUNNER_ARCH` | `x64` | Build arg: `x64` or `arm64` |
| `INSTALL_DOCKER_CLI` | `false` | Build arg: also install the Docker CLI and compose plugin |

### Ephemeral runners

With `EPHEMERAL=true` (the default) the runner takes exactly one job and exits;
`restart: unless-stopped` then brings the container back with a clean
workspace. This is the recommended setup — a long-lived runner accumulates
state from every job it has run, and workflows can see each other's leftovers.

### Docker inside jobs

The image has no Docker daemon. To let jobs use the host's daemon, build with
`INSTALL_DOCKER_CLI=true` and uncomment the `docker.sock` mount and `group_add`
entries in `docker-compose.yml`. Note that access to the host's Docker socket
is equivalent to root on the host.

### Upgrading the runner

Bump `RUNNER_VERSION` and the checksums in the `Dockerfile` (`RUNNER_SHA256_X64`
/ `RUNNER_SHA256_ARM64`, published in the
[release notes](https://github.com/actions/runner/releases)), then
`docker compose up -d --build`.

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
