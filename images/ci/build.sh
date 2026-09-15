#!/usr/bin/env bash
#
# Builds the CI pool's runner image and, given a registry, pushes it.
#
#   images/ci/build.sh                                   # build, tag locally
#   images/ci/build.sh ghcr.io/clems4ever/ci-runner      # build, tag, push
#
# Then point the pool at it: Pools -> edit -> Image, the same tag this printed.
# The daemon replaces a pool's runners when its image changes, so the next job
# lands on the new one; runners mid-job finish first.
#
# The versions live in the Dockerfile's ARGs and are overridable here:
#
#   GO_VERSIONS="1.26.8 1.26.6" NODE_VERSIONS="20 22" images/ci/build.sh
set -euo pipefail

registry=${1:-}
tag=${TAG:-$(date -u +%Y%m%d)}
image=${registry:+${registry}:}${registry:-actions-runner-ci}
[ -n "$registry" ] && image="${registry}:${tag}" || image="actions-runner-ci:${tag}"

cd "$(dirname "$0")"

# --pull, because the base is a floating tag: without it a host that built this
# last month rebuilds on last month's runner, and the one thing worse than an
# old runner image is two hosts disagreeing about which one they have.
docker build \
	--pull \
	--build-arg "GO_VERSIONS=${GO_VERSIONS:-1.26.8 1.26.6}" \
	--build-arg "NODE_VERSIONS=${NODE_VERSIONS:-20 22}" \
	${RUNNER_IMAGE:+--build-arg "RUNNER_IMAGE=${RUNNER_IMAGE}"} \
	-t "$image" \
	.

echo
echo "checking what the image actually carries"
# Each of these is something a job on this pool does today. A build that
# succeeds and then cannot run `make` has not saved anybody anything, and the
# way that would otherwise be discovered is a red pull request.
docker run --rm --entrypoint bash "$image" -euo pipefail -c '
	fail=0
	check() { printf "  %-34s %s\n" "$1" "$2"; }
	for tool in gcc make docker iptables sudo curl git; do
		if path=$(command -v "$tool"); then check "$tool" "$path"; else check "$tool" "MISSING"; fail=1; fi
	done
	if docker compose version >/dev/null 2>&1; then
		check "docker compose" "$(docker compose version --short)"
	else
		check "docker compose" "MISSING"; fail=1
	fi
	# The tool cache, as setup-go and setup-node read it: a directory AND the
	# marker beside it. A directory with no marker is invisible to them, which
	# is the failure this whole image exists to avoid and is silent.
	for dir in "$RUNNER_TOOL_CACHE"/go/*/ "$RUNNER_TOOL_CACHE"/node/*/; do
		[ -d "$dir" ] || continue
		version=$(basename "$(dirname "$dir")")/$(basename "$dir")
		for arch in "$dir"*/; do
			[ -d "$arch" ] || continue
			name=$(basename "$arch")
			if [ -f "${dir}${name}.complete" ]; then
				check "toolcache $version" "complete"
			else
				check "toolcache $version" "NO .complete MARKER — setup-* will ignore it"; fail=1
			fi
		done
	done
	# One at a time. The glob matches every version in the cache, and passing
	# the whole expansion to the first binary runs `go <path-to-other-go>
	# version`, which Go reports as "unknown command" — a confusing way to
	# fail a check on an image that is perfectly fine.
	for go in "$RUNNER_TOOL_CACHE"/go/*/x64/bin/go; do echo "  $("$go" version)"; done
	for node in "$RUNNER_TOOL_CACHE"/node/*/x64/bin/node; do echo "  node $("$node" --version)"; done
	# And the runner is still the one the agent hands the job to.
	check "whoami" "$(id -un)"
	exit $fail
'

echo
echo "built $image"
if [ -n "$registry" ]; then
	docker push "$image"
	echo "pushed $image"
	echo "point the pool's Image field at: $image"
else
	echo "no registry given, so nothing was pushed."
	echo "to publish: images/ci/build.sh ghcr.io/<owner>/<name>"
fi
