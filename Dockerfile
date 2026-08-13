FROM ubuntu:24.04

# Version of the GitHub Actions runner to install.
# See https://github.com/actions/runner/releases
ARG RUNNER_VERSION=2.336.0
# SHA-256 of the runner tarball, as published in the release notes.
ARG RUNNER_SHA256_AMD64=04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d
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
    # .runner-state exists so that a volume mounted there is created with the
    # runner's ownership rather than root's.
    && mkdir -p /home/runner/actions-runner /home/runner/_work /home/runner/.runner-state \
    && chown -R runner:runner /home/runner

WORKDIR /home/runner/actions-runner

# Set automatically by BuildKit, one of amd64 / arm64 here.
ARG TARGETARCH

# Download, verify and extract the runner package.
RUN set -eux; \
    target_arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$target_arch" in \
        amd64) runner_arch=x64;   sha256="$RUNNER_SHA256_AMD64" ;; \
        arm64) runner_arch=arm64; sha256="$RUNNER_SHA256_ARM64" ;; \
        *) echo "unsupported architecture: $target_arch" >&2; exit 1 ;; \
    esac; \
    tarball="actions-runner-linux-${runner_arch}-${RUNNER_VERSION}.tar.gz"; \
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
