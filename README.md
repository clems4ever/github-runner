# github-runner

[![test](https://github.com/clems4ever/github-runner/actions/workflows/test.yml/badge.svg)](https://github.com/clems4ever/github-runner/actions/workflows/test.yml)

Run [self-hosted GitHub Actions runners](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners)
in throwaway QEMU virtual machines. One VM per runner, deleted when it stops.

A job gets a machine of its own: its own Docker daemon, a kernel it is free to
break, and `/dev/kvm` too if you install the runner with `--nested`. Nothing it
does survives into the next job.

It is one bash script with no dependencies beyond QEMU.

## Getting started

**1. Install**

```bash
curl -fsSL https://raw.githubusercontent.com/clems4ever/github-runner/main/runner-vm.sh \
  | sudo bash -s -- install
```

**2. Check the host**

```bash
sudo runner-vm.sh doctor
```

It prints the fix for anything missing — usually installing QEMU:

```bash
sudo apt-get install -y qemu-system-x86 qemu-utils cloud-image-utils
```

Only if you want jobs to boot VMs of their own, the host needs nested
virtualisation on as well:

```bash
echo 'options kvm_amd nested=1' | sudo tee /etc/modprobe.d/kvm_amd.conf   # kvm_intel on Intel
sudo modprobe -r kvm_amd && sudo modprobe kvm_amd nested=1
```

**3. Build the image**

Once per host, a few minutes. Every VM afterwards boots from a copy-on-write
overlay on it, in seconds.

```bash
sudo runner-vm.sh build
```

**4. Run a runner**

Take a registration token from *Settings → Actions → Runners → New self-hosted
runner* and:

```bash
sudo runner-vm.sh run --url https://github.com/OWNER/REPO --token AAAA...
```

The runner appears in that page within a minute, and jobs reach it with
`runs-on: self-hosted`. Ctrl-C stops it and deletes the VM.

**5. Keep it running across reboots**

A registration token is good for an hour, and a VM registers afresh on every
boot, so anything long-lived needs a credential that can mint tokens — a PAT or
a GitHub App. Then:

```bash
sudo runner-vm.sh install --service \
  --url https://github.com/OWNER/REPO \
  --github-token github_pat_...
```

That stores the credential at `/etc/runner-vm/creds/pat` (root-only), installs a
systemd unit, and starts `runner-vm@runner-1`. It comes back after a reboot.

```bash
runner-vm.sh list                         # what is running
sudo journalctl -u runner-vm@runner-1 -f  # what it is doing
```

## More runners for the same repository

One runner takes one job at a time. `--replicas` sets up several, each with a VM
of its own, so the repository can run that many jobs at once:

```bash
sudo runner-vm.sh install --service --name web \
  --url https://github.com/OWNER/REPO --replicas 3
```

They are named `web-1`, `web-2`, `web-3` and get a configuration file each, so
raising the count later leaves the runners already going untouched, and one of
them can be given a different size or labels by editing its own file. Lowering
it does not stop anything — install names the runners left over and prints the
command to remove them, because stopping one is stopping a machine that may be
halfway through a job.

Size the total against the host: three runners at the default 4 GiB want 12 GiB
of RAM and three cores' worth of contention, plus a 40 GiB disk each.

## Several repositories on one host

Every runner is an instance of one unit template and reads its own
configuration, so adding a repository is running `install --service` again with
a different `--name`:

```bash
sudo runner-vm.sh install --service --name web \
  --url https://github.com/OWNER/web --github-token github_pat_...

sudo runner-vm.sh install --service --name api \
  --url https://github.com/OWNER/api
```

The second one reuses the credential already on the host — pass a token of its
own if that repository needs a different one — and leaves the first runner's
configuration untouched. Sizes and labels are per runner too:

```bash
sudo runner-vm.sh install --service --name big \
  --url https://github.com/OWNER/web --cpus 8 --memory 16384 --labels big
```

What ends up where:

| | |
| --- | --- |
| `/etc/runner-vm/env` | defaults shared by every runner on the host |
| `/etc/runner-vm/env.NAME` | one runner's settings, read after the shared file so it wins |
| `/etc/runner-vm/creds/pat` | the credential runners use unless they have their own |
| `/etc/runner-vm/creds/pat.NAME` | one runner's own credential (likewise `app.pem` and `app.NAME.pem`) |

Editing a file takes effect on the next restart of that runner alone:

```bash
sudoedit /etc/runner-vm/env.api
sudo systemctl restart runner-vm@api   # waits for the job in flight
```

If the repositories are all in one organisation, an organisation-level runner is
simpler than one per repository: point `--url` at
`https://github.com/ORGANISATION` and every repository in it can use the runner.
GitHub has no equivalent for a personal account, so repositories under a user
account need one runner each.

## Commands

| | |
| --- | --- |
| `doctor` | Check KVM, nested virtualisation and QEMU, and say how to fix what is missing |
| `build` | Build the golden image the VMs boot from |
| `run` | Boot a VM and run a runner in it until stopped |
| `list` | The VMs on this host: state, repository, size, nested virtualisation, ssh port, uptime |
| `install` | Install this script; `--service` also sets up systemd |
| `clean` | Stop the services and VMs and delete their disks, keeping the image cache |
| `uninstall` | Remove everything: services, unit, configuration, state, the script |
| `print-unit` | Print the systemd unit `install --service` writes |

`runner-vm.sh --help` lists every flag.

## Credentials

Three ways in, in increasing order of how long they last:

| | Lasts | Good for |
| --- | --- | --- |
| `--token` | one hour | trying it out |
| `--github-token` (a PAT) | until the PAT expires | a machine you revisit |
| `--app-id` + `--app-key` | indefinitely | a machine that must stay up |

A fine-grained PAT needs **Administration: Read and write** on the repository,
or the organisation's **Self-hosted runners: Read and write**. A GitHub App
needs the same, and belongs to the organisation rather than to a person, so it
does not expire or leave when someone does.

Any of them can come from a file or stdin instead of the command line, where
`ps` would show it to every user on the machine:

```bash
sudo runner-vm.sh run --url ... --github-token-file /root/pat
printf %s "$PAT" | sudo runner-vm.sh run --url ... --github-token-file -
```

Whatever you pass to `install --service` is stored under `/etc/runner-vm/creds`,
`0600` and root-owned, and handed to the service through systemd's credential
mechanism — so it is never in the unit, never in a command line, and never
readable on disk by the service user. The first credential installed on a host
is the one every runner uses; a runner that needs a different one gets it as
`creds/pat.NAME`.

## What the VM has

Ubuntu 24.04 with the runner, Docker, QEMU, and the toolchain workflows written
against GitHub-hosted runners tend to assume: compilers and autotools, python3,
node and npm, the GitHub CLI, git-lfs, the common `-dev` libraries, and the
usual utilities. Language versions are left to `setup-node`, `setup-python` and
friends, which work exactly as they do on a hosted runner.

Anything else, without editing the script:

```bash
EXTRA_PACKAGES="ffmpeg imagemagick" sudo runner-vm.sh build
```

The package list is hashed into the image name, so changing it builds a new
image rather than reusing one without the new tools.

Verify it from a workflow:

```yaml
jobs:
  build:
    runs-on: self-hosted
    steps:
      - run: docker run --rm hello-world   # the VM's own daemon
      - run: kvm-ok                        # only with --nested
```

## How it works

- `build` boots the Ubuntu cloud image once with a cloud-init seed that
  installs everything, then powers off. That is the golden image.
- `run` creates a qcow2 overlay on it and a second seed carrying the
  registration details and the guest runner script, and boots it with
  `-cpu host,+svm` (or `+vmx`) so the guest can run VMs of its own.
- Inside, a systemd unit registers the runner and runs it as an unprivileged
  user, with its output on the serial console — which is what the script
  streams to your terminal.
- When the runner stops, the VM powers off; when the VM is gone, its disk is
  deleted. Interrupting the script does the same from the other end.

Images and VM disks live in `~/.local/share/runner-vm`, or
`/var/lib/runner-vm` for the service. `RUNNER_VM_HOME` moves them.

## Options

Every flag has an environment variable equivalent.

| Flag | Variable | Default | |
| --- | --- | --- | --- |
| `--url` | `GITHUB_URL` | — | Repository or organisation |
| `--token` | `RUNNER_TOKEN` | — | Registration token |
| `--github-token` | `GITHUB_TOKEN` | — | PAT, to mint tokens per boot |
| `--github-token-file` | `GITHUB_TOKEN_FILE` | — | Read the PAT from a file, or `-` for stdin |
| `--app-id` | `GITHUB_APP_ID` | — | GitHub App id |
| `--app-key` | `GITHUB_APP_PRIVATE_KEY` | — | The app's PEM private key |
| `--name` | `RUNNER_NAME` | `vm-<host>-<pid>` | Runner name |
| `--labels` | `RUNNER_LABELS` | — | Extra labels |
| `--group` | `RUNNER_GROUP` | `Default` | Runner group |
| `--cpus` | `VM_CPUS` | `2` | vCPUs |
| `--memory` | `VM_MEMORY_MB` | `4096` | Memory, MiB |
| `--disk` | `VM_DISK_GB` | `40` | Disk, GiB |
| `--ephemeral` | `EPHEMERAL` | `false` | One job per VM |
| `--nested` | `VM_NESTED` | off | Expose `vmx`/`svm`, so jobs can boot VMs of their own |
| `--replicas` | `REPLICAS` | `1` | How many runners `install --service` sets up on the repository |

With `--ephemeral` the runner takes one job, the VM powers off, and under
systemd `Restart=always` starts a clean one — a genuinely fresh machine per job.

## When a job fails before it runs a step

Every job starts by downloading the actions it uses from
`codeload.github.com`, and those requests are **anonymous**, so they are
rate-limited per source IP:

```
Failed to download action 'https://codeload.github.com/actions/checkout/tar.gz/...'
Error: Response status code does not indicate success: 429 (Too Many Requests).
```

The job dies in *Set up job*, with nothing of its own having run, which reads as
though the runner is broken. It is not, and it is not specific to self-hosted
runners either — the same 429 turns up on GitHub-hosted ones when the service is
busy. A host with a fixed egress address makes it likelier than a hosted runner
getting a fresh address per job, but that is the whole of the difference.

Retrying is usually enough. If it is persistent, the durable answers are to use
fewer network-fetched actions, or to bake the ones you rely on into the image.

## Tests

```bash
tests/run-tests.sh
```

Covers what does not need a hypervisor: the generated cloud-init and systemd
files, credential handling, image naming, port allocation, `list` and `clean`.
CI runs those on every push, and separately installs the script the documented
way to check it touches nothing else and that `uninstall` reverses it. Booting
a guest needs `/dev/kvm`, which hosted runners do not have, so that part is
checked by hand.

## Security

GitHub recommends self-hosted runners only for **private** repositories: on a
public one, a pull request from a fork can run arbitrary code on the runner.

A VM is a much harder boundary than a container, and the only way to give a job
Docker — or `/dev/kvm` — without handing over the host. It is not a promise,
though: a job can still exhaust the host's memory and CPU, and a VM only gets a
clean disk when it is replaced — use `--ephemeral` if you want that per job.

Nested virtualisation is off unless a runner is installed with `--nested`, and
that is deliberate: it is the largest piece of the host's CPU a job gets to
touch. Turning it on is per runner, so a repository that needs it does not
hand it to every other repository on the host.

## Licence

[MIT](LICENSE)
