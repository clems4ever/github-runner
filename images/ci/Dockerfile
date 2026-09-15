# The runner image for the organisation's CI pool.
#
# images/dind is the minimum that lets a container pool run a Docker daemon.
# This is that, plus the toolchains the jobs on this pool would otherwise
# install on every single run — and "every single run" is the whole argument,
# because an ephemeral runner starts from this image and keeps nothing.
#
# WHAT IS IN HERE AND WHY, WITH THE MEASUREMENTS THAT PUT IT THERE.
#
# Taken from runyard-ai/workspace's jobs over their first day on the pool:
#
#   gcc     17s  `Go backend` apt-gets it for cgo, which `go test -race` needs
#   make     5s  `OpenAPI contracts` apt-gets it to run the Makefile
#   node     4s  setup-node, when nothing is in the tool cache to find
#   go       8s  setup-go, downloading and extracting the toolchain
#
# The Go and Node versions are NOT installed on PATH. They go into the runner's
# tool cache, which is where actions/setup-go and actions/setup-node look
# before they download anything: find a match there and they skip the download
# entirely, miss and they fetch as they do today. So a repository that moves to
# a version this image does not carry gets slower, never broken.
#
# WHAT THIS DELIBERATELY DOES NOT FIX. `Set up Go` costs ~34s on this pool and
# only 8s of that is the toolchain. The other ~23s is actions/cache pulling a
# 282 MB Go build cache over the network, and no image can hold that — it is
# per-repository, it changes every day, and it buys back 130-250s of compiling.
# The fix for that one is host-side state shared between runners, not a layer.
ARG RUNNER_IMAGE=ghcr.io/actions/actions-runner:latest
FROM ${RUNNER_IMAGE}

# Both toolchains, as the repositories on this pool ask for them today:
#
#   go 1.26.8  runyard-ai/workspace — go.mod says `go 1.26`, and setup-go reads
#              that as "newest 1.26.x", so this tracks the newest patch
#   go 1.26.6  runyard-ai/runyard — go.mod pins the patch, so this is exact
#   node 20    workspace's web build and its contract generator
#   node 22    runyard's consoles
#
# Bump them when a go.mod does. Nothing breaks if you forget; the job pays the
# download again, which is what it pays today.
ARG GO_VERSIONS="1.26.8 1.26.6"
ARG NODE_VERSIONS="20 22"
ARG TARGETARCH=amd64

USER root

# What dockerd needs before it can build a network, and what a build then wants.
# Same list as images/dind — see that file for why each one is here; without
# iptables the daemon exits at startup and takes the runner with it.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      iptables \
      iproute2 \
      kmod \
      pigz \
      e2fsprogs \
      xfsprogs \
# And the toolchain a job would otherwise apt-get on every run. build-essential
# rather than gcc alone: it carries make and libc6-dev, so it covers both jobs
# that install something today in one layer.
      build-essential \
      ca-certificates \
      curl \
      xz-utils \
 && rm -rf /var/lib/apt/lists/*

# docker compose, as a CLI plugin.
#
# Not a nicety: the stock image carries exactly one plugin, buildx, and a job
# that runs `docker compose config` on a container runner fails on a tree with
# nothing wrong with it. runyard's go-ci had to split its two compose parses
# into a separate job on the VM pool for exactly this — a job that runs for ten
# seconds and then queues for minutes behind real work.
ARG COMPOSE_VERSION=v2.40.3
RUN curl -fsSL --retry 3 \
      "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-$(uname -m)" \
      -o /usr/local/lib/docker/cli-plugins/docker-compose \
 && chmod 0755 /usr/local/lib/docker/cli-plugins/docker-compose \
 && docker compose version

# The tool cache.
#
# AGENT_TOOLSDIRECTORY is what the runner reads to set RUNNER_TOOL_CACHE, and
# RUNNER_TOOL_CACHE is where every actions/setup-* looks first. The layout is
# the one GitHub's own images use and the one the toolkit expects:
#
#   $TOOLS/<tool>/<version>/<arch>/        the tool itself
#   $TOOLS/<tool>/<version>/<arch>.complete   a marker, or the directory is ignored
#
# The marker is not optional and its absence is silent: the directory is there,
# setup-go walks past it, and the only sign is a download in the log.
ENV AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache

# Plain COPY and an explicit interpreter, rather than COPY --chmod and running
# the file: --chmod is a BuildKit extension, and a host whose dockerd still has
# the classic builder fails the build on it rather than falling back.
COPY install-tools.sh /tmp/install-tools.sh
RUN bash /tmp/install-tools.sh "${GO_VERSIONS}" "${NODE_VERSIONS}" "${TARGETARCH}" \
 && rm /tmp/install-tools.sh \
# The job runs as `runner`, and setup-go writes into the tool cache when it has
# to fetch a version this image does not carry. Owned rather than world-
# readable so that write lands where it belongs instead of failing.
 && chown -R runner:runner /opt/hostedtoolcache

# Back to the account the runner was built for. The agent starts as root to
# start the Docker daemon and returns to this user before it registers
# anything, so this is what a job runs as.
USER runner
