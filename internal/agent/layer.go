package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A layer is one repository's additions, sitting on a pool's golden image.
//
// It is an overlay rather than an image of its own, which is the whole point.
// A golden image is twenty gigabytes of Ubuntu, a runner and a compiler
// toolchain, and it takes tens of minutes to build; what a repository adds to
// it is usually a handful of packages. Building a second whole image for that
// would cost the disk and the time of the first one over again, per
// repository, and a host with fifteen repositories on one pool would be
// carrying fifteen copies of the same Ubuntu.
//
// So the layer is a qcow2 with the golden image as its backing file: it holds
// the blocks its own provisioning wrote and nothing else, which for a few apt
// packages is a few hundred megabytes. A machine then boots from an overlay on
// the layer, so the chain is three deep — machine, layer, golden — and the
// collector already keeps whole chains rather than only what a machine names.

// LayerSpec is what a repository adds to a pool's image.
type LayerSpec struct {
	// Base is the file name of the golden image this sits on.
	Base string
	// Packages and Recipe are what the repository asked for, already
	// validated: nothing in here is trusted, and it is not this file that
	// checks it. See internal/repospec.
	Packages []string
	Recipe   string
}

// Name is the file name for a layer, and like a golden image's it is a hash of
// everything that goes into it — here, the image underneath and the
// provisioning on top.
//
// The repository is deliberately not part of it. Two repositories that ask for
// the same packages on the same image are asking for the same layer, and
// building it twice would be two copies of one thing. What the layer is *for*
// is recorded in the database, which is where a question like "why is this on
// my disk" is answered; what it *is* is this hash.
func (s LayerSpec) Name() string {
	h := sha256.New()
	h.Write([]byte(s.Base))
	h.Write([]byte{0})
	h.Write([]byte(layerScript()))
	h.Write([]byte{0})
	h.Write([]byte(s.Recipe))
	for _, pkg := range s.Packages {
		h.Write([]byte(pkg))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("runner-%s-layer-%s.qcow2", UbuntuRelease, hex.EncodeToString(h.Sum(nil))[:12])
}

// Path is where a layer lives. It is beside the image it is backed by, and has
// to stay there: the backing file is recorded inside the qcow2 by name, and is
// resolved relative to the overlay.
func (s LayerSpec) Path(imagesDir string) string { return filepath.Join(imagesDir, s.Name()) }

// BuiltLayer is where a layer lives on this host, and whether it is there.
func BuiltLayer(spec LayerSpec, imagesDir string) (string, bool) {
	path := spec.Path(imagesDir)
	_, err := os.Stat(path)
	return path, err == nil
}

// LayerOptions is one layer build.
type LayerOptions struct {
	Spec      LayerSpec
	ImagesDir string
	PublicKey string
	// Repo is who asked, for the journal. It has no part in the layer's
	// identity.
	Repo    string
	Journal io.Writer
	Phase   func(BuildPhase)
	Log     *slog.Logger
}

// BuildLayer builds one repository's layer and publishes it.
//
// Same shape as BuildImage and for the same reasons: the guest's console is
// the only thing the host can see, a guest that powered off is not a guest
// that succeeded, and nothing is published until the marker has been printed.
func BuildLayer(ctx context.Context, o LayerOptions) (string, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Journal == nil {
		o.Journal = io.Discard
	}
	if o.Phase == nil {
		o.Phase = func(BuildPhase) {}
	}

	base := filepath.Join(o.ImagesDir, o.Spec.Base)
	if _, err := os.Stat(base); err != nil {
		// The pool's own image has to exist first. A layer with no image under
		// it is not something to build; it is something to wait for.
		return "", fmt.Errorf("the image this layer sits on is not built yet: %w", err)
	}

	layer := o.Spec.Path(o.ImagesDir)
	if _, err := os.Stat(layer); err == nil {
		say(o.Journal, "%s is already here", filepath.Base(layer))
		return layer, nil
	}

	ctx, cancel := context.WithTimeout(ctx, BuildTimeout)
	defer cancel()

	say(o.Journal, "building %s for %s", filepath.Base(layer), o.Repo)
	say(o.Journal, "on %s: %d packages, %d bytes of recipe",
		o.Spec.Base, len(o.Spec.Packages), len(o.Spec.Recipe))

	work := filepath.Join(o.ImagesDir, "layer-build")
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	// Built under its final name with a suffix, in the images directory rather
	// than in the working directory, because the backing file is stored inside
	// the qcow2 as a name resolved against wherever the overlay is. Built next
	// door and moved in, it would point at nothing for as long as it took to
	// move, and at nothing for ever if the move crossed a filesystem.
	building := layer + ".partial"
	if err := os.Remove(building); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	o.Phase(BuildRunning)
	if err := runIn(ctx, o.ImagesDir, "qemu-img", "create", "-q", "-f", "qcow2",
		"-F", "qcow2", "-b", o.Spec.Base, filepath.Base(building)); err != nil {
		return "", fmt.Errorf("create the overlay: %w", err)
	}
	// Every path out of here that is not success leaves nothing behind: a
	// half-provisioned overlay on a good image boots, and would be worse than
	// no layer at all.
	published := false
	defer func() {
		if !published {
			os.Remove(building)
		}
	}()

	seed := filepath.Join(work, "seed.iso")
	if err := makeSeed(ctx, layerUserData(o.Spec, o.PublicKey),
		metaData("runner-fleet-layer", fmt.Sprintf("layer-%d", time.Now().UnixNano())), seed); err != nil {
		return "", err
	}

	port, err := freePort()
	if err != nil {
		return "", err
	}
	options := VMOptions{
		Name: "runner-fleet-layer", Dir: work, Disk: building, Seed: seed,
		CPUs: 2, MemoryMB: 2048, SSHPort: port,
		CPUModel:  CPUModel(CPUVendor(), false),
		QMPSocket: filepath.Join(work, "qmp.sock"),
		Console:   filepath.Join(work, "console.log"),
	}

	say(o.Journal, "booting the layer build; everything below is its console")
	cmd, err := bootVM(ctx, options)
	if err != nil {
		return "", err
	}
	stop := followConsole(options.Console, o.Journal)
	waitErr := cmd.Wait()
	console := stop()

	if ctx.Err() != nil {
		return "", fmt.Errorf("the layer build did not finish within %s: %w", BuildTimeout, ctx.Err())
	}
	if waitErr != nil && ignoreCleanExit(waitErr) != nil {
		return "", fmt.Errorf("the layer build machine failed: %w", waitErr)
	}
	if !buildSucceeded(console) {
		return "", errors.New("the layer build stopped without reporting that it finished; " +
			"the console above is what it said (a recipe that exits non-zero fails the build)")
	}

	if err := os.Rename(building, layer); err != nil {
		return "", err
	}
	published = true
	say(o.Journal, "published %s", filepath.Base(layer))
	o.Log.Info("layer ready", "layer", filepath.Base(layer), "repo", o.Repo, "image", o.Spec.Base)
	return layer, nil
}

// layerUserData is the cloud-init for a layer build.
//
// It boots an image cloud-init has already run in once. That works because the
// seed carries a new instance id, which is what cloud-init uses to decide it
// is on a machine it has not provisioned — the same mechanism that makes a
// golden image usable as a runner at all.
//
// It deliberately does not repeat the base provisioning. The runner, the user,
// the sudoers file and the services that were turned off are all in the image
// underneath; running it again would be minutes spent to reach the state the
// disk is already in.
func layerUserData(spec LayerSpec, publicKey string) string {
	var packages strings.Builder
	for _, pkg := range spec.Packages {
		fmt.Fprintf(&packages, "  - %s\n", pkg)
	}

	return fmt.Sprintf(`#cloud-config
hostname: runner-fleet-layer

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s

# A build fetches several hundred packages and takes minutes to do it. Without
# this, one connection dropped anywhere in that is the whole build: apt gives
# up on the file, the install exits non-zero, and everything downloaded so far
# is thrown away with the disk. Retrying costs nothing when the link is good.
apt:
  conf: |
    Acquire::Retries "5";

package_update: true
packages:
%s
write_files:
%s  - path: /usr/local/bin/layer.sh
    permissions: '0755'
    owner: 'root:root'
    content: |
%s

runcmd:
  - [ /usr/local/bin/layer.sh ]
`, publicKey, packages.String(), layerRecipeFile(spec.Recipe), indent(layerScript(), "      "))
}

// layerRecipeFile is the repository's recipe as a write_files entry, or
// nothing when it has none.
func layerRecipeFile(recipe string) string {
	if recipe == "" {
		return ""
	}
	return fmt.Sprintf(`  - path: %s
    permissions: '0755'
    owner: 'root:root'
    content: |
%s
`, layerRecipePath, indent(recipe, "      "))
}

// layerRecipePath is where a repository's recipe lands inside the build.
const layerRecipePath = "/usr/local/bin/repo-recipe.sh"

// layerScript is what cloud-init runs in a layer build: the repository's
// recipe if it has one, and a power-off either way.
//
// The power-off is in a trap for the reason it is in buildScript: it is how
// the host learns the build is over, and a recipe that exits non-zero would
// otherwise leave the machine at a login prompt with the host waiting on it.
// A repository's recipe is somebody else's shell, edited in a pull request,
// and is even less this daemon's to assume things about than a pool's.
func layerScript() string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

finish() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo '%[2]s' > /dev/console
    echo "the layer build failed with status $status" > /dev/console
  fi
  systemctl poweroff --no-block
}
trap finish EXIT

