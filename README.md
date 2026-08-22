# runner-fleet

[![ci](https://github.com/clems4ever/github-runner/actions/workflows/ci.yml/badge.svg)](https://github.com/clems4ever/github-runner/actions/workflows/ci.yml)

A daemon that maintains a fleet of [self-hosted GitHub Actions
runners](https://docs.github.com/en/actions/hosting-your-own-runners) on one
host, with a web UI to configure it.

Each runner is a throwaway QEMU virtual machine or a Docker container. Per pool
you choose the repository or organisation, the runtime, whether jobs get
`/dev/kvm`, whether runners are ephemeral, what labels they register with, how
big they are — and how far the pool may scale itself when work arrives.

**Upgrading the daemon does not touch the runners.** It supervises nothing: the
runners are systemd units and Docker containers of their own, and the daemon
only creates, drains and deletes them. Restart it, upgrade it, or let it crash —
the jobs on this host carry on.

![The pools table](docs/img/pools.png)

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/clems4ever/github-runner/main/install.sh | sudo bash
```

It fetches the latest release, verifies its checksum, installs the systemd
unit, and asks for the user name and password the web UI will use. Then open
<http://127.0.0.1:8080>.

Rerunning it is how you upgrade, and it is deliberately uneventful.

If something else on the host already has port 8080, pick another — the
installer writes it into the unit, and later upgrades keep it:

```bash
curl -fsSL https://raw.githubusercontent.com/clems4ever/github-runner/main/install.sh \
  | sudo ADDR=127.0.0.1:8181 bash
```

The UI binds to the loopback address. If the host is remote, tunnel to it rather
than opening the port — the daemon holds a credential that administers your
repositories:

```bash
ssh -N -L 8080:127.0.0.1:8080 your-host
```

## Getting started

1. **Add a credential** — a GitHub App, or a personal access token. Either
   needs **Administration: Read and write** on the repositories it covers, or
   **Self-hosted runners: Read and write** on an organisation, and either is
   encrypted at rest and never shown again.
2. **Create a pool.** Pick the scope, the runtime, and the number of replicas.
3. Runners appear within a few seconds, and again after a reboot.

A pool named `web` with a maximum of three gives you `web-1`, `web-2` and
`web-3` when it is busy, and just `web-1` when it is not.

## What a pool decides

| | |
| --- | --- |
| **Scope** | one repository, or an organisation whose repositories all share the runners |
| **Runtime** | a virtual machine per job, or a container |
| **Nested virtualisation** | whether jobs get `/dev/kvm` and can boot machines of their own |
| **Ephemeral** | take one job, then be replaced by a clean runner |
| **Minimum runners** | what the pool keeps up when nothing is running, at least one |
| **Maximum runners** | how far it may grow under load; equal to the minimum for a fixed size |
| **Labels** | what a workflow targets with `runs-on` |
| **Size** | vCPUs, memory, and disk for VM pools |

Every runner also registers with labels describing what it is — `vm` or
`container`, plus `nestedvirt` and `ephemeral` when they apply — so a workflow
can ask for what it needs:

```yaml
jobs:
  build:
    runs-on: [self-hosted, nestedvirt]
```

Those follow the settings rather than the name, so a pool cannot claim to be
something it is not.

The nested virtualisation label used to be `nested`, which said nothing about
what was nested. A workflow still asking for `nested` will queue for ever;
change it to `nestedvirt`. The runners themselves need nothing done to them —
the label is part of what a runner is built from, so each is replaced with a
correctly labelled one as it finishes its current job.

![The pool editor](docs/img/pool-editor.png)

### Templates

Pools can be written down and imported, so a fleet is something a repository
carries rather than something rebuilt by hand on each host.

**Pools → Import**, paste a template or choose the file, pick the credential,
and press **Preview**. The preview is not a guess about what would happen: it
is the import itself, run against the database and rolled back, so a line
saying *new* is a pool that was created a moment ago and then undone. Press
**Import** underneath it and the same thing happens for real.

**Pools → Export** writes the fleet out in the same format, for the next host or
for the repository it serves.

A template carries nothing local to one installation — no pool ids, no
credential, no timestamps. The credential is chosen at import time, and the
scope can be replaced, so one document serves several repositories:

```json
{
  "version": 1,
  "name": "github-runner CI",
  "pools": [
    { "name": "ci-container", "runtime": "container", "minReplicas": 1, "maxReplicas": 3 },
    { "name": "ci-vm", "runtime": "vm", "cpus": 4, "memoryMb": 8192, "diskGb": 40 }
  ]
}
```

Anything left out takes the same default it has in the editor, except that a
pool is enabled and ephemeral unless it says otherwise. Importing over a pool
that already exists has to be asked for, and when it is, the pool keeps its
identity and its runners are replaced gracefully as each finishes its job.

[`templates/`](templates/) holds the ones shipped with the daemon, including the
two pools this repository's own CI needs. They are imported by the test suite,
so one that stops working fails the build rather than an operator's afternoon.

### Autoscaling

Set the minimum to 1 and the maximum to 5, and the pool sits at one idle runner
until work arrives.

**Growing.** GitHub does not publish how many jobs are queued for a set of
labels — only what each runner is doing. So demand is inferred: when *every*
runner in a pool is busy, the next job would have nowhere to go, and one more
runner is added. That is also why the minimum is never zero. A pool with no
runners has nothing to observe, so it could never learn that it should grow;
the idle runner is what makes the pool able to answer the question at all.

It grows one runner at a time, and the daemon comes back within seconds after a
scale-up rather than waiting for its next tick, so a burst ramps quickly without
a single long job conjuring a full fleet.

**Shrinking** is deliberately the slow direction. The pool returns to its
minimum only after five minutes with nothing busy, which rides out the gap
between one job and the next. Shrinking drains like every other change, and it
picks idle runners: a runner with a job on it is kept, not stopped and waited
on.

Minimum equal to maximum is a fixed-size pool that never moves — which is what
every pool was before this existed, and what an upgraded database keeps.

The fleet view says which pool is at what size and why: *every runner is busy*,
*quiet for 7m*, *spare capacity available*.

### Activity

The daemon records what it observed on every pass and keeps two days of it, so
the fleet view can show what has been happening rather than only what is
happening now. The filled area is work actually running; the line above it is
how many runners existed to run it — one axis, both counting runners — so a
pool scaling up reads as the line stepping out to meet the area, and settling
back when the work stops.

![Activity](docs/img/activity.png)

Each point is the **peak** of its interval, not the mean: a burst that filled
the fleet for two minutes is the thing worth seeing, and averaging over a
ten-minute bucket would flatten it into nothing. Narrow it to a single pool
with the filter, or leave it on the whole fleet.

### How quickly a machine comes back

An ephemeral runner is replaced after every job, so the turnaround is paid once
per job. It is about half a minute, and it is worth knowing what it is made of:

| | |
| --- | --- |
| systemd's restart delay | 2s |
| overlay disk, registration token, seed image | 1–2s |
| the guest booting to *Listening for Jobs* | ~16s |

The image is built for that: a machine boots for one job and is destroyed, so
the services a stock cloud image starts and a runner never uses — snapd, modem
and disk managers, the daily apt timers — are turned off in the golden image.

If half a minute matters for your pool, the lever is **capacity, not speed**.
A pool whose minimum is two always has a registered machine waiting while the
other one recycles, so a queue never pays the turnaround at all. Turning
**ephemeral** off is the other end of that trade: the machine is reused between
jobs and the boot disappears entirely, at the cost of a job seeing what the
last one left behind.

### Resources

The **Resources** page is what the host is actually doing, as opposed to what
its pools were promised. Three meters — processor, memory, and the filesystem
holding the state directory, which is the one golden images and machine disks
fill — over a chart of the same three across the last day, and under it a row
per runner.

Everything is read from the kernel, and each runtime is measured the way that
runtime can be measured. A **container** is asked of Docker, minus the page
cache, which is what makes a container that has cloned a large repository look
about to die when it is fine. A **machine** is asked of systemd: every runner is
a unit, every unit is a cgroup, and the accounting is already there — so nothing
has to be read out of the guest, and a machine that has wandered off into swap
is still counted. Processor figures are a share of the whole host, the same
scale the meters use, so a runner's number and the host's can be read against
each other. A runner that has only been seen once shows a dash rather than a
zero: a rate needs two readings, and a machine that is busily booting is not
idle.

Beneath them is what the pools have **committed** — what they would take if
every one of them grew to its ceiling at the same moment. That is arithmetic on
the configuration rather than a measurement, and it is the number a quiet fleet
hides: a host at four per cent can still be promising three times the machine it
is on. Over-committing is not flagged as a fault, because it is usually
deliberate; it is stated, and left to you.

Readings are taken every fifteen seconds — `--resource-interval` moves that —
and two days of them are kept, on the same retention as the activity history.
Each point on the chart is the peak of its interval, for the same reason.

### Virtual machines or containers

A **virtual machine** gives a job its own kernel, its own Docker daemon, a disk
that is thrown away afterwards, and passwordless `sudo` — a workflow written for
a GitHub-hosted runner runs unchanged. It boots in seconds from a golden image
built once per host.

A **container** starts faster and costs less, and is a weaker boundary: a job
shares the host kernel. Nested virtualisation in a container means handing the
job the host's `/dev/kvm`, which is a real hole in an already weaker boundary.
Both are offered; the UI says which is which.

The two also differ in what the runner is trusted with. A machine keeps the
credential and mints its own registration tokens, which is what lets it come
back after a reboot with the daemon still down — the job is inside the guest
and never sees it. A container shares everything with its job, so it is given
nothing but a registration token the daemon minted: short-lived, and able only
to register a runner. The cost is that containers are replaced by the daemon
rather than restarted by Docker, so a container that finishes a job while the
daemon is down waits for it to come back.

Container images are expected to carry the GitHub Actions runner. The official
`ghcr.io/actions/actions-runner` works as it is; a custom image is found by
looking for `config.sh`, or told where to look with `FLEET_RUNNER_HOME`.

## How the daemon works

It is a reconciler. The database holds what you asked for; systemd and Docker
hold what exists; every pass compares the two and acts.

```
pools in SQLite  ──►  Plan(desired, actual, GitHub)  ──►  create / start / drain / remove
                            ▲              ▲
                   systemd + Docker    is a job running?
```

Three properties fall out of that, and each has tests that would fail loudly if
it stopped being true:

- **A restarted daemon adopts the fleet.** It has no memory. It reads the
  runners back from the host's environment files and container labels, and if
  they match what the pools ask for it does nothing at all.
- **No change ever fails a job.** Scaling down, editing a pool, rotating a
  token or deleting a pool all *drain*: the runner is asked to stop, finishes
  the job it is on, and only then is removed. Draining a machine is an ACPI
  shutdown, which the guest turns into a clean stop of the runner; the unit
  waits up to an hour for it.
- **A busy runner is never removed.** The host says whether a machine is up;
  only GitHub knows whether a job is on it, and the daemon asks — including for
  runners whose pool has just been deleted, which carry their own scope for
  exactly that reason.

The fleet view shows both facts side by side, because they answer different
questions:

![The fleet view](docs/img/fleet.png)

Each runner's configuration is hashed into a *generation*. A runner whose
generation no longer matches its pool is running the wrong configuration and is
replaced, gracefully. The scaling bounds deliberately are not part of that hash:
the autoscaler moves them many times an hour, and that must never replace a
runner that is already correct.

The hash also carries a build revision, raised when a release changes *how* the
daemon builds a runner rather than what you asked for. Without it, an upgrade
that fixes the building would leave every existing runner on the broken recipe,
since the pool hashes the same before and after — which is exactly what happened
once, and cost an afternoon of deleting containers by hand.

## Credentials

A **GitHub App** is the better of the two, and what the form offers first:

- Nothing expires on a calendar. The daemon signs a short assertion with the
  app's key and exchanges it for an installation token that lives an hour, and
  it does that again whenever it needs to.
- The repositories it can reach are a list you edit on GitHub, not a token you
  regenerate.
- Uninstalling the app revokes everything on this host at once.

![Adding a GitHub App](docs/img/credential-app.png)

Setting one up, in full:

1. Register an App under your account — **Settings → Developer settings →
   GitHub Apps → New GitHub App**.
2. Set **Where can this GitHub App be installed?** to **Only on this account**.
   It never becomes public and nobody else can install it.
3. Under **Webhook**, deselect **Active**. This app is a credential, not an
   integration, and nothing on your host has to be reachable from the internet:
   the daemon only ever makes outbound calls.
4. Give it **Repository permissions → Administration: Read and write** (or, for
   an organisation, **Organization permissions → Self-hosted runners: Read and
   write**).
5. **Homepage URL** is a required field but only a link — GitHub never fetches
   it. Point it anywhere.
6. Generate a private key, install the app, and choose the repositories it may
   use. Paste the app id and the `.pem` into runner-fleet.

A **personal access token** still works everywhere an app does; a pool cannot
tell the difference. Existing installations keep the token they have.

Rotating either — a new key, a new token — replaces the runners using it,
gracefully, as each finishes the job it is on.

## Security

- The web UI is HTTP Basic over a loopback bind, with bcrypt and attempt
  throttling. Until a password is set, the daemon serves nothing.
- Credentials are encrypted with AES-256-GCM under a key in
  `/etc/runner-fleet/master.key` (0600, root, generated on first start). The
  daemon has to decrypt to use them, so this is not protection against root: it
  protects backups, snapshots and disks that travel where the key does not.
- The decrypted copy a runner needs lives in `/run/runner-fleet`, which is a
  tmpfs. It never reaches a disk, and a runner can still register itself after
  a reboot with the daemon still starting — for an app that means the private
  key is there, and the agent does its own token exchange. That is the cost of
  runners that do not depend on the daemon: losing the host means uninstalling
  the app to revoke it.
- GitHub recommends self-hosted runners only for **private** repositories: on a
  public one, a pull request from a fork can run arbitrary code on the runner.
  A VM is a much harder boundary than a container, and ephemeral runners give
  each job a clean machine.

## Layout on the host

| | |
| --- | --- |
| `/usr/local/bin/runner-fleet` | the daemon, the agent and the CLI, in one binary |
| `/etc/runner-fleet/fleet.db` | pools and credentials |
| `/etc/runner-fleet/master.key` | the encryption key |
| `/etc/runner-fleet/runners/` | one environment file per VM runner |
| `/run/runner-fleet/credentials/` | decrypted tokens, on tmpfs |
| `/var/lib/runner-fleet/` | golden images and VM disks |
| `/var/lib/runner-fleet/consoles/` | the last console of each machine, kept after it is gone |
| `gh-runner@NAME.service` | one runner |
| `runner-fleetd.service` | the daemon |

## When a runner will not come up

The Fleet page says `failing` beside a runner the host is refusing to keep
running, with what went wrong and the command that explains it. Two places have
the detail:

```bash
sudo journalctl -u gh-runner@web-1 -n 50     # the agent, on the host
sudo cat /var/lib/runner-fleet/consoles/web-1.log   # inside the machine
```

A machine is rebuilt from scratch on every start, so its console would go with
it — the last one is kept there instead, which is the only account of what
happened inside a machine that has already gone.

To look inside a machine that is still up, the daemon keeps a key for exactly
that. The port is in the agent's log line for that runner:

```bash
journalctl -u gh-runner@web-1 | grep booting        # ssh_port=43209
ssh -i /var/lib/runner-fleet/ssh/id_ed25519 -p 43209 ubuntu@127.0.0.1
systemd-analyze blame | head            # where a boot went
journalctl -u github-runner              # the runner itself
```

A machine that powers off without its runner ever being able to take a job is
reported as a failure rather than treated as a runner finishing, so a fleet that
is looping does not look like a fleet that is working.

## Cutting a release

**Actions → release → Run workflow**, and pick `patch`, `minor` or `major` — or
type an exact version. It runs the suite first and only tags if that passes, so
a failed release leaves no tag behind, then builds both architectures and
publishes the archives with their checksums.

A tag pushed by hand does the same thing:

```bash
git tag v0.2.0 && git push origin v0.2.0
```

Releases are cut from `main`; the workflow refuses to run anywhere else.

## Development

```bash
make ui        # build the web interface into the Go module
make build     # build the binary
make test      # go vet + go test
make ui-test   # vitest
make dev       # run against ./tmp, no root, nothing on the real host
```

The UI runs on its own with `npm --prefix web run dev`, proxying to the daemon
on 8080.

Tests cover everything that does not need a hypervisor: the reconciler's rules
as table tests against fake executors, the rendered units and container specs,
the API including every authentication path, encryption, and end-to-end runs
that drive the daemon over HTTP against a real database. Booting a guest needs
`/dev/kvm`, which hosted CI runners do not have, so that stays a manual check.

## Roadmap

**Per-repository images.** A repository's toolchain baked into its own image, so
a job does not pay for the install every time. The pieces are already in place:
a pool carries an image field, image names are content-addressed by their
package list, and container pools can already name any image. What is missing is
letting a pool carry a package list and building the variant on demand.

## Licence

[MIT](LICENSE)
