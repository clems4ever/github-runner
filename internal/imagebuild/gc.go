package imagebuild

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/clems4ever/github-runner/internal/paths"
)

// Images accumulate. That is the whole reason this file exists.
//
// An image is named by a hash of everything it is built from, which is what
// makes a pool's edit produce a new image rather than silently reusing the old
// one. The cost of that — and nothing used to pay it — is that every edit,
// every runner-version bump, and every release that changes the build script
// leaves a whole image behind, several gigabytes of it, for ever. A host that
// had been up for a few months was carrying tens of them, and the first anyone
// heard about it was the filesystem filling up underneath the jobs.
//
// So: an image nothing is booting and no pool asks for is garbage. It is kept
// for a grace period anyway, because coming back from an edit within the hour
// is common and rebuilding is minutes — and then it goes.

// GracePeriod is how long an image nothing wants is kept before it is deleted.
//
// It is not zero because the most common reason an image goes unwanted is
// somebody editing a recipe, looking at the result, and putting it back. That
// round trip is minutes and a rebuild is tens of minutes, so the grace pays
// for itself. It is not longer than a day because the point of the grace is to
// survive an afternoon's editing, not to be a second retention policy.
const GracePeriod = 24 * time.Hour

// GC is the state directory's image collector.
type GC struct {
	imagesDir string
	vmsDir    string
	// maxBytes is a ceiling on what the fleet may occupy on disk: the golden
	// images and the machines' overlays together, because they share one
	// filesystem and it was the filesystem that filled. Zero is no ceiling, and
	// the grace period is then the only rule.
	//
	// Over the ceiling, the grace is overridden: an image that would still be
	// inside its grace is deleted anyway, oldest first, until the total fits.
	// A grace period that could fill a disk would be the bug this file was
	// written to fix, one level up.
	//
	// Only images are ever deleted to meet it. A machine's overlay is a running
	// job and belongs to the reconciler, which holds the fleet under the same
	// ceiling by not starting the machine that would cross it. The collector's
	// half of that bargain is to count what the machines have taken, so it does
	// not hand a full disk back by measuring only its own half.
	maxBytes int64
	grace    time.Duration
	now      func() time.Time
	// inspect reads a qcow2's backing chain, replaced in tests by something
	// that does not need qemu-img.
	inspect func(ctx context.Context, path string) ([]string, error)
}

// NewGC builds a collector over one state directory.
func NewGC(imagesDir, vmsDir string, maxBytes int64) *GC {
	return &GC{
		imagesDir: imagesDir,
		vmsDir:    vmsDir,
		maxBytes:  maxBytes,
		grace:     GracePeriod,
		now:       time.Now,
		inspect:   backingChain,
	}
}

// WithCeiling changes the ceiling on a collector already built.
//
// The budget is a setting somebody edits while the daemon is running, and a
// collector that had to be rebuilt to notice would be one more thing to
// remember to do.
func (g *GC) WithCeiling(maxBytes int64) *GC { g.maxBytes = maxBytes; return g }

// WithClock replaces the clock, for tests.
func (g *GC) WithClock(now func() time.Time) *GC { g.now = now; return g }

// WithGrace replaces the grace period, for tests.
func (g *GC) WithGrace(d time.Duration) *GC { g.grace = d; return g }

// WithInspector replaces the backing-chain reader, for tests.
func (g *GC) WithInspector(f func(ctx context.Context, path string) ([]string, error)) *GC {
	g.inspect = f
	return g
}

// Collected is what one pass deleted, for the log and the UI.
type Collected struct {
	Deleted []string
	Freed   int64
	// Kept is how many images survived. Total is what the fleet occupies
	// afterwards — the surviving images and the machines' overlays — which is
	// the figure the ceiling is about and the one worth logging.
	Kept  int
	Total int64
}

