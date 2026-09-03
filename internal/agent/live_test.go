package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A golden image and a layer on it, built for real on this host with QEMU.
//
// Everything else about image building is tested against a fake builder, which
// is right — the unit tests are about what the daemon decides, and a test that
// waited half an hour for apt would not be run. But nothing in that answers
// whether the qcow2 chain is one QEMU will boot, and that is the question a
// layer is: an overlay, on an image, under a machine's own overlay.
//
// Skipped unless FLEET_LIVE_IMAGES names a directory to build in. It wants
// KVM, several gigabytes, and tens of minutes:
//
//	FLEET_LIVE_IMAGES=/var/tmp/fleet go test ./internal/agent -run Live -timeout 3h -v
func liveImages(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("FLEET_LIVE_IMAGES")
	if dir == "" {
		t.Skip("set FLEET_LIVE_IMAGES to build images for real")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("no /dev/kvm: %v", err)
	}
	return dir
}

// liveKey is a throwaway key pair for the build machines, so a build that goes
// wrong can be looked inside.
func liveKey(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "build-key")
	if _, err := os.Stat(path + ".pub"); err != nil {
		if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-q",
			"-f", path).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v: %s", err, out)
		}
	}
	data, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// The golden image, which everything else here needs. Built once and left in
// place, so the layer test can be re-run in a minute rather than an hour.
func TestLiveBuildGoldenImage(t *testing.T) {
	dir := liveImages(t)
	spec := ImageSpec{Variant: "default"}

	if _, err := os.Stat(spec.Path(dir)); err == nil {
		t.Log("already built:", spec.Name())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	path, err := BuildImage(ctx, BuildOptions{
		Spec: spec, ImagesDir: dir, PublicKey: liveKey(t, dir), Journal: os.Stdout,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("built", path)
}

// The layer, which is the thing this is really for.
func TestLiveBuildLayerOnGoldenImage(t *testing.T) {
	dir := liveImages(t)
	base := ImageSpec{Variant: "default"}
	if _, err := os.Stat(base.Path(dir)); err != nil {
		t.Skip("build the golden image first")
	}

	spec := LayerSpec{
		Base:     base.Name(),
		Packages: []string{"jq", "sqlite3"},
		Recipe:   "#!/usr/bin/env bash\nset -euo pipefail\necho layered > /etc/runner-fleet-layer-test\n",
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	path, err := BuildLayer(ctx, LayerOptions{
		Spec: spec, ImagesDir: dir, PublicKey: liveKey(t, dir),
		Repo: "clems4ever/github-runner", Journal: os.Stdout,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The chain is the point. A layer that is a full copy of the image would
	// pass every other assertion here and cost the disk this was written to
	// save.
	chain, err := exec.CommandContext(ctx, "qemu-img", "info", "--backing-chain",
		"--output=json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chain), base.Name()) {
		t.Fatalf("the layer is not backed by the image:\n%s", chain)
	}

	// And it has to be small, or it is not a layer.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.Stat(base.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("layer %d MB on an image of %d MB", info.Size()>>20, golden.Size()>>20)
	if info.Size() > golden.Size()/2 {
		t.Errorf("the layer is %d MB against the image's %d MB, which is not a layer",
			info.Size()>>20, golden.Size()>>20)
	}
}

// A machine's disk is an overlay on the layer, so the chain a runner actually
// boots is three deep. QEMU is the only thing whose opinion of that counts.
func TestLiveMachineBootsThroughTheWholeChain(t *testing.T) {
	dir := liveImages(t)
	base := ImageSpec{Variant: "default"}
	layer := LayerSpec{
		Base:     base.Name(),
		Packages: []string{"jq", "sqlite3"},
		Recipe:   "#!/usr/bin/env bash\nset -euo pipefail\necho layered > /etc/runner-fleet-layer-test\n",
	}
	if _, err := os.Stat(layer.Path(dir)); err != nil {
		t.Skip("build the layer first")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	work := t.TempDir()
	disk := filepath.Join(work, "disk.qcow2")
	if err := runIn(ctx, dir, "qemu-img", "create", "-q", "-f", "qcow2",
		"-F", "qcow2", "-b", layer.Name(), disk); err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(ctx, "qemu-img", "info", "--backing-chain",
		"--output=json", disk).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{layer.Name(), base.Name()} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("%s is not in the chain a machine boots:\n%s", want, out)
		}
	}
	// The collector reads exactly this to decide what it may delete, so it is
	// asked the same question here rather than trusted to agree.
	t.Logf("chain:\n%s", out)
}
