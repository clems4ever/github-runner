package resources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProc writes a directory that looks enough like /proc to read.
func fakeProc(t *testing.T, stat, meminfo, loadavg string) string {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"stat": stat, "meminfo": meminfo, "loadavg": loadavg,
	} {
		if contents == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const meminfo = `MemTotal:       16000 kB
MemFree:         1000 kB
MemAvailable:    8000 kB
Buffers:          500 kB
Cached:          2000 kB
`

const loadavg = "1.50 0.75 0.25 2/512 9999\n"

func collector(t *testing.T, proc string) *HostCollector {
	t.Helper()
	c := &HostCollector{proc: proc, disk: t.TempDir(), statfs: statfs}
	c.prime()
	return c
}

func TestCPUPercentIsTheShareOfEveryCore(t *testing.T) {
	// Idle and iowait are the two fields that are not work; everything else is.
	// Between the readings the host spent 200 ticks working and 800 idle.
	proc := fakeProc(t, "cpu  100 0 100 800 0 0 0 0\n", meminfo, loadavg)
	c := collector(t, proc)

	if err := os.WriteFile(filepath.Join(proc, "stat"), []byte("cpu  200 0 200 1600 0 0 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if host.CPUPercent != 20 {
		t.Fatalf("want 20%% of the machine busy, got %v", host.CPUPercent)
	}
}

// Guest time is already counted inside user and nice. A host that runs virtual
// machines — which is every host this daemon is for — would otherwise report a
// total larger than the time that actually passed, and a percentage smaller
// than the truth.
func TestGuestTimeIsNotCountedTwice(t *testing.T) {
	proc := fakeProc(t, "cpu  100 0 100 800 0 0 0 0 5000 5000\n", meminfo, loadavg)
	c := collector(t, proc)

	if err := os.WriteFile(filepath.Join(proc, "stat"),
		[]byte("cpu  200 0 200 1600 0 0 0 0 9000 9000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if host.CPUPercent != 20 {
		t.Fatalf("the guest columns leaked into the total: got %v", host.CPUPercent)
	}
}

// The first sample has nothing to subtract from. Zero is the honest answer for
// a window that does not exist, and the collector primes itself when it is
// built so that this is only ever true of a reading taken instantly.
func TestACounterThatWentBackwardsDoesNotBecomeASpike(t *testing.T) {
	proc := fakeProc(t, "cpu  500 0 500 4000 0 0 0 0\n", meminfo, loadavg)
	c := collector(t, proc)

	// A reboot: the counters start again from nearly nothing.
	if err := os.WriteFile(filepath.Join(proc, "stat"), []byte("cpu  1 0 1 8 0 0 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if host.CPUPercent != 0 {
		t.Fatalf("want nothing reported for an impossible window, got %v", host.CPUPercent)
	}
}

// Free memory on a busy Linux host is close to zero by design: the page cache
// takes the rest and hands it back on demand. A meter drawn from MemFree would
// show every healthy host as full.
func TestMemoryUsedIsWhatIsNotAvailableRatherThanWhatIsNotFree(t *testing.T) {
	c := collector(t, fakeProc(t, "cpu  1 0 1 8 0 0 0 0\n", meminfo, loadavg))
	host, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if host.MemoryTotalBytes != 16000*1024 {
		t.Fatalf("total memory: got %d", host.MemoryTotalBytes)
	}
	if host.MemoryUsedBytes != 8000*1024 {
		t.Fatalf("want total minus MemAvailable, got %d", host.MemoryUsedBytes)
	}
}

func TestLoadAverageIsReadInOrder(t *testing.T) {
	c := collector(t, fakeProc(t, "cpu  1 0 1 8 0 0 0 0\n", meminfo, loadavg))
	host, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if host.Load1 != 1.5 || host.Load5 != 0.75 || host.Load15 != 0.25 {
		t.Fatalf("load: got %v %v %v", host.Load1, host.Load5, host.Load15)
	}
}

// A host missing one of these files still has the rest, and an operator is
// better served by three numbers and a note than by nothing at all.
func TestOneUnreadableFileDoesNotLoseTheOthers(t *testing.T) {
	c := collector(t, fakeProc(t, "cpu  1 0 1 8 0 0 0 0\n", meminfo, ""))

	host, err := c.Sample()
	if err == nil {
		t.Fatal("want the missing file named in an error")
	}
	if !strings.Contains(err.Error(), "loadavg") {
		t.Fatalf("the error should say what could not be read: %v", err)
	}
	if host.MemoryTotalBytes == 0 {
		t.Fatal("memory was readable and should have been reported anyway")
	}
	if host.DiskTotalBytes == 0 {
		t.Fatal("the disk was measurable and should have been reported anyway")
	}
}

func TestDiskIsMeasuredOnTheDirectoryTheFleetFills(t *testing.T) {
	dir := t.TempDir()
	c := &HostCollector{proc: fakeProc(t, "cpu  1 0 1 8 0 0 0 0\n", meminfo, loadavg), disk: dir, statfs: statfs}
	c.prime()

	host, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if host.DiskPath != dir {
		t.Fatalf("want the path reported so it is clear which filesystem this is, got %q", host.DiskPath)
	}
	if host.DiskTotalBytes <= 0 || host.DiskUsedBytes > host.DiskTotalBytes {
		t.Fatalf("disk: used %d of %d", host.DiskUsedBytes, host.DiskTotalBytes)
	}
}