// Collect deletes the images no pool wants and nothing is booting.
//
// wanted is the set of image file names the pools currently ask for. It is
// passed in rather than read from the database here because an image is wanted
// by a pool's *specification*, and the one place that turns a pool into a spec
// is the builder — deciding it twice would be two answers to one question.
//
// The rule, in order:
//
//  1. An image backing a live machine is never deleted. This is read from the
//     machines' own overlays rather than from anything remembered, because a
//     disk with a backing file is the only unforgeable statement that an image
//     is in use — and after a daemon restart it is the only one left.
//  2. An image a pool asks for is never deleted, whether or not anything is
//     booting it right now.
//  3. Anything else is deleted once it has been unwanted for the grace period,
//     oldest first while the total is over the ceiling.
func (g *GC) Collect(ctx context.Context, wanted map[string]bool) (Collected, error) {
	var out Collected

	images, err := g.list()
	if err != nil {
		return out, err
	}

	inUse, err := g.InUse(ctx)
	if err != nil {
		// A chain that cannot be read is not permission to delete. Refusing
		// to collect leaves a full disk; deleting an image out from under a
		// running job destroys the job — so this stops, and says so.
		return out, fmt.Errorf("read what the machines are booting: %w", err)
	}

	// Oldest first, so that eviction over the ceiling takes the least recently
	// built rather than whatever the directory happened to list first.
	sort.Slice(images, func(i, j int) bool { return images[i].modified.Before(images[j].modified) })

	// What the machines have already taken counts against the same ceiling.
	// Measured rather than taken from the pools' promised sizes: a qcow2
	// overlay grows as its job writes, so what a pool asked for says very
	// little about what is on the disk right now.
	total := g.overlays()
	for _, image := range images {
		total += image.size
	}

	out.Total = g.overlays()

	deadline := g.now().Add(-g.grace)
	for _, image := range images {
		keep := inUse[image.name] || wanted[image.name]
		if !keep {
			// Inside its grace, and the total fits: keep it for the operator
			// who is about to change their mind.
			if image.modified.After(deadline) && (g.maxBytes == 0 || total <= g.maxBytes) {
				keep = true
			}
		}
		if keep {
			out.Kept++
			out.Total += image.size
			continue
		}
		if err := os.Remove(image.path); err != nil && !os.IsNotExist(err) {
			return out, fmt.Errorf("delete %s: %w", image.name, err)
		}
		total -= image.size
		out.Freed += image.size
		out.Deleted = append(out.Deleted, image.name)
	}
	return out, nil
}

// InUse is every image some machine on this host is booting from, read from
// the machines' own overlays.
//
// The whole chain of each overlay is collected, not just its immediate backing
// file, because a per-repository layer is itself an overlay on the base image:
// deleting the base because "no machine names it" would break every layer on
// it at once.
func (g *GC) InUse(ctx context.Context) (map[string]bool, error) {
	used := map[string]bool{}

	entries, err := os.ReadDir(g.vmsDir)
	if os.IsNotExist(err) {
		return used, nil
	}
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		disk := filepath.Join(g.vmsDir, entry.Name(), "disk.qcow2")
		if _, err := os.Stat(disk); err != nil {
			continue
		}
		chain, err := g.inspect(ctx, disk)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", disk, err)
		}
		for _, backing := range chain {
			used[filepath.Base(backing)] = true
		}
	}
	return used, nil
}

// overlays is what the machines' working directories occupy.
//
// Best effort, and deliberately so: this number decides whether the collector
// is over its ceiling, and a directory that could not be walked should make it
// collect a little less rather than refuse to collect at all.
func (g *GC) overlays() int64 {
	var total int64
	_ = filepath.WalkDir(g.vmsDir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += paths.OnDisk(info)
		}
		return nil
	})
	return total
}

// image is one golden image on disk.
type image struct {
	name     string
	path     string
	size     int64
	modified time.Time
}

// list reads the images in the directory.
//
// Only files this daemon names as images are considered. The directory also
// holds the stock cloud image every build starts from, the build working
// directory, and the build logs — deleting any of those because no pool
// "wants" it would mean fetching a few hundred megabytes again on the next
// build, or worse.
func (g *GC) list() ([]image, error) {
	entries, err := os.ReadDir(g.imagesDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []image
	for _, entry := range entries {
		if entry.IsDir() || !IsImageName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, image{
			name: entry.Name(), path: filepath.Join(g.imagesDir, entry.Name()),
			// The size on disk, not the virtual size. A qcow2 is grown to
			// twenty gigabytes and allocates a fraction of it, and a ceiling
			// set against the virtual size would be a ceiling against a number
			// nothing on the host ever occupies.
			size: paths.OnDisk(info), modified: info.ModTime(),
		})
	}
	return out, nil
}

// IsImageName reports whether a file in the images directory is one of this
// daemon's golden images, as opposed to the stock cloud image it builds them
// from or a log beside them.
func IsImageName(name string) bool {
	return strings.HasPrefix(name, "runner-") && strings.HasSuffix(name, ".qcow2")
}

// backingChain is every file a qcow2 depends on, itself included.
func backingChain(ctx context.Context, path string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "qemu-img", "info", "--backing-chain",
		"--output=json", path).Output()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info: %w", err)
	}
	var chain []struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(out, &chain); err != nil {
		return nil, fmt.Errorf("parse qemu-img info: %w", err)
	}
	names := make([]string, 0, len(chain))
	for _, link := range chain {
		names = append(names, link.Filename)
	}
	return names, nil
}
