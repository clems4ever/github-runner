package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		"FLEET_LABELS":          "vm,nested,gpu",
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
	if strings.Join(c.Labels, ",") != "vm,nested,gpu" {
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
		{"no credential", map[string]string{
			"FLEET_RUNNER": "web-1", "FLEET_URL": "u", "FLEET_SCOPE": "o/r",
		}, "credential"},
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

func TestRunUserDataCarriesTheRegistration(t *testing.T) {
	c := Config{
		Runner: "web-1", URL: "https://github.com/o/r", Labels: []string{"vm", "gpu"},
		Group: "Default", Ephemeral: true,
	}
	data := runUserData(c, "AAAA-registration-token", "ssh-ed25519 KEY runner-fleet")

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
	data := runUserData(Config{Runner: "web-1", URL: "u"}, "token", "key")
	i := strings.Index(data, "/etc/runner-fleet/runner.env")
	if i < 0 {
		t.Fatal("no runner environment file")
	}
	window := data[i : i+200]
	if !strings.Contains(window, "permissions: '0600'") || !strings.Contains(window, "owner: 'root:root'") {
		t.Fatalf("the registration token is not protected:\n%s", window)
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

// A container is handed a token by the daemon rather than a credential of its
// own, and must not leave it lying in the environment for the job to read.
func TestTheMintedTokenIsTakenOutOfTheEnvironment(t *testing.T) {
	t.Setenv("FLEET_REGISTRATION_TOKEN", "AAAA-registration")
	c := Config{Runner: "api-1"}

	if got := c.RegistrationToken(); got != "AAAA-registration" {
		t.Fatalf("got %q", got)
	}
	// Everything this process starts inherits its environment, and the job is
	// one of them.
	if left := os.Getenv("FLEET_REGISTRATION_TOKEN"); left != "" {
		t.Fatalf("the token is still in the environment: %q", left)
	}
	if again := c.RegistrationToken(); again != "" {
		t.Fatalf("it came back: %q", again)
	}
}

// A machine has no minted token and mints for itself, which is what lets it
// come back after a reboot with the daemon still down.
func TestAMachineHasNoMintedToken(t *testing.T) {
	t.Setenv("FLEET_REGISTRATION_TOKEN", "")
	c := Config{Runner: "web-1"}
	if got := c.RegistrationToken(); got != "" {
		t.Fatalf("got %q", got)
	}
}
