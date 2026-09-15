#!/usr/bin/env bash
#
# Puts Go and Node into the runner's tool cache, in the layout actions/setup-go
# and actions/setup-node look for. Run from images/ci/Dockerfile; it is a
# script rather than a RUN block so that the version resolution and the
# checksum handling can be read.
#
#   install-tools.sh "1.26.8 1.26.6" "20 22" amd64
#
# A Node version may be a line ("20") or exact ("20.20.2"); a line resolves to
# the newest release on it. A Go version is exact, because that is what a
# go.mod pins and there is no "newest 1.26.x" endpoint worth the guess — see
# the Dockerfile's note on what a miss costs (a download, not a failure).
set -euo pipefail

go_versions=${1:?go versions}
node_versions=${2:?node versions}
arch=${3:-amd64}

# The toolkit's own names for the architecture, which is what setup-* look
# under: x64 rather than amd64, arm64 the same either way.
case "$arch" in
	amd64) tool_arch=x64; node_arch=x64 ;;
	arm64) tool_arch=arm64; node_arch=arm64 ;;
	*) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

tools=${RUNNER_TOOL_CACHE:-/opt/hostedtoolcache}

# A tool is only in the cache once the marker beside it exists. Written last,
# deliberately: a half-extracted directory with no marker is skipped and
# re-downloaded, where a marker written first would advertise a tool that is
# not all there.
complete() {
	touch "$1/$2.complete"
}

fetch() {
	curl -fsSL --retry 3 --retry-delay 2 "$1"
}

for version in $go_versions; do
	dest="$tools/go/${version}/${tool_arch}"
	tarball="go${version}.linux-${arch}.tar.gz"
	echo "go ${version} -> ${dest}"
	mkdir -p "$dest"
	# Checksummed against go.dev's release index.
	#
	# NOT against https://go.dev/dl/<file>.sha256, which looks like the obvious
	# source and is a trap: that path answers 200 with go.dev's HTML rather
	# than a checksum, so a naive `curl | sha256sum -c` fails with "no properly
	# formatted checksum lines" if you are lucky and verifies nothing if you
	# are not. The JSON index carries a sha256 per file and is what the
	# download page itself renders from.
	#
	# Same origin as the tarball either way, so this catches a truncated or
	# corrupted download rather than a hostile mirror. Pinning the digests
	# somewhere else is a decision for whoever runs this, not a default it can
	# invent.
	want=$(fetch "https://go.dev/dl/?mode=json&include=all" |
		python3 -c "
import json,sys
for release in json.load(sys.stdin):
    if release['version'] != 'go${version}':
        continue
    for f in release['files']:
        if f['filename'] == '${tarball}':
            print(f['sha256']); raise SystemExit
    raise SystemExit('go${version} has no ${tarball}')
else:
    raise SystemExit('no such Go release: go${version}')
")
	[ -n "$want" ] || { echo "no checksum for $tarball" >&2; exit 1; }
	fetch "https://go.dev/dl/${tarball}" > "/tmp/${tarball}"
	echo "${want}  /tmp/${tarball}" | sha256sum -c -
	# --strip-components=1: the tarball holds a `go/` directory and the cache
	# wants its CONTENTS at <version>/<arch>, so that <arch>/bin/go is the
	# binary. One level out and setup-go finds the directory, adds
	# <arch>/bin to PATH, and the job fails on `go: command not found`.
	tar -xzf "/tmp/${tarball}" -C "$dest" --strip-components=1
	rm -f "/tmp/${tarball}"
	complete "$tools/go/${version}" "${tool_arch}"
done

for spec in $node_versions; do
	if [[ "$spec" == *.*.* ]]; then
		version="v${spec#v}"
	else
		# Newest release on that major line. index.json is newest-first.
		version=$(fetch https://nodejs.org/dist/index.json |
			python3 -c "
import json,sys
line = 'v${spec}.'
for release in json.load(sys.stdin):
    if release['version'].startswith(line):
        print(release['version']); break
else:
    raise SystemExit('no node release on line ${spec}')
")
	fi
	dest="$tools/node/${version#v}/${node_arch}"
	tarball="node-${version}-linux-${node_arch}.tar.xz"
	echo "node ${version} -> ${dest}"
	mkdir -p "$dest"
	# SHASUMS256.txt covers every artefact of the release; take the line for
	# this one. Same same-origin caveat as Go above.
	want=$(fetch "https://nodejs.org/dist/${version}/SHASUMS256.txt" | awk -v f="$tarball" '$2 == f {print $1}')
	[ -n "$want" ] || { echo "no checksum for $tarball" >&2; exit 1; }
	fetch "https://nodejs.org/dist/${version}/${tarball}" > "/tmp/${tarball}"
	echo "${want}  /tmp/${tarball}" | sha256sum -c -
	tar -xJf "/tmp/${tarball}" -C "$dest" --strip-components=1
	rm -f "/tmp/${tarball}"
	complete "$tools/node/${version#v}" "${node_arch}"
done
