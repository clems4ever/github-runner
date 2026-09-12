# A runner image that can run a Docker daemon of its own.
#
# The stock image carries the whole static Docker bundle — the client and the
# daemon binaries — on a base that has nothing the daemon needs to build a
# network. dockerd on it starts, fails to set up the bridge, and exits several
# seconds in, which reads as a pool whose runners keep restarting for no
# reason. This adds what is missing and nothing else.
#
# Build and push it somewhere the host can pull from, then name it in the
# pool's image field:
#
#   docker build -t ghcr.io/you/actions-runner-dind:latest images/dind
#   docker push ghcr.io/you/actions-runner-dind:latest
#
# Anything a job would otherwise install on every run belongs here too — this
# is the container equivalent of a machine pool's recipe.
ARG RUNNER_IMAGE=ghcr.io/actions/actions-runner:latest
FROM ${RUNNER_IMAGE}

USER root

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      # What dockerd sets the bridge network up with. Without these it exits
      # at startup and takes the runner with it.
      iptables \
      iproute2 \
      # Loading overlay and br_netfilter, when the host has not already.
      kmod \
      # Parallel decompression of pulled layers. Not required; the daemon
      # warns about its absence on every pull, in a console people read.
      pigz \
      # The storage driver's filesystem tools, for a volume the host formatted
      # as something other than ext4.
      e2fsprogs \
      xfsprogs \
 && rm -rf /var/lib/apt/lists/*

# Back to the account the runner was built for. The agent starts as root to
# start the daemon and returns to whoever owns the runner before it registers
# anything, so this is what a job runs as.
USER runner
