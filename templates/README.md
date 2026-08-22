# Pool templates

Templates are the portable form of a fleet's pools. Import one from **Pools →
Import** in the UI, or post it:

```bash
curl -fsS -u admin:PASSWORD http://127.0.0.1:8080/api/pools/import \
  -H 'Content-Type: application/json' \
  -d "{\"credentialId\": 1, \"dryRun\": true, \"document\": $(cat templates/github-runner-ci.json)}"
```

`dryRun` reports what would happen and writes nothing — it runs the real import
and rolls it back — so it is worth doing first. Drop it to import for real.

Nothing local to one installation is in a template: no pool ids, no credential,
no timestamps. The credential is chosen when importing, and `scope` in the
request replaces the scope of every pool in the document, so the same file
serves more than one repository.

## github-runner-ci.json

The two pools this repository's own CI needs.

| Pool | Runtime | Jobs | Why |
| --- | --- | --- | --- |
| `ci-container` | container | `go`, `ui` | Nothing outside their own tree: the tests fake systemd and Docker, and the toolchains come from `setup-go` and `setup-node`. |
| `ci-vm` | vm | `container-runner`, `installer` | `container-runner` needs a Docker daemon it may create and destroy containers in. `installer` installs a systemd service as root, binds a port, and deletes what it installed. |

Neither needs nested virtualisation: Docker does not want `/dev/kvm`, and nested
is a hole in the boundary. Both are ephemeral, which matters most for
`installer` — it leaves the host changed on purpose.

After importing, point the jobs at them:

```yaml
jobs:
  go:
    runs-on: [self-hosted, linux, container]
  container-runner:
    runs-on: [self-hosted, linux, vm]
```

`self-hosted`, `linux` and `x64` come from the runner itself; `container` and
`vm` are added by the daemon from the pool's runtime, so a pool cannot claim to
be something it is not.

Two things this template cannot do for you:

- The `installer` job runs `shellcheck`, which is not in the golden image's
  package list. Add `sudo apt-get install -y shellcheck` to the job, or the
  package to `basePackages` in `internal/agent/cloudinit.go`.
- This repository is public, and a self-hosted runner on a public repository
  will run pull-request code from any fork. Gate the workflow on
  `github.event.pull_request.head.repo.full_name == github.repository`, or make
  the repository private, before pointing anything at these pools.

## Writing one

```json
{
  "version": 1,
  "name": "shown in the import preview",
  "description": "what these pools are for",
  "pools": [
    {
      "name": "ci-container",
      "scopeKind": "repository",
      "scope": "owner/repository",
      "runtime": "container",
      "nested": false,
      "ephemeral": true,
      "minReplicas": 1,
      "maxReplicas": 3,
      "labels": ["fast"],
      "cpus": 2,
      "memoryMb": 4096,
      "diskGb": 40,
      "image": "default",
      "enabled": true
    }
  ]
}
```

Only `version`, `pools` and each pool's `name` are required. `scope` may be left
out for a template meant to be reused, and then the import supplies it.
Everything else takes the same default as the editor, except that a pool is
enabled and ephemeral unless it says otherwise — a template that said nothing
and imported a fleet switched off would be a surprise.

`diskGb` applies to machines only; containers have no disk of their own.

Every template in this directory is imported by the test suite
(`internal/template` and `internal/api`), so one that stops working fails the
build.
