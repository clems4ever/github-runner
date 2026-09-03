package agent

import (
	"context"
	"fmt"
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
// boots is three deep: golden image, repository layer, the machine's own disk.
// QEMU is the only thing whose opinion of that counts, so this boots it.
//
// What it looks for inside is one thing from each level — the runner from the
// image, the packages from the layer, the file the repository's recipe wrote —
// because a chain that boots but has lost a level is exactly the failure this
// is for. A job would find that out halfway through.
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	work := t.TempDir()
	disk := filepath.Join(work, "disk.qcow2")
	// The backing file is named absolutely, which is what the agent does: the
	// name is stored inside the qcow2 and resolved against wherever the
	// overlay is, and the overlay is in the machine's own directory rather
	// than next to the images.
	if err := runIn(ctx, work, "qemu-img", "create", "-q", "-f", "qcow2",
		"-F", "qcow2", "-b", layer.Path(dir), disk); err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(ctx, "qemu-img", "info", "--backing-chain",
		"--output=json", disk).Output()
	if err != nil {
		t.Fatal(err)
	}
	// The collector reads exactly this to decide what it may delete, so it is
	// asked the same question here rather than trusted to agree.
	for _, want := range []string{layer.Name(), base.Name()} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("%s is not in the chain a machine boots:\n%s", want, out)
		}
	}

	seed := filepath.Join(work, "seed.iso")
	if err := makeSeed(ctx, chainCheckUserData(liveKey(t, dir)),
		metaData("runner-fleet-chain", "chain-check-1"), seed); err != nil {
		t.Fatal(err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(work, "console.log")
	cmd, err := bootVM(ctx, VMOptions{
		Name: "runner-fleet-chain", Dir: work, Disk: disk, Seed: seed,
		CPUs: 2, MemoryMB: 2048, SSHPort: port,
		CPUModel:  CPUModel(CPUVendor(), false),
		QMPSocket: filepath.Join(work, "qmp.sock"),
		Console:   console,
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := followConsole(console, os.Stdout)
	waitErr := cmd.Wait()
	said := string(stop())

	if ctx.Err() != nil {
		t.Fatalf("the machine never finished: %v", ctx.Err())
	}
	if waitErr != nil && ignoreCleanExit(waitErr) != nil {
		t.Fatalf("the machine failed: %v", waitErr)
	}
	if !strings.Contains(said, chainOKMarker) {
		t.Fatalf("the machine did not report a whole chain; its console said:\n%s",
			lastOf(said, 4000))
	}
	t.Log("booted a three-deep chain with every level intact")
}

// chainOKMarker is how the machine says every level of its chain is there. The
// console is the only channel out of a machine nothing has logged into.
const chainOKMarker = "runner-fleet: chain ok"

// chainCheckUserData boots the machine, looks for one thing from each level of
// the chain, says what it found, and switches the machine off.
func chainCheckUserData(publicKey string) string {
	check := `#!/usr/bin/env bash
set -uo pipefail
fail=0
say() { echo "runner-fleet: $*" > /dev/console; }

# From the golden image: the Actions runner, and the user it runs as.
[ -x /home/runner/actions-runner/run.sh ] || { say "MISSING the actions runner"; fail=1; }
id runner >/dev/null 2>&1 || { say "MISSING the runner user"; fail=1; }

# From the layer: the packages the repository asked for.
for tool in jq sqlite3; do
  command -v "$tool" >/dev/null 2>&1 || { say "MISSING $tool"; fail=1; }
done

# From the layer's recipe: the file it wrote.
[ -f /etc/runner-fleet-layer-test ] || { say "MISSING what the recipe wrote"; fail=1; }

if [ "$fail" -eq 0 ]; then say "chain ok"; else say "chain broken"; fi
systemctl poweroff
`
	return fmt.Sprintf(`#cloud-config
hostname: runner-fleet-chain

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s

write_files:
  - path: /usr/local/bin/chain-check.sh
    permissions: '0755'
    owner: 'root:root'
    content: |
%s

runcmd:
  - [ /usr/local/bin/chain-check.sh ]
`, publicKey, indent(check, "      "))
}

// lastOf is the end of a console, for a failure message that has to be read.
func lastOf(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}
