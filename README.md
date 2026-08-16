# github-runner

A [self-hosted GitHub Actions runner](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/about-self-hosted-runners)
you can run on your own machine, two ways: in a container, or in a virtual
machine that is thrown away after use.

Both follow the download / configure / run procedure GitHub shows on the
*Settings → Actions → Runners → New self-hosted runner* page, and both run the
same `entrypoint.sh`. The difference is what a job is allowed to touch.

## Which one

|  | [container](docker/) | [VM](vm/) |
| --- | --- | --- |
| Isolation | a container, sharing the host kernel | a machine of its own, with its own kernel |
| Docker for jobs | the host's daemon through `docker.sock`, which is root on the host | a daemon of its own, inside the VM |
| `/dev/kvm` for jobs | the host's, shared | its own, through nested virtualisation |
| Preinstalled tools | the runner and the Docker CLI | compilers, python, node, `gh`, docker, qemu |
| After a job | whatever it left behind in the volume | the disk is deleted |
| Cost | a few MB, starts instantly | a few GB of RAM, boots in seconds |
| Needs | Docker | KVM and QEMU, plus nested virtualisation for VMs in jobs |

Use the container when the workflows are yours and you trust them. Use the VM
when they are not, when jobs need Docker or KVM without being handed the host,
or when you want a genuinely clean machine per job.

## Quick start

**Container** — [docker/README.md](docker/README.md):

```bash
cd docker
cp .env.example .env      # set GITHUB_URL and RUNNER_TOKEN
docker compose up -d
```

**VM** — [vm/README.md](vm/README.md):

```bash
curl -fsSL https://raw.githubusercontent.com/clems4ever/github-runner/main/vm/runner-vm.sh \
  | sudo bash -s -- install
```

That installs the management script and prints how to boot a first runner. It
touches nothing else on the host.

## Layout

```
entrypoint.sh          registers the runner and runs it — used by both
docker/                the container: Dockerfile, compose files, .env.example
vm/                    the VM: runner-vm.sh, a single self-contained script
.github/workflows/     builds and publishes the container image
```

`entrypoint.sh` sits at the root deliberately: it is the one piece both setups
share, and neither owns it. The container bakes it into the image; the VM ships
it into the guest through cloud-init at boot, so the registration logic exists
once rather than twice.

## Security

GitHub recommends self-hosted runners only for **private** repositories: on a
public one, a pull request from a fork can run arbitrary code on the runner.
That applies to both setups here.

The VM is the stronger boundary of the two, and the only one that can safely
give a job Docker and `/dev/kvm`. It is not a promise, though: a job can exhaust
the host's memory and CPU either way, and a VM only gets a clean disk when it is
replaced — use `--ephemeral` if you want that per job.

The only credential either setup needs is a registration token, which enrols one
runner and expires an hour after it is issued. The VM setup optionally takes a
PAT or a GitHub App to mint those itself, which is what lets a runner come back
after a reboot without anyone pasting anything.
