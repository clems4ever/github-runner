// Package containerimage is what a container pool bakes into the image its
// runners start from.
//
// A machine pool has had this since the beginning: extra packages and a recipe,
// run once while a golden image is built, so that what a job would otherwise
// install on every run is already there. A container pool had no equivalent —
// it named a prebuilt image and that was the whole of it — so the choice was to
// build and publish an image by hand, somewhere the host can pull from, or to
// pay for the install on every job.
//
// On an ephemeral pool that second cost is not small and it never stops. The
// pool that prompted this runs sixteen jobs a push; four of them `apt-get
// install` a compiler and a compose plugin the image does not carry, twelve
// seconds at a time, on every runner, for ever.
//
// So: the same two fields, and the daemon builds the image. What differs from
// the machine side is only how the image is made — a Dockerfile and a build,
// rather than a machine booted from a cloud image — and deliberately nothing
// else. The name is a hash of everything it is built from, the build is queued
// and logged and kept the same way, and a pool takes no jobs until its image
// exists.
package containerimage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Prefix is the repository built images are tagged under, so that everything
// this daemon made on a host can be found — and told apart from an image an
// operator pulled — with one `docker images` filter.
const Prefix = "runner-fleet/pool"

// dockerfileVersion is bumped when the generated Dockerfile changes in a way
// that changes what is IN the image.
//
// It is hashed into the name, for the reason the machine side hashes its
// provisioning script: a release that changes what a build does, without
// changing anything the operator typed, would otherwise produce the same name,
// find that image already on the host, and reuse it. The fix would ship,
// install, and do nothing at all.
const dockerfileVersion = "1"

// Spec is everything an image is built from.
type Spec struct {
	// Base is the pool's image field: what the build starts FROM. Empty means
	// the executor's default runner image.
	Base string
	// Packages are apt packages, installed as root before the recipe runs.
	Packages []string
	// Recipe is arbitrary shell, run as root in the build. It is for what apt
	// cannot give: a toolchain at a version no archive carries, a pinned
	// linter, a warm cache.
	Recipe string
}

// Wanted reports whether there is anything to build. A pool that names a
// prebuilt image and nothing else keeps working exactly as it did: no build, no
// wait, no image belonging to this daemon.
func (s Spec) Wanted() bool { return len(s.Packages) > 0 || strings.TrimSpace(s.Recipe) != "" }

// Name is the tag this image is built as: a hash of everything that goes into
// it, so two pools wanting the same thing share one build and a pool that edits
// its recipe asks for a new image rather than quietly keeping the old one.
func (s Spec) Name() string {
	h := sha256.New()
	h.Write([]byte(dockerfileVersion))
	h.Write([]byte(s.base()))
	for _, p := range s.packages() {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	h.Write([]byte(s.Recipe))
	return fmt.Sprintf("%s:%s", Prefix, hex.EncodeToString(h.Sum(nil))[:12])
}

// RecipeFile is what the recipe is called inside the build context.
const RecipeFile = "recipe.sh"

// Files are the build context: the generated Dockerfile, and the recipe beside
// it when there is one.
func (s Spec) Files() map[string][]byte {
	files := map[string][]byte{"Dockerfile": []byte(s.Dockerfile())}
	if recipe := strings.TrimSpace(s.Recipe); recipe != "" {
		files[RecipeFile] = []byte(recipe + "\n")
	}
	return files
}

// Dockerfile is the build, generated rather than written by the operator.
//
// Generated because the interesting half of this feature is that the two fields
// mean the SAME thing on both runtimes: `packages` is apt, `recipe` is shell as
// root, and a pool moved from a machine to a container does not have to be
// rewritten. An operator who wants to write their own Dockerfile already can —
// that is the `image` field, and it stays.
func (s Spec) Dockerfile() string {
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", s.base())
	// Root to install, and back afterwards: the runner image runs as its own
	// unprivileged account, and an image left as root is one whose jobs run as
	// root — a difference nobody asked this feature for.
	fmt.Fprint(&b, "USER root\n")
	if pkgs := s.packages(); len(pkgs) > 0 {
		// One layer, no recommends, and the lists removed in the same layer so
		// they are not carried in the image.
		fmt.Fprintf(&b, "RUN apt-get update \\\n"+
			" && apt-get install -y --no-install-recommends \\\n      %s \\\n"+
			" && rm -rf /var/lib/apt/lists/*\n", strings.Join(pkgs, " \\\n      "))
	}
	if strings.TrimSpace(s.Recipe) != "" {
		// COPY and run, rather than a heredoc or an escaped one-liner. A
		// heredoc needs BuildKit and this builds over the Docker API's /build,
		// which is the classic builder; escaping a script into a single RUN
		// mangles exactly the quoting a recipe is full of. As a file it arrives
		// byte for byte, and the log of a failed build shows what somebody
		// wrote rather than what escaping made of it.
		//
		// `sh -e` so a step that fails fails the build, which is the machine
		// side's rule too. Removed in the same layer: a recipe can carry a
		// token somebody should not find in the image later.
		fmt.Fprintf(&b, "COPY %s /tmp/%s\nRUN sh -e /tmp/%s && rm -f /tmp/%s\n",
			RecipeFile, RecipeFile, RecipeFile, RecipeFile)
	}
	fmt.Fprint(&b, "USER runner\n")
	return b.String()
}

func (s Spec) base() string {
	if strings.TrimSpace(s.Base) == "" || s.Base == "default" {
		return DefaultBase
	}
	return s.Base
}

// DefaultBase is what a pool that names no image builds on top of. It is the
// executor's default runner image, repeated here rather than imported because
// the executor imports this package.
const DefaultBase = "ghcr.io/actions/actions-runner:latest"

// packages is the list, sorted and deduplicated, so that two pools asking for
// the same set in a different order share an image.
func (s Spec) packages() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(s.Packages))
	for _, p := range s.Packages {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
