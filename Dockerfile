FROM ubuntu:24.04

# Version of the GitHub Actions runner to install.
# See https://github.com/actions/runner/releases
ARG RUNNER_VERSION=2.336.0
# x64 or arm64
ARG RUNNER_ARCH=x64
# SHA-256 of the runner tarball, as published in the release notes.
ARG RUNNER_SHA256_X64=04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d
ARG RUNNER_SHA256_ARM64=58b758e420b87093fbd4bfddd368074960053e2f1388f01848c82624b90f27d1
# Set to "true" to also install the Docker CLI + compose plugin, so jobs can
# talk to a Docker daemon mounted through /var/run/docker.sock.
ARG INSTALL_DOCKER_CLI=false

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        gnupg \
        jq \
        sudo \
        tar \
        unzip \
        zip \
    && rm -rf /var/lib/apt/lists/*

RUN if [ "$INSTALL_DOCKER_CLI" = "true" ]; then \
        install -m 0755 -d /etc/apt/keyrings \
        && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
        && chmod a+r /etc/apt/keyrings/docker.asc \
        && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
            > /etc/apt/sources.list.d/docker.list \
        && apt-get update \
        && apt-get install -y --no-install-recommends docker-ce-cli docker-buildx-plugin docker-compose-plugin \
        && rm -rf /var/lib/apt/lists/*; \
    fi

# The runner refuses to run as root, so use a dedicated unprivileged user.
# Ubuntu 24.04 already ships a uid 1000 "ubuntu" user, hence 1001.
RUN useradd --create-home --shell /bin/bash --uid 1001 runner \
    && echo "runner ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/runner \
    && chmod 0440 /etc/sudoers.d/runner \
    && mkdir -p /home/runner/actions-runner /home/runner/_work \
    && chown -R runner:runner /home/runner

WORKDIR /home/runner/actions-runner

# Download, verify and extract the runner package.
RUN set -eux; \
    case "$RUNNER_ARCH" in \
        x64) sha256="$RUNNER_SHA256_X64" ;; \
        arm64) sha256="$RUNNER_SHA256_ARM64" ;; \
        *) echo "unsupported RUNNER_ARCH: $RUNNER_ARCH" >&2; exit 1 ;; \
    esac; \
    tarball="actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"; \
    curl -fsSL -o "$tarball" \
        "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${tarball}"; \
    echo "${sha256}  ${tarball}" | sha256sum -c -; \
    tar xzf "./${tarball}"; \
    rm "./${tarball}"; \
    ./bin/installdependencies.sh; \
    chown -R runner:runner /home/runner/actions-runner

COPY --chown=root:root --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

USER runner
ENV RUNNER_WORKDIR=/home/runner/_work

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
