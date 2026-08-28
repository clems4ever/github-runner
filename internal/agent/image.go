package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// cloudImageURL is the stock Ubuntu image the golden image is built from.
func cloudImageURL() string {
	return fmt.Sprintf("https://cloud-images.ubuntu.com/%s/current/%s-server-cloudimg-amd64.img",
		UbuntuRelease, UbuntuRelease)
}

// goldenSizeGB is how big the golden image is grown before provisioning. A
// machine's own disk is an overlay on this and can be larger, never smaller.
const goldenSizeGB = 20

// buildTimeout bounds one image build.
//
// A build that fails now says so and powers off, but one that HANGS says
// nothing — a recipe waiting on a prompt, a download against a host that
// accepted the connection and then went quiet. Without a deadline that is a
// machine burning two vCPU until somebody notices, and a pool that never gets
// a runner. Under the stale-lock timer, so the build that is still holding the
// lock is the one that gives up first.
const buildTimeout = 40 * time.Minute

// EnsureImage returns the golden image for a spec, building it if this host
// does not have it yet.
//
// The build takes minutes and happens once per host per image. Two runners
// starting together must not both build into the same file, which is what the
// lock is for; the loser waits and finds the image finished.
//
// This is also where per-repository images will arrive: a pool naming its own
// variant already gets its own image name, so the remaining work is letting a
// pool carry a package list rather than inheriting the default one.
func EnsureImage(ctx context.Context, spec ImageSpec, imagesDir, publicKey string, log *slog.Logger) (string, error) {
	if err := os.MkdirAll(imagesDir, 0o700); err != nil {
		return "", err
	}
	golden := spec.Path(imagesDir)
	if _, err := os.Stat(golden); err == nil {
		return golden, nil
	}

	unlock, err := lock(filepath.Join(imagesDir, ".build.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()

	// Another agent may have finished while this one waited.
	if _, err := os.Stat(golden); err == nil {
		return golden, nil
	}

	log.Info("building the golden image; this takes a few minutes and happens once per host",
		"image", filepath.Base(golden))

	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()

	work := filepath.Join(imagesDir, "build")
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	base := filepath.Join(imagesDir, "cloud-"+UbuntuRelease+".img")
	if _, err := os.Stat(base); os.IsNotExist(err) {
		if err := run(ctx, "curl", "-fsSL", "-o", base, cloudImageURL()); err != nil {
			return "", fmt.Errorf("download the Ubuntu cloud image: %w", err)
		}
	}

	building := filepath.Join(work, "golden.qcow2")
	if err := run(ctx, "qemu-img", "convert", "-O", "qcow2", base, building); err != nil {
		return "", err
	}
	if err := run(ctx, "qemu-img", "resize", "-q", building, fmt.Sprintf("%dG", goldenSizeGB)); err != nil {
		return "", err
	}

	seed := filepath.Join(work, "seed.iso")
	if err := makeSeed(ctx, buildUserData(spec, publicKey),
		metaData("runner-fleet-build", fmt.Sprintf("build-%d", time.Now().Unix())), seed); err != nil {
		return "", err
	}

	port, err := freePort()
	if err != nil {
		return "", err
	}
	options := VMOptions{
		Name: "runner-fleet-build", Dir: work, Disk: building, Seed: seed,
		CPUs: 2, MemoryMB: 2048, SSHPort: port,
		// The build machine never runs anything nested, whatever the pool that
		// triggered it asked for.
		CPUModel:  CPUModel(CPUVendor(), false),
		QMPSocket: filepath.Join(work, "qmp.sock"),
		Console:   filepath.Join(work, "console.log"),
	}

	cmd, err := bootVM(ctx, options)
	if err != nil {
		return "", err
	}
	waitErr := cmd.Wait()

	// Read and kept before the working directory goes, because the console is
	// the only account of what happened inside the machine and the errors
	// below both name it and read it. It used to name a file this function had
	// already deleted.
	console, saved := keepBuildConsole(options.Console, imagesDir)

	if ctx.Err() != nil {
		return "", fmt.Errorf("the image build did not finish within %s; the console is at %s: %w",
			buildTimeout, saved, ctx.Err())
	}
	if waitErr != nil && ignoreCleanExit(waitErr) != nil {
		return "", fmt.Errorf("the image build failed; the console is at %s: %w", saved, waitErr)
	}

	// A guest that powered off is not a guest that succeeded. It powers off
	// when its recipe fails too — deliberately, so a failure is not a hang —
	// and it would power off if something inside it panicked. The marker on
	// the console is the build saying it got to the end, and without it a
	// half-provisioned image would be published and booted by every job in the
	// pool.
	if !buildSucceeded(console) {
		return "", fmt.Errorf("the image build did not report that it finished; the console is at %s "+
			"(a recipe that exits non-zero fails the build)", saved)
	}

	// Published only once the guest has powered itself off, so an interrupted
	// build cannot leave a half-provisioned image for the next runner to boot.
	if err := os.Rename(building, golden); err != nil {
		return "", err
	}
	log.Info("golden image ready", "image", filepath.Base(golden))
	return golden, nil
}

// buildSucceeded reports whether the guest said it finished. A console that
// could not be read says nothing, and nothing is not success.
func buildSucceeded(console []byte) bool {
	return bytes.Contains(console, []byte(ImageReadyMarker))
}

// keepBuildConsole reads a finished build's console and copies it somewhere it
// will still exist when somebody reads the error, returning both. The working
// directory it came from is removed as EnsureImage returns.
//
// Beside the images rather than with the runners' consoles, because it is
// about an image and not about a runner, and because a failed build has no
// runner to file it under. Best effort: if the copy cannot be made, the
// original path is still the most useful thing to name.
func keepBuildConsole(console, imagesDir string) ([]byte, string) {
	out, err := os.ReadFile(console)
	if err != nil {
		return nil, console
	}
	saved := filepath.Join(imagesDir, "last-build-console.log")
	if err := os.WriteFile(saved, out, 0o600); err != nil {
		return out, console
	}
	return out, saved
}

// lock takes an exclusive lock, waiting for whoever holds it.
func lock(path string) (func(), error) {
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return func() {
				file.Close()
				os.Remove(path)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		// Someone else is building. A stale lock from a killed process would
		// otherwise wait for ever, so it is broken after long enough that a
		// real build would have finished.
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 45*time.Minute {
			os.Remove(path)
			continue
		}
		select {
		case <-time.After(5 * time.Second):
		}
	}
}