if [ -x %[3]s ]; then
  echo "running this repository's recipe" > /dev/console
  %[3]s
fi

# Not required — a runner's seed carries an instance id of its own, which is
# what makes cloud-init run in it — but this build's logs and cached seed are
# written into every machine booted from this layer for as long as it exists,
# and they are this build's, not theirs.
cloud-init clean --logs >/dev/null 2>&1 || true

touch /var/lib/runner-fleet-layer-ready
echo '%[1]s' > /dev/console
`, ImageReadyMarker, ImageFailedMarker, layerRecipePath)
}

// runIn is run, from a directory. qemu-img records a backing file exactly as
// it is given it, so an overlay is created from the images directory and told
// the plain name of what is under it — which keeps the whole directory
// movable.
func runIn(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// layerName refuses anything that is not a plain image file name.
//
// The name arrives in an environment variable and is joined to the images
// directory, and a runner is the one place in this daemon that is close to a
// job. It is checked rather than trusted because the cost of being wrong is
// booting a machine off a file somebody else chose.
func layerName(name string) error {
	// Spelled out rather than borrowed from the collector's IsImageName: the
	// collector is in a package that imports this one.
	if name != filepath.Base(name) ||
		!strings.HasPrefix(name, "runner-") || !strings.HasSuffix(name, ".qcow2") {
		return fmt.Errorf("%q is not an image this daemon builds", name)
	}
	return nil
}
