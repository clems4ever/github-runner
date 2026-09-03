package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// CPUModel is the -cpu argument for a machine.
//
// "-cpu host" copies the host CPU, virtualisation extensions included, so on a
// host with nested virtualisation enabled a guest would see vmx or svm whether
// or not anyone asked for it. Both cases are therefore spelled out: the flag is
// added when the pool asked for it, which also makes a host that cannot do it
// fail at boot rather than surface as a mysteriously broken job, and masked
// when it did not, so that off means off.
func CPUModel(vendor string, nested bool) string {
	flag := nestedFlag(vendor)
	if flag == "" {
		return "host"
	}
	if nested {
		return "host,+" + flag
	}
	return "host,-" + flag
}

func nestedFlag(vendor string) string {
	switch vendor {
	case "intel":
		return "vmx"
	case "amd":
		return "svm"
	default:
		return ""
	}
}

// CPUVendor reads the vendor from the host, because the name of the flag that
// carries virtualisation support depends on it and asking QEMU for the wrong
// one makes it refuse to start.
func CPUVendor() string {
	info, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	text := string(info)
	switch {
	case strings.Contains(text, "GenuineIntel"):
		return "intel"
	case strings.Contains(text, "AuthenticAMD"):
		return "amd"
	default:
		return "unknown"
	}
}

// qemuBinary is the emulator for this architecture.
func qemuBinary() string {
	if runtime.GOARCH == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

func machineType() string {
	if runtime.GOARCH == "arm64" {
		return "virt,gic-version=host"
	}
	return "q35"
}

// VMOptions is everything needed to boot one runner's machine.
type VMOptions struct {
	Name      string
	Dir       string
	Disk      string
	Seed      string
	CPUs      int
	MemoryMB  int
	SSHPort   int
	CPUModel  string
	QMPSocket string
	Console   string
}

// QEMUArgs builds the command line. It is a function of its arguments alone so
// that a test can assert on what a pool's settings turn into, which is the part
// that decides whether a job gets /dev/kvm.
func QEMUArgs(o VMOptions) []string {
	return []string{
		"-name", o.Name,
		"-machine", machineType(),
		"-cpu", o.CPUModel,
		"-accel", "kvm",
		"-smp", strconv.Itoa(o.CPUs),
		"-m", strconv.Itoa(o.MemoryMB),
		"-drive", "file=" + o.Disk + ",if=virtio,format=qcow2,cache=writeback",
		"-drive", "file=" + o.Seed + ",if=virtio,format=raw,readonly=on",
		// User-mode networking with one forwarded port: a runner needs to reach
		// GitHub, and nothing needs to reach it except this host's ssh.
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", o.SSHPort),
		"-device", "virtio-net-pci,netdev=net0",
		"-device", "virtio-rng-pci",
		// A balloon, for one reason: so the guest can give memory back.
		//
		// Guest RAM is faulted in on demand — there is no -mem-prealloc above,
		// so -m is a ceiling and not a reservation — but without this nothing
		// ever goes the other way. Every page the guest touches once is a host
		// page QEMU holds until it exits, so a job that links something large
		// leaves its peak resident for the rest of the machine's life: free
		// inside the guest, and still charged to the host. On a box running
		// several pools that is the difference between a fleet that settles
		// after a busy morning and one that only settles when the machines are
		// replaced.
		//
		// free-page-reporting is the half of virtio-balloon that needs nothing
		// watching it. The guest volunteers pages it has already freed and the
		// host drops them; nobody inflates the balloon, so nothing here can
		// take memory away from a job in flight. It reports free pages and only
		// free pages — the guest's page cache is not free, so the ~1 GiB a
		// booted machine sits at is not what this recovers. What it recovers is
		// the peak afterwards.
		"-device", "virtio-balloon,free-page-reporting=on",
		"-display", "none",
		"-serial", "file:" + o.Console,
		// The monitor is how the agent asks for a clean shutdown: an ACPI
		// power button press, which the guest's systemd turns into a proper
		// stop — and that waits for the job in flight.
		"-qmp", "unix:" + o.QMPSocket + ",server=on,wait=off",
		"-no-reboot",
	}
}

// bootVM starts QEMU and returns the running process.
func bootVM(ctx context.Context, o VMOptions) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, qemuBinary(), QEMUArgs(o)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", qemuBinary(), err)
	}
	return cmd, nil
}

// freePort finds a loopback port for the machine's ssh forward.
//
// Asking the kernel for one and closing it immediately is not perfect — the
// port could be taken in the gap — but QEMU only binds it at boot, and the
// alternative is a registry that has to survive a daemon restart.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// EnsureSSHKey creates the key that reaches every machine on this host. It is
// for looking inside a runner that is misbehaving; nothing in the normal path
// uses it.
func EnsureSSHKey(path string) (string, error) {
	if pub, err := os.ReadFile(path + ".pub"); err == nil {
		return strings.TrimSpace(string(pub)), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "runner-fleet", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ssh-keygen: %w: %s", err, out)
	}
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(pub)), nil
}

// makeSeed builds the cloud-init ISO the machine reads its configuration from.
func makeSeed(ctx context.Context, userData, metaData, out string) error {
	dir := filepath.Dir(out)
	userPath := filepath.Join(dir, "user-data")
	metaPath := filepath.Join(dir, "meta-data")
	if err := os.WriteFile(userPath, []byte(userData), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(metaPath, []byte(metaData), 0o600); err != nil {
		return err
	}

	// cloud-localds is the usual tool; the others are what is available when
	// it is not installed, and all three produce the same thing.
	if _, err := exec.LookPath("cloud-localds"); err == nil {
		return run(ctx, "cloud-localds", out, userPath, metaPath)
	}
	for _, tool := range []string{"genisoimage", "mkisofs", "xorrisofs"} {
		if _, err := exec.LookPath(tool); err != nil {
			continue
		}
		return run(ctx, tool, "-output", out, "-volid", "cidata", "-joliet", "-rock", userPath, metaPath)
	}
	return fmt.Errorf("no ISO builder found: install cloud-image-utils, genisoimage or xorriso")
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
