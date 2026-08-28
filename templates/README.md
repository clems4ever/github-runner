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
| `ci-container` | container | `ui` | Node comes from `setup-node`, and nothing leaves the working tree. |
| `ci-vm` | vm | `go`, `container-runner`, `installer` | `go` runs `go test -race`, which needs cgo and so a C compiler — the official runner image has none. `container-runner` needs a Docker daemon it may create and destroy containers in. `installer` installs a systemd service as root, binds a port, and deletes what it installed. |

That the image has no compiler is checked rather than assumed:
`TestTheOfficialImageHasNoCToolchain` in `internal/executor/docker` runs the
real image and looks. If that ever fails because a compiler has appeared, the
`go` job can move to the container pool.

Neither pool needs nested virtualisation: Docker does not want `/dev/kvm`, and
nested is a hole in the boundary. Both are ephemeral, which matters most for
`installer` — it leaves the host changed on purpose.

The maximums add up to three machines and two containers at once — 14 vCPU and
28 GiB if everything runs together. Lower them in the editor if the host is
smaller; a maximum equal to the minimum is a pool that never grows.

The workflow selects them like this, with forks sent to GitHub's runners
because this repository is public:

```yaml
jobs:
  ui:
    runs-on: ${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || fromJSON('["self-hosted", "linux", "container"]') }}
  go:
    runs-on: ${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || fromJSON('["self-hosted", "linux", "vm"]') }}
```

`self-hosted`, `linux` and `x64` come from the runner itself; `container` and
`vm` are added by the daemon from the pool's runtime, so a pool cannot claim to
be something it is not.

One thing to know before importing it: a machine pool builds a golden image the
first time it needs one, and the image is a hash of everything it is built from
— the package list, and now the build script and the pool's recipe too. The list
gained `shellcheck` when the `installer` job moved onto the fleet, so the first
machine after upgrading spends a few minutes building an image before its runner
appears. That happens once.

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
      "packages": ["nftables", "conntrack"],
      "recipe": "#!/usr/bin/env bash\nset -euo pipefail\ncurl -fsSL https://example.invalid/tool | tar -C /usr/local -xz\n",
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

`diskGb` applies to machines only; containers have no disk of their own, and so
do `packages` and `recipe` — a container pool names a prebuilt image instead,
and an import that gives it either is refused rather than quietly dropping them.

`recipe` is a shell script in a JSON string, which means its newlines are `\n`.
That is a poor way to write one and a fine way to move one: author it in the
pool editor, export the template, and the file that comes out is the pool that
went in. It is stored and exported in the clear — a template is something people
paste into issues, so nothing in a recipe should be secret.

Every template in this directory is imported by the test suite
(`internal/template` and `internal/api`), so one that stops working fails the
build.
