package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

func TestConfigFromEnv(t *testing.T) {
	for key, value := range map[string]string{
		"FLEET_RUNNER":          "web-1",
		"FLEET_POOL":            "web",
		"FLEET_GENERATION":      "abc123",
		"FLEET_URL":             "https://github.com/o/r",
		"FLEET_SCOPE_KIND":      "repository",
		"FLEET_SCOPE":           "o/r",
		"FLEET_LABELS":          "vm,nestedvirt,gpu",
		"FLEET_EPHEMERAL":       "true",
		"FLEET_NESTED":          "true",
		"FLEET_CPUS":            "8",
		"FLEET_MEMORY_MB":       "16384",
		"FLEET_DISK_GB":         "100",
		"FLEET_CREDENTIAL_FILE": "/run/runner-fleet/credentials/1",
	} {
		t.Setenv(key, value)
	}

	c, err := ConfigFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Runner != "web-1" || c.Pool != "web" || c.Generation != "abc123" {
		t.Fatalf("got %+v", c)
	}
	if strings.Join(c.Labels, ",") != "vm,nestedvirt,gpu" {
		t.Fatalf("labels are %v", c.Labels)
	}
	if !c.Ephemeral || !c.Nested {
		t.Fatalf("flags are %t/%t", c.Ephemeral, c.Nested)
	}
	if c.CPUs != 8 || c.MemoryMB != 16384 || c.DiskGB != 100 {
		t.Fatalf("sizing is %d/%d/%d", c.CPUs, c.MemoryMB, c.DiskGB)
	}
	// The runtime defaults to a machine, which is the safer of the two.
	if c.Runtime != model.RuntimeVM {
		t.Fatalf("runtime is %q", c.Runtime)
	}
}

