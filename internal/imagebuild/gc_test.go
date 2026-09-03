package imagebuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// host builds an images directory and a machines directory to collect over.
type host struct {
	t         *testing.T
	imagesDir string
	vmsDir    string
	// chains is what each machine's disk is backed by, keyed by machine name.
	chains map[string][]string
}

func newHost(t *testing.T) *host {
	t.Helper()
	root := t.TempDir()
	h := &host{
		t:         t,
		imagesDir: filepath.Join(root, "images"),
		vmsDir:    filepath.Join(root, "vms"),
		chains:    map[string][]string{},
	}
	for _, dir := range []string{h.imagesDir, h.vmsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// image writes an image of a given size, last modified at a given age.
func (h *host) image(name string, size int64, age time.Duration) string {
	h.t.Helper()
	path := filepath.Join(h.imagesDir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		h.t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		h.t.Fatal(err)
	}
	return path
}

// machine puts a running machine on the host, booting the given chain.
func (h *host) machine(name string, chain ...string) {
	h.t.Helper()
	dir := filepath.Join(h.vmsDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.qcow2"), []byte("overlay"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	full := make([]string, 0, len(chain))
	for _, link := range chain {
		full = append(full, filepath.Join(h.imagesDir, link))
	}
	h.chains[name] = full
}

func (h *host) gc(maxBytes int64) *GC {
	return NewGC(h.imagesDir, h.vmsDir, maxBytes).WithInspector(
		func(_ context.Context, disk string) ([]string, error) {
			return h.chains[filepath.Base(filepath.Dir(disk))], nil
		})
}

func (h *host) exists(name string) bool {
	_, err := os.Stat(filepath.Join(h.imagesDir, name))
	return err == nil
}

func names(images []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range images {
		out[name] = true
	}
	return out
}

// The whole point: an image nothing wants and nothing boots goes, once it is
// past its grace.
func TestCollectDeletesWhatNothingWants(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-default-aaaaaaaaaaaa.qcow2", 1000, 48*time.Hour)

	got, err := h.gc(0).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 1 {
		t.Fatalf("deleted %v, want the one unwanted image", got.Deleted)
	}
	if h.exists("runner-noble-default-aaaaaaaaaaaa.qcow2") {
		t.Fatal("the image is still there")
	}
}

// A pool asking for an image keeps it, whether or not anything is booting it.
// Enabling a pool ahead of time and building its image is a supported thing to
// do, and collecting the result an hour later would undo it.
func TestCollectKeepsWhatAPoolAsksFor(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-default-aaaaaaaaaaaa.qcow2", 1000, 48*time.Hour)

	got, err := h.gc(0).Collect(context.Background(),
		names([]string{"runner-noble-default-aaaaaaaaaaaa.qcow2"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 0 {
		t.Fatalf("deleted %v, want nothing", got.Deleted)
	}
}

// The rule that matters most: deleting an image a job is running on destroys
// the job. A pool deleted mid-job leaves exactly this — an image no pool wants
// and a machine still booting it.
func TestCollectNeverDeletesWhatAMachineIsBooting(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-default-aaaaaaaaaaaa.qcow2", 1000, 90*24*time.Hour)
	h.machine("web-1", "runner-noble-default-aaaaaaaaaaaa.qcow2")

	got, err := h.gc(0).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 0 {
		t.Fatalf("deleted %v while a machine was booting it", got.Deleted)
	}
}

// A per-repository layer is an overlay on a base image, and the machine only
// names the layer. Collecting the base because nothing named it directly would
// break every layer on the host at once.
func TestCollectKeepsTheWholeBackingChain(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-base-aaaaaaaaaaaa.qcow2", 1000, 90*24*time.Hour)
	h.image("runner-noble-repo-bbbbbbbbbbbb.qcow2", 100, 90*24*time.Hour)
	h.machine("web-1",
		"runner-noble-repo-bbbbbbbbbbbb.qcow2", "runner-noble-base-aaaaaaaaaaaa.qcow2")

	got, err := h.gc(0).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 0 {
		t.Fatalf("deleted %v from a live backing chain", got.Deleted)
	}
}

// The grace is what makes editing a recipe and changing your mind cheap.
func TestCollectKeepsARecentImageThroughItsGrace(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-default-aaaaaaaaaaaa.qcow2", 1000, time.Hour)

	got, err := h.gc(0).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 0 {
		t.Fatalf("deleted %v inside its grace", got.Deleted)
	}
}

// A grace period that could fill a disk would be the bug this was written to
// fix. Over the ceiling, the oldest unwanted images go regardless of grace —
// and only as many as it takes to fit.
func TestCollectEvictsPastTheCeilingOldestFirst(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-default-oldest000000.qcow2", 4096, 3*time.Hour)
	h.image("runner-noble-default-middle000000.qcow2", 4096, 2*time.Hour)
	h.image("runner-noble-default-newest000000.qcow2", 4096, time.Hour)

	// Room for two of the three.
	got, err := h.gc(9000).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 1 || got.Deleted[0] != "runner-noble-default-oldest000000.qcow2" {
		t.Fatalf("deleted %v, want just the oldest", got.Deleted)
	}
	if !h.exists("runner-noble-default-newest000000.qcow2") ||
		!h.exists("runner-noble-default-middle000000.qcow2") {
		t.Fatal("evicted more than it had to")
	}
}

// Over the ceiling and every image is in use is not a licence to delete one.
// The fleet is over its disk budget and something has to say so, but not by
// destroying a running job.
func TestCollectRespectsUseOverTheCeiling(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-default-aaaaaaaaaaaa.qcow2", 8192, 90*24*time.Hour)
	h.machine("web-1", "runner-noble-default-aaaaaaaaaaaa.qcow2")

	got, err := h.gc(1000).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 0 {
		t.Fatalf("deleted %v that a machine was booting, to meet a ceiling", got.Deleted)
	}
	if got.Total < 8192 {
		t.Fatalf("total %d, want the image it could not collect counted", got.Total)
	}
}

// The stock cloud image every build starts from lives in the same directory
// and is not a golden image. Deleting it costs a few hundred megabytes of
// download on the next build.
func TestCollectLeavesTheStockCloudImageAlone(t *testing.T) {
	h := newHost(t)
	h.image("cloud-noble.img", 5000, 90*24*time.Hour)
	h.image("runner-noble-default-aaaaaaaaaaaa.qcow2", 1000, 90*24*time.Hour)

	if _, err := h.gc(0).Collect(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !h.exists("cloud-noble.img") {
		t.Fatal("deleted the stock cloud image")
	}
}

// A chain that cannot be read is not permission to delete. The alternative is
// deleting an image out from under a running job because qemu-img was missing.
func TestCollectStopsWhenAChainCannotBeRead(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-default-aaaaaaaaaaaa.qcow2", 1000, 90*24*time.Hour)
	h.machine("web-1", "runner-noble-default-aaaaaaaaaaaa.qcow2")

	gc := NewGC(h.imagesDir, h.vmsDir, 0).WithInspector(
		func(context.Context, string) ([]string, error) {
			return nil, os.ErrPermission
		})

	if _, err := gc.Collect(context.Background(), nil); err == nil {
		t.Fatal("collected anyway")
	}
	if !h.exists("runner-noble-default-aaaaaaaaaaaa.qcow2") {
		t.Fatal("deleted an image without being able to read what was booting it")
	}
}

// The ceiling is about a filesystem, and the machines are on the same one. A
// collector that measured only its own images would report a fleet as
// comfortably inside a budget the machines had already spent.
func TestCollectCountsTheMachinesTowardTheCeiling(t *testing.T) {
	h := newHost(t)
	h.image("runner-noble-default-aaaaaaaaaaaa.qcow2", 4096, time.Hour)

	// Inside its grace, and the images alone fit under the ceiling.
	got, err := h.gc(16384).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 0 {
		t.Fatalf("deleted %v while the fleet was inside its ceiling", got.Deleted)
	}

	// Now a machine takes the rest of it. The image is still inside its grace,
	// but the grace does not survive a full disk.
	h.machine("web-1")
	if err := os.WriteFile(
		filepath.Join(h.vmsDir, "web-1", "disk.qcow2"), make([]byte, 16384), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err = h.gc(16384).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deleted) != 1 {
		t.Fatalf("deleted %v, want the image evicted for the machine", got.Deleted)
	}
}
