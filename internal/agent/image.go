package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// BuildTimeout bounds one image build.
//
// A build that fails now says so and powers off, but one that HANGS says
// nothing — a recipe waiting on a prompt, a download against a host that
// accepted the connection and then went quiet. Without a deadline that is a
// machine burning two vCPU until somebody notices, and a pool that never gets
// a runner.
const BuildTimeout = 40 * time.Minute

// ErrImageNotBuilt is what a runner says when the golden image its pool asks
// for is not on this host.
//
// It is not a failure to recover from and it is deliberately not something a
// runner fixes: the daemon builds images, once, where the result can be
// reported and kept. A runner that finds one missing has been started too
// early, and the honest thing for it to do is say so and stop rather than
// build one of its own in the dark.
var ErrImageNotBuilt = errors.New("the golden image this pool asks for has not been built on this host")

// ExitImageNotBuilt is what the agent exits with when it finds no image, and
// what the unit is told never to restart after.
//
// Restarting would be the loop this whole arrangement exists to end: a runner
// that cannot boot, replaced every two seconds, until a start limiter nobody
// reads gives up on it. The daemon is what fixes this — by building the image,
// or by not creating the runner — and it can only do that if the unit stays
// stopped and says why.
const ExitImageNotBuilt = 3

// BuildPhase is where a build has got to. It is what a page watching one shows
// while it waits, and the two phases are slow for different reasons.
type BuildPhase string

const (
	// BuildFetching is the stock Ubuntu image coming down. It happens once per
	// host and it is the slowest part of a first build, with no console to
	// show for itself: no machine has booted yet.
	BuildFetching BuildPhase = "fetching"
	// BuildRunning is the machine running its provisioning. This is where a
	// pool's recipe runs, and where a console exists to say what it is doing.
	BuildRunning BuildPhase = "running"
)

// GoldenImage is where a spec's image lives on this host, and whether it is
// there.
func GoldenImage(spec ImageSpec, imagesDir string) (string, bool) {
	path := spec.Path(imagesDir)
	_, err := os.Stat(path)
	return path, err == nil
}

// BuildOptions is one image build.
type BuildOptions struct {
	Spec      ImageSpec
	ImagesDir string
	// PublicKey is the host's key, baked in so a machine built from this image
	// can be looked inside when it misbehaves.
	PublicKey string
	// Journal is the whole account of the build: what the builder is doing,
	// and everything the build machine prints on its console, in one stream
	// and in order. It is what somebody watching reads and what is kept
	// afterwards, which is the same thing on purpose — a log that only exists
	// once the build has failed is a log nobody could have watched.
	//
	// It may be written from more than one goroutine, so it has to be safe for
	// concurrent use.
	Journal io.Writer
	// Phase is called when the build moves on, so a page watching it can say
	// which of the slow parts it is in. Optional.
	Phase func(BuildPhase)
	Log   *slog.Logger
}