func TestConfigRequiresWhatItCannotInvent(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no name", map[string]string{"FLEET_URL": "u", "FLEET_SCOPE": "o/r"}, "runner name"},
		{"no scope", map[string]string{"FLEET_RUNNER": "web-1"}, "repository or organisation"},
		{"nothing to register with", map[string]string{
			"FLEET_RUNNER": "web-1", "FLEET_URL": "u", "FLEET_SCOPE": "o/r",
		}, "nothing to register with"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"FLEET_RUNNER", "FLEET_URL", "FLEET_SCOPE", "FLEET_CREDENTIAL_FILE"} {
				t.Setenv(key, "")
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			_, err := ConfigFromEnv("")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestTokenIsReadFreshFromTheCredentialFile(t *testing.T) {
	path := t.TempDir() + "/credential"
	if err := os.WriteFile(path, []byte("github_pat_first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Config{Runner: "web-1", CredentialFile: path}

	got, err := c.Token()
	if err != nil || got != "github_pat_first" {
		t.Fatalf("got %q, %v", got, err)
	}

	// Rotating the credential must reach the next registration without the
	// runner being rebuilt.
	if err := os.WriteFile(path, []byte("github_pat_second"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = c.Token()
	if err != nil || got != "github_pat_second" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTokenSaysWhenTheDaemonHasNotWrittenIt(t *testing.T) {
	c := Config{Runner: "web-1", CredentialFile: t.TempDir() + "/missing"}
	_, err := c.Token()
	if err == nil || !strings.Contains(err.Error(), "runner-fleetd") {
		t.Fatalf("got %v", err)
	}
}

// "-cpu host" copies the host CPU, extensions included, so a guest on a host
// with nested virtualisation enabled would see vmx or svm whether or not the
// pool asked. Off has to mean off.
func TestCPUModel(t *testing.T) {
	for _, tt := range []struct {
		vendor string
		nested bool
		want   string
	}{
		{"intel", true, "host,+vmx"},
		{"intel", false, "host,-vmx"},
		{"amd", true, "host,+svm"},
		{"amd", false, "host,-svm"},
		{"unknown", true, "host"},
		{"unknown", false, "host"},
	} {
		if got := CPUModel(tt.vendor, tt.nested); got != tt.want {
			t.Errorf("%s nested=%t gives %q, want %q", tt.vendor, tt.nested, got, tt.want)
		}
	}
}

func TestQEMUArgs(t *testing.T) {
	args := strings.Join(QEMUArgs(VMOptions{
		Name: "web-1", Disk: "/vms/web-1/disk.qcow2", Seed: "/vms/web-1/seed.iso",
		CPUs: 4, MemoryMB: 8192, SSHPort: 2222, CPUModel: "host,+vmx",
		QMPSocket: "/vms/web-1/qmp.sock", Console: "/vms/web-1/console.log",
	}), " ")

	for _, want := range []string{
		"-name web-1",
		"-cpu host,+vmx",
		"-accel kvm",
		"-smp 4",
		"-m 8192",
		"file=/vms/web-1/disk.qcow2",
		"hostfwd=tcp:127.0.0.1:2222-:22",
		// The monitor is how a drain becomes a clean shutdown rather than a
		// power cut, so it must always be there.
		"-qmp unix:/vms/web-1/qmp.sock",
		"-no-reboot",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("the command line is missing %q:\n%s", want, args)
		}
	}
}

// Guest memory is a ratchet without this, and a silent one.
//
// -m is a ceiling rather than a reservation — there is no -mem-prealloc — so a
// machine only takes what its guest touches. But nothing gives it back: a page
// the guest touches once is a host page QEMU holds until it exits, so the peak
// of the heaviest job a machine ever ran stays resident for the rest of that
// machine's life. free-page-reporting is the guest volunteering pages it has
// freed so the host can drop them.
//
// Spelled out here because losing it costs nothing that any test would notice —
// every machine still boots, every job still passes, and the host simply
// creeps.
func TestMachinesCanGiveMemoryBack(t *testing.T) {
	args := strings.Join(QEMUArgs(VMOptions{Name: "web-1", CPUs: 2, MemoryMB: 4096}), " ")

	if !strings.Contains(args, "virtio-balloon,free-page-reporting=on") {
		t.Errorf("no balloon reporting free pages, so this machine will never return "+
			"memory to the host:\n%s", args)
	}
	// And nothing that would inflate it. The balloon is here to receive what the
	// guest offers, not to squeeze a machine with a job on it — taking memory
	// from underneath a compiler is how a passing job becomes an OOM kill.
	if strings.Contains(args, "deflate-on-oom") || strings.Contains(args, "-balloon ") {
		t.Errorf("the balloon is being driven, not just listening:\n%s", args)
	}

	// A machine already running when this shipped has no balloon and cannot
	// grow one — the command line is fixed when QEMU starts. Nothing else can
	// notice: the QEMU arguments are not part of the golden image, so the old
	// machine and the new one hash to the same generation and the reconciler
	// leaves it exactly where it is. The revision is the only lever that
	// reaches this, and without it the fix arrives on an idle host never.
	if model.SpecRevision < 5 {
		t.Fatalf("spec revision %d: machines booted without a balloon are still what the"+
			" pools asked for, so nothing replaces them and this fix reaches a host that"+
			" is not busy only when somebody restarts the units by hand", model.SpecRevision)
	}
}

func TestImageNameChangesWithItsContents(t *testing.T) {
	base := ImageSpec{Variant: "default"}
	same := ImageSpec{Variant: "default"}
	if base.Name() != same.Name() {
		t.Fatal("the same spec named two images")
	}
	if !strings.HasSuffix(base.Name(), ".qcow2") {
		t.Fatalf("got %q", base.Name())
	}

	// A pool that wants more tools gets its own image rather than silently
	// reusing one without them. This is the hook per-repository images hang
	// on.
	custom := ImageSpec{Variant: "runyard", Packages: []string{"ffmpeg"}}
	if custom.Name() == base.Name() {
		t.Fatal("a different package list produced the same image name")
	}
	if !strings.Contains(custom.Name(), "runyard") {
		t.Fatalf("the variant is not in the name: %q", custom.Name())
	}
}

func TestEffectivePackages(t *testing.T) {
	spec := ImageSpec{Variant: "x", Packages: []string{"ffmpeg", "docker.io"}}
	packages := spec.EffectivePackages()

	var docker int
	for _, pkg := range packages {
		if pkg == "docker.io" {
			docker++
		}
	}
	if docker != 1 {
		t.Fatalf("docker.io appears %d times", docker)
	}
	if !contains(packages, "ffmpeg") || !contains(packages, "qemu-system-x86") {
		t.Fatalf("got %v", packages)
	}
	for i := 1; i < len(packages); i++ {
		if packages[i-1] > packages[i] {
			t.Fatalf("not sorted, so the image name would depend on argument order: %v", packages)
		}
	}
}

// What is in the image decides which jobs can run in a container pool and which
// need a machine of their own, so the packages this repository's own workflow
// depends on are named here rather than discovered by a job failing.
//
// build-essential is the load-bearing one: `go test -race` needs a C toolchain,
// and the official runner container image has none — see
// TestTheOfficialImageHasNoCToolchain in the docker executor.
func TestTheImageCarriesWhatTheCIJobsNeed(t *testing.T) {
	needed := map[string]string{
		"build-essential": "go test -race needs a C toolchain",
		"docker.io":       "the container-runner job runs real containers",
		"shellcheck":      "the installer job lints install.sh",
		"git":             "actions/checkout",
		"nodejs":          "actions written in JavaScript, and the ui job",
		"sudo":            "the installer job installs a service as root",
	}
	packages := (ImageSpec{Variant: "default"}).EffectivePackages()
	for pkg, why := range needed {
		if !contains(packages, pkg) {
			t.Errorf("%s is not in the image, and %s", pkg, why)
		}
	}
}

func TestRunUserDataCarriesTheRegistration(t *testing.T) {
	c := Config{
		Runner: "web-1", URL: "https://github.com/o/r", Labels: []string{"vm", "gpu"},
		Group: "Default", Ephemeral: true,
	}
	data := runUserData(c, Registration{Token: "AAAA-registration-token"},
		"ssh-ed25519 KEY runner-fleet")

	for _, want := range []string{
		"hostname: web-1",
		`GITHUB_URL="https://github.com/o/r"`,
		`RUNNER_TOKEN="AAAA-registration-token"`,
		`RUNNER_NAME="web-1"`,
		`RUNNER_LABELS="vm,gpu"`,
		"EPHEMERAL=true",
		// The inside half of a drain: the runner finishes its job before the
		// machine stops.
		"KillSignal=SIGTERM",
		"TimeoutStopSec=3h",
		"ExecStopPost=+/usr/bin/systemctl poweroff --no-block",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("the machine's configuration is missing %q", want)
		}
	}
}

// The token is 0600 and root-owned inside the machine: it is short-lived, but
// a job has no reason to be able to read it.
func TestTheRegistrationTokenIsNotReadableByTheJob(t *testing.T) {
	data := runUserData(Config{Runner: "web-1", URL: "u"}, Registration{Token: "token"}, "key")
	i := strings.Index(data, "/etc/runner-fleet/runner.env")
	if i < 0 {
		t.Fatal("no runner environment file")
	}
	window := data[i : i+200]
	if !strings.Contains(window, "permissions: '0600'") || !strings.Contains(window, "owner: 'root:root'") {
		t.Fatalf("the registration token is not protected:\n%s", window)
	}
}

// The registration a machine gets is minted on the host, so the guest never
// holds anything that could administer a repository — and with a just-in-time
// configuration there is no registration step in there at all.
func TestRunUserDataCarriesAJustInTimeConfiguration(t *testing.T) {
	c := Config{
		Runner: "web-1", URL: "https://github.com/o/r", Labels: []string{"vm"},
		Ephemeral: true,
	}
	data := runUserData(c, Registration{JIT: "BASE64-JIT-CONFIG"}, "ssh-ed25519 KEY")

	if !strings.Contains(data, `RUNNER_JITCONFIG="BASE64-JIT-CONFIG"`) {
		t.Error("the machine was not given its configuration")
	}
	// Empty rather than absent: the guest picks whichever it was given, and an
	// empty one is how it knows which that is.
	if !strings.Contains(data, `RUNNER_TOKEN=""`) {
		t.Error("a registration token was minted for a just-in-time runner as well")
	}
}

// A just-in-time configuration is spent by the job it takes, so the guest must
// use it directly rather than running config.sh with it — and it must prefer it
// over the older path when it has both.
func TestGuestScriptPrefersAJustInTimeConfiguration(t *testing.T) {
	for _, want := range []string{
		`if [[ -n "${RUNNER_JITCONFIG:-}" ]]; then`,
		`exec ./run.sh --jitconfig "$RUNNER_JITCONFIG"`,
	} {
		if !strings.Contains(GuestRunnerScript, want) {
			t.Errorf("the guest script is missing %q", want)
		}
	}
	jit := strings.Index(GuestRunnerScript, "--jitconfig")
	config := strings.Index(GuestRunnerScript, "./config.sh")
	if jit < 0 || config < 0 || jit > config {
		t.Error("the guest registers the old way before looking at the configuration it was given")
	}
}

func TestGuestScriptHandlesEphemeralAndLabels(t *testing.T) {
	for _, want := range []string{
		`[[ "$EPHEMERAL" == "true" ]] && args+=(--ephemeral)`,
		`args+=(--labels "$RUNNER_LABELS")`,
		"--replace",
		// exec, so the runner receives the unit's SIGTERM directly rather than
		// through a shell that would swallow it.
		"exec ./run.sh",
	} {
		if !strings.Contains(GuestRunnerScript, want) {
			t.Errorf("the guest script is missing %q", want)
		}
	}
}

func TestBuildUserDataInstallsTheRunner(t *testing.T) {
	data := buildUserData(ImageSpec{Variant: "default"}, "ssh-ed25519 KEY")
	for _, want := range []string{
		"actions-runner-linux",
		RunnerVersion,
		"usermod -aG docker runner",
		"usermod -aG kvm runner",
		// Unattended upgrades would fight a job for the package lock and could
		// reboot the machine underneath it.
		"unattended-upgrades",
		"systemctl poweroff --no-block",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("the build configuration is missing %q", want)
		}
	}
}

func TestQuoteKeepsAValueOnOneLine(t *testing.T) {
	if got := quote("a\nb"); strings.Contains(got, "\n") {
		t.Fatalf("got %q", got)
	}
	if got := quote(`say "hi"`); got != `"say \"hi\""` {
		t.Fatalf("got %q", got)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// The agent authenticates on its own, so a runner can come back after a reboot
// with the daemon still starting. For an app that means signing an assertion,
// which needs the app id beside the key.
func TestConfigReadsAppCredentials(t *testing.T) {
	for key, value := range map[string]string{
		"FLEET_RUNNER":          "web-1",
		"FLEET_URL":             "https://github.com/o/r",
		"FLEET_SCOPE":           "o/r",
		"FLEET_CREDENTIAL_FILE": "/run/runner-fleet/credentials/1",
		"FLEET_CREDENTIAL_KIND": "app",
		"FLEET_APP_ID":          "123456",
		"FLEET_INSTALLATION_ID": "42",
	} {
		t.Setenv(key, value)
	}

	c, err := ConfigFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if c.CredentialKind != model.CredentialApp || c.AppID != 123456 || c.InstallationID != 42 {
		t.Fatalf("got %+v", c)
	}
}

func TestAnAppWithoutAnAppIDIsRefused(t *testing.T) {
	for key, value := range map[string]string{
		"FLEET_RUNNER":          "web-1",
		"FLEET_URL":             "https://github.com/o/r",
		"FLEET_SCOPE":           "o/r",
		"FLEET_CREDENTIAL_FILE": "/run/x",
		"FLEET_CREDENTIAL_KIND": "app",
		"FLEET_APP_ID":          "",
	} {
		t.Setenv(key, value)
	}
	_, err := ConfigFromEnv("")
	if err == nil || !strings.Contains(err.Error(), "app id") {
		t.Fatalf("got %v", err)
	}
}

// A runner with no credential kind is a token one, which is what every runner
// created before apps existed will report.
func TestConfigDefaultsToATokenCredential(t *testing.T) {
	for key, value := range map[string]string{
		"FLEET_RUNNER":          "web-1",
		"FLEET_URL":             "https://github.com/o/r",
		"FLEET_SCOPE":           "o/r",
		"FLEET_CREDENTIAL_FILE": "/run/x",
		"FLEET_CREDENTIAL_KIND": "",
	} {
		t.Setenv(key, value)
	}
	c, err := ConfigFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if c.CredentialKind != model.CredentialPAT {
		t.Fatalf("got %q", c.CredentialKind)
	}
}

// The official runner image puts the runner in the home directory; others put
// it in a subdirectory. Looking for config.sh rather than assuming a path is
// what makes a custom image work, and it is what the first container pool ran
// into: the agent looked in /home/runner/actions-runner and the image had it
// in /home/runner.
func TestFindRunnerHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_RUNNER_HOME", filepath.Join(dir, "somewhere-else"))
	got, err := findRunnerHome()
	if err != nil || got != filepath.Join(dir, "somewhere-else") {
		t.Fatalf("an override was ignored: %q, %v", got, err)
	}
}

func TestFindRunnerHomeSaysWhereItLooked(t *testing.T) {
	t.Setenv("FLEET_RUNNER_HOME", "")
	_, err := findRunnerHome()
	if err == nil {
		t.Skip("this host happens to have a runner installed in one of the usual places")
	}
	for _, want := range []string{"/home/runner", "config.sh", "FLEET_RUNNER_HOME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q: %v", want, err)
		}
	}
}

// A container is given a token by the daemon and no credential file, which is
// the whole point — and it has to be enough on its own. It was not, and a
// container that reached this point died saying it had no credential.
func TestAMintedTokenIsEnoughOnItsOwn(t *testing.T) {
	for key, value := range map[string]string{
		"FLEET_RUNNER":             "api-1",
		"FLEET_URL":                "https://github.com/o/r",
		"FLEET_SCOPE":              "o/r",
		"FLEET_RUNTIME":            "container",
		"FLEET_CREDENTIAL_FILE":    "",
		"FLEET_REGISTRATION_TOKEN": "AAAA-registration",
	} {
		t.Setenv(key, value)
	}

	c, err := ConfigFromEnv("")
	if err != nil {
		t.Fatalf("a container with a minted token was refused: %v", err)
	}
	if got := c.RegistrationToken(); got != "AAAA-registration" {
		t.Fatalf("got %q", got)
	}
	// Everything this process starts inherits its environment, and the job is
	// one of them.
	if left := os.Getenv("FLEET_REGISTRATION_TOKEN"); left != "" {
		t.Fatalf("the token is still in the environment: %q", left)
	}
}

// A machine has no minted token and mints for itself, which is what lets it
// come back after a reboot with the daemon still down.
func TestAMachineHasNoMintedToken(t *testing.T) {
	for key, value := range map[string]string{
		"FLEET_RUNNER":             "web-1",
		"FLEET_URL":                "https://github.com/o/r",
		"FLEET_SCOPE":              "o/r",
		"FLEET_CREDENTIAL_FILE":    "/run/runner-fleet/credentials/1",
		"FLEET_REGISTRATION_TOKEN": "",
	} {
		t.Setenv(key, value)
	}
	c, err := ConfigFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RegistrationToken(); got != "" {
		t.Fatalf("got %q", got)
	}
}

// The recipe is part of what the image is, not a step that runs on top of one.
// An image built from a different recipe is a different image, and one built
// from the same recipe is the same image — which is what makes editing the
// field rebuild once rather than every pass, and leaving it alone rebuild
// nothing.
func TestTheImageNameCoversTheRecipe(t *testing.T) {
	plain := ImageSpec{Variant: "default"}
	withRecipe := ImageSpec{Variant: "default", Recipe: "echo hello\n"}
	edited := ImageSpec{Variant: "default", Recipe: "echo goodbye\n"}

	if withRecipe.Name() == plain.Name() {
		t.Fatal("a recipe left the image called what it was called without one")
	}
	if withRecipe.Name() == edited.Name() {
		t.Fatal("two different recipes named the same image")
	}
	if withRecipe.Name() != (ImageSpec{Variant: "default", Recipe: "echo hello\n"}).Name() {
		t.Fatal("the same recipe named two images")
	}
	// And a pool that has no recipe is left exactly where it was, so upgrading
	// the daemon does not rebuild an image for a fleet that changed nothing.
	if plain.Name() != (ImageSpec{Variant: "default", Recipe: ""}).Name() {
		t.Fatal("an empty recipe is not the same as no recipe")
	}
}

func TestBuildUserDataRunsThePoolsRecipe(t *testing.T) {
	data := buildUserData(ImageSpec{Variant: "runyard", Recipe: "install-the-toolchain\n"}, "ssh-ed25519 KEY")

	for _, want := range []string{
		"path: " + recipePath,
		"install-the-toolchain",
		// Run by the build script, after the base provisioning.
		"if [ -x " + recipePath + " ]",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("the build configuration is missing %q:\n%s", want, data)
		}
	}

	// And the order: a recipe that ran before the packages were in would be a
	// recipe that cannot use them.
	provisioning := strings.Index(data, "/usr/local/bin/provision.sh\n")
	recipe := strings.Index(data, "if [ -x "+recipePath+" ]")
	if provisioning < 0 || recipe < 0 || recipe < provisioning {
		t.Error("the recipe does not run after the base provisioning")
	}
}

// A pool with no recipe must produce a document with no recipe in it, rather
// than an empty file the build script would try to run.
func TestBuildUserDataWithoutARecipe(t *testing.T) {
	data := buildUserData(ImageSpec{Variant: "default"}, "ssh-ed25519 KEY")
	if strings.Contains(data, "path: "+recipePath) {
		t.Errorf("an empty recipe was written into the image anyway:\n%s", data)
	}
}

// The failure this exists for: a script that exits non-zero never reaches the
// power-off that tells the host the build is over, so the host waits on a
// machine that is already dead until the stale-lock timer fires. Ours changed
// twice a year and could be read; a recipe is somebody else's shell.
func TestTheBuildPowersOffEvenWhenItFails(t *testing.T) {
	script := buildScript()

	for _, want := range []string{"trap finish EXIT", "systemctl poweroff --no-block", "set -euo pipefail"} {
		if !strings.Contains(script, want) {
			t.Errorf("the build script is missing %q", want)
		}
	}
	// The power-off has to be in the trap. At the end of the script it is
	// exactly what it was before: unreachable on the path that needs it.
	trap := strings.Index(script, "finish() {")
	poweroff := strings.Index(script, "systemctl poweroff")
	closing := strings.Index(script, "trap finish EXIT")
	if trap < 0 || poweroff < trap || poweroff > closing {
		t.Error("the power-off is outside the trap, so a failed build still hangs")
	}
	// And it says which it was, on the console, which is all the host can see.
	for _, marker := range []string{ImageReadyMarker, ImageFailedMarker} {
		if !strings.Contains(script, marker) {
			t.Errorf("the build never says %q, so the host cannot tell what happened", marker)
		}
	}
}

// A guest that powered off is not a guest that succeeded: it powers off when
// its recipe fails too. Publishing an image on the strength of a clean exit is
// how a half-provisioned disk becomes what every job in the pool boots.
func TestABuildIsOnlyDoneWhenItSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  string
		want bool
	}{
		{"finished", "cloud-init ran\n" + ImageReadyMarker + "\n", true},
		{"failed", "cloud-init ran\n" + ImageFailedMarker + "\nthe image build failed with status 1\n", false},
		{"stopped saying nothing", "cloud-init ran\n", false},
		{"no console at all", "", false},
	} {
		if got := buildSucceeded([]byte(tc.log)); got != tc.want {
			t.Errorf("%s: buildSucceeded = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The log is what somebody watching a build reads, and it has to be the same
// file that is kept afterwards: a console copied in only once the build has
// failed is a console nobody could have watched.
func TestTheConsoleReachesTheLogWhileTheBuildIsStillRunning(t *testing.T) {
	console := filepath.Join(t.TempDir(), "console.log")
	var log lockedBuffer

	stop := followConsole(console, &log)
	if err := os.WriteFile(console, []byte("cloud-init running\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Before the build ends, which is the whole point.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "cloud-init running") {
		if time.Now().After(deadline) {
			t.Fatalf("nothing reached the log while the build was running: %q", log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And the last thing the machine said before it powered off is not lost to
	// the tick the copier never got to.
	f, err := os.OpenFile(console, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(ImageReadyMarker + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	whole := stop()
	if !strings.Contains(string(whole), ImageReadyMarker) {
		t.Errorf("the last words of the build were lost: %q", whole)
	}
	if !strings.Contains(log.String(), ImageReadyMarker) {
		t.Errorf("the log ends before the build did: %q", log.String())
	}
}

// A build that cannot start still has to say so in the log, because the log is
// the only thing a person opens.
func TestAFailedBuildSaysSoInItsLog(t *testing.T) {
	var log lockedBuffer

	// Cancelled before it starts, so this fails on the first command it runs
	// rather than downloading an operating system.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := BuildImage(ctx, BuildOptions{
		Spec:      ImageSpec{Variant: "default", Recipe: "echo hello\n"},
		ImagesDir: t.TempDir(),
		PublicKey: "ssh-ed25519 KEY",
		Journal:   &log,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		t.Fatal("a build with a cancelled context reported success")
	}
	if !strings.Contains(log.String(), "building runner-") {
		t.Errorf("the log does not say what was being built: %q", log.String())
	}
}

// A runner does not build its own image. The daemon builds it first, and a
// machine that finds one missing says which one and stops — the alternative is
// a unit rebuilding a broken recipe every two seconds.
func TestARunnerRefusesToStartWithoutItsImage(t *testing.T) {
	state := t.TempDir()
	err := runVM(context.Background(), Config{
		Runner: "web-1", Pool: "web", StateDir: state,
		Image: "default", Recipe: "echo hello\n",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !errors.Is(err, ErrImageNotBuilt) {
		t.Fatalf("a runner with no image failed with %v", err)
	}
	// Named, because "an image is missing" on a host with six of them is not
	// something anybody can act on.
	want := ImageSpec{Variant: "default", Recipe: "echo hello\n"}.Name()
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the error does not name the image it wanted: %v", err)
	}
}

// lockedBuffer is a journal a test can read while a build is writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestConfigCarriesWhatTheImageBakesIn(t *testing.T) {
	recipe := "#!/usr/bin/env bash\nset -euo pipefail\ninstall-the-toolchain\n"
	for key, value := range map[string]string{
		"FLEET_RUNNER":          "web-1",
		"FLEET_URL":             "https://github.com/o/r",
		"FLEET_SCOPE":           "o/r",
		"FLEET_CREDENTIAL_FILE": "/run/runner-fleet/credentials/1",
		"FLEET_PACKAGES":        "nftables,conntrack",
		"FLEET_RECIPE_BASE64":   base64.StdEncoding.EncodeToString([]byte(recipe)),
	} {
		t.Setenv(key, value)
	}

	c, err := ConfigFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Packages, ",") != "nftables,conntrack" {
		t.Fatalf("packages are %v", c.Packages)
	}
	if c.Recipe != recipe {
		t.Fatalf("the recipe arrived as %q", c.Recipe)
	}
}

// A recipe that cannot be decoded is an error, not an empty recipe. Carrying
// on would build an image missing everything the pool asked to bake in, boot
// it, and run jobs on it — green until the first job that needed what is not
// there, by which time nobody is looking at this.
func TestAnUndecodableRecipeStopsTheRunner(t *testing.T) {
	for key, value := range map[string]string{
		"FLEET_RUNNER":          "web-1",
		"FLEET_URL":             "https://github.com/o/r",
		"FLEET_SCOPE":           "o/r",
		"FLEET_CREDENTIAL_FILE": "/run/runner-fleet/credentials/1",
		"FLEET_RECIPE_BASE64":   "this is not base64 !!",
	} {
		t.Setenv(key, value)
	}

	if _, err := ConfigFromEnv(""); err == nil {
		t.Fatal("a runner started with a recipe it could not read, and would have built an image without it")
	}
}