// BuildImage builds one golden image and publishes it, returning where it put
// it.
//
// The daemon does this, not a runner: it takes minutes, it is worth reporting
// while it happens, and its outcome decides whether a pool may take work at
// all. Callers serialise their own builds — two of these at once would fight
// over the working directory.
func BuildImage(ctx context.Context, o BuildOptions) (string, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Journal == nil {
		o.Journal = io.Discard
	}
	if o.Phase == nil {
		o.Phase = func(BuildPhase) {}
	}

	if err := os.MkdirAll(o.ImagesDir, 0o700); err != nil {
		return "", err
	}
	golden := o.Spec.Path(o.ImagesDir)

	ctx, cancel := context.WithTimeout(ctx, BuildTimeout)
	defer cancel()

	say(o.Journal, "building %s", filepath.Base(golden))
	say(o.Journal, "%d packages, %d bytes of recipe",
		len(o.Spec.EffectivePackages()), len(o.Spec.Recipe))

	work := filepath.Join(o.ImagesDir, "build")
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	o.Phase(BuildFetching)
	base := filepath.Join(o.ImagesDir, "cloud-"+UbuntuRelease+".img")
	if _, err := os.Stat(base); os.IsNotExist(err) {
		say(o.Journal, "fetching %s; this happens once per host", cloudImageURL())
		// Nothing prints while a few hundred megabytes come down, so the file's
		// own size is reported: the difference between a slow download and a
		// dead one is whether the number moves.
		stop := watchDownload(base, o.Journal)
		err := run(ctx, "curl", "-fsSL", "-o", base, cloudImageURL())
		stop()
		if err != nil {
			return "", fmt.Errorf("download the Ubuntu cloud image: %w", err)
		}
	}
	say(o.Journal, "preparing a %d GB disk from the cloud image", goldenSizeGB)

	building := filepath.Join(work, "golden.qcow2")
	if err := run(ctx, "qemu-img", "convert", "-O", "qcow2", base, building); err != nil {
		return "", err
	}
	if err := run(ctx, "qemu-img", "resize", "-q", building, fmt.Sprintf("%dG", goldenSizeGB)); err != nil {
		return "", err
	}

	seed := filepath.Join(work, "seed.iso")
	if err := makeSeed(ctx, buildUserData(o.Spec, o.PublicKey),
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

	// The machine is about to boot, which is when a console starts existing
	// and the build stops being a silent download.
	o.Phase(BuildRunning)
	say(o.Journal, "booting the build machine; everything below is its console")

	cmd, err := bootVM(ctx, options)
	if err != nil {
		return "", err
	}
	// Copied into the journal as it is printed rather than at the end, because
	// the whole reason anybody opens this log is that the build has been going
	// for six minutes and they want to know what it is doing.
	stop := followConsole(options.Console, o.Journal)
	waitErr := cmd.Wait()
	console := stop()

	if ctx.Err() != nil {
		return "", fmt.Errorf("the build did not finish within %s: %w", BuildTimeout, ctx.Err())
	}
	if waitErr != nil && ignoreCleanExit(waitErr) != nil {
		return "", fmt.Errorf("the build machine failed: %w", waitErr)
	}

	// A guest that powered off is not a guest that succeeded. It powers off
	// when its recipe fails too — deliberately, so a failure is not a hang —
	// and it would power off if something inside it panicked. The marker on
	// the console is the build saying it got to the end, and without it a
	// half-provisioned image would be published and booted by every job in the
	// pool.
	if !buildSucceeded(console) {
		return "", errors.New("the build machine stopped without reporting that it finished; " +
			"the console above is what it said (a recipe that exits non-zero fails the build)")
	}

	// Published only once the guest has powered itself off, so an interrupted
	// build cannot leave a half-provisioned image for the next runner to boot.
	if err := os.Rename(building, golden); err != nil {
		return "", err
	}
	say(o.Journal, "published %s", filepath.Base(golden))
	o.Log.Info("golden image ready", "image", filepath.Base(golden))
	return golden, nil
}

// say writes one of the builder's own lines into the journal.
//
// Marked, because the rest of the file is a guest's console and the two are
// worth telling apart when a recipe has printed six hundred lines and the
// question is which step it is on.
func say(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "==> "+format+"\n", args...)
}

// downloadReport is how often the size of the image coming down is written to
// the journal. Often enough that a stalled download is obvious, rarely enough
// that the log is still readable afterwards.
const downloadReport = 20 * time.Second

// watchDownload reports the size of a file being downloaded until it is told
// to stop.
func watchDownload(path string, journal io.Writer) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(downloadReport)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				info, err := os.Stat(path)
				if err != nil {
					continue
				}
				say(journal, "fetching: %d MB so far", info.Size()/(1<<20))
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// consolePoll is how often a running build's console is copied into its
// journal. Reading the end of a file QEMU is appending to costs nothing, and
// this is what decides how live a log somebody is watching feels.
const consolePoll = time.Second

// followConsole copies a machine's console into the journal as it is written,
// and returns everything it copied once told to stop.
//
// The whole console comes back rather than being read again from disk, because
// the working directory it lives in is deleted the moment the build returns —
// and the last thing the guest printed is what decides whether the build
// worked.
func followConsole(path string, journal io.Writer) (stop func() []byte) {
	done := make(chan struct{})
	copied := make(chan []byte, 1)

	go func() {
		var seen bytes.Buffer
		var offset int64
		drain := func() {
			for {
				n, err := copyFrom(path, offset, io.MultiWriter(journal, &seen))
				offset += n
				if err != nil || n == 0 {
					return
				}
			}
		}
		ticker := time.NewTicker(consolePoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				// Once more, so the last thing the machine said before it
				// powered off is in the log rather than lost to the tick it
				// missed.
				drain()
				copied <- seen.Bytes()
				return
			case <-ticker.C:
				drain()
			}
		}
	}()

	return func() []byte {
		close(done)
		return <-copied
	}
}

// consoleChunk is how much of a console is copied at a time. A boot prints
// megabytes; this keeps one read from holding all of it in memory at once.
const consoleChunk = 64 << 10

// copyFrom copies up to one chunk of a file from an offset, and says how much
// it moved.
func copyFrom(path string, offset int64, out io.Writer) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.CopyN(out, f, consoleChunk)
}

// buildSucceeded reports whether the guest said it finished. A console that
// could not be read says nothing, and nothing is not success.
func buildSucceeded(console []byte) bool {
	return bytes.Contains(console, []byte(ImageReadyMarker))
}
