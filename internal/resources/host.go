// Package resources reports what the host and the runners on it are actually
// using, as opposed to what their pools were promised.
//
// Everything here is read from the kernel: /proc and statfs answer every
// question this needs, and a dependency that pulls in a hundred files to read
// four of them is a poor trade for a daemon that only ships for Linux. The
// paths are injectable so the readers can be tested against a directory of
// fixtures rather than against whatever the machine running the tests happens
// to be doing.
package resources

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Host is what the whole machine is doing.
type Host struct {
	// CPUs is how many logical processors there are, which is what the
	// percentage below is a fraction of.
	CPUs int `json:"cpus"`
	// CPUPercent is 0 to 100 across every core together: a host with eight
	// cores and four of them saturated reads 50, not 400. Percentages that can
	// exceed 100 are how a dashboard ends up with a bar longer than its track.
	CPUPercent       float64 `json:"cpuPercent"`
	MemoryUsedBytes  int64   `json:"memoryUsedBytes"`
	MemoryTotalBytes int64   `json:"memoryTotalBytes"`
	// The disk reported is the filesystem holding the state directory, because
	// that is the one the fleet fills: golden images and every machine's
	// copy-on-write disk live there.
	DiskPath       string  `json:"diskPath"`
	DiskUsedBytes  int64   `json:"diskUsedBytes"`
	DiskTotalBytes int64   `json:"diskTotalBytes"`
	Load1          float64 `json:"load1"`
	Load5          float64 `json:"load5"`
	Load15         float64 `json:"load15"`
}

// HostCollector reads the host, one sample at a time.
//
// It has to be a struct rather than a function because processor time is a
// counter, not a level: a percentage is the difference between two readings,
// so the previous one has to live somewhere between calls.
type HostCollector struct {
	proc   string
	disk   string
	statfs func(path string) (total, used int64, err error)

	// last is the previous reading of /proc/stat. No timestamp goes with it:
	// the aggregate line counts ticks across every core, so idle is in the
	// total and the ratio is already a fraction of the whole machine however
	// long the gap was. A late tick therefore averages, rather than spiking.
	mu   sync.Mutex
	last cpuCounters
}

// cpuCounters is /proc/stat's first line, reduced to the two numbers a
// percentage needs.
type cpuCounters struct {
	busy, total uint64
	valid       bool
}

// NewHostCollector watches the filesystem that holds diskPath.
//
// The first processor reading is taken here rather than on the first sample,
// so that the sample has something to subtract from and never has to report a
// rate it could not compute.
func NewHostCollector(diskPath string) *HostCollector {
	c := &HostCollector{proc: "/proc", disk: diskPath, statfs: statfs}
	c.prime()
	return c
}

func (c *HostCollector) prime() {
	if counters, err := readCPU(filepath.Join(c.proc, "stat")); err == nil {
		c.last = counters
	}
}

// Sample reads the host now.
//
// Whatever could not be read is left at zero and named in the error, rather
// than failing the whole sample: a container host with no /proc/loadavg still
// has memory worth reporting, and an operator looking at a dashboard is better
// served by three numbers and a note than by nothing at all.
func (c *HostCollector) Sample() (Host, error) {
	host := Host{CPUs: runtime.NumCPU(), DiskPath: c.disk}
	var problems []string

	if percent, err := c.cpuPercent(); err != nil {
		problems = append(problems, err.Error())
	} else {
		host.CPUPercent = percent
	}

	if total, used, err := readMemory(filepath.Join(c.proc, "meminfo")); err != nil {
		problems = append(problems, err.Error())
	} else {
		host.MemoryTotalBytes, host.MemoryUsedBytes = total, used
	}

	if one, five, fifteen, err := readLoad(filepath.Join(c.proc, "loadavg")); err != nil {
		problems = append(problems, err.Error())
	} else {
		host.Load1, host.Load5, host.Load15 = one, five, fifteen
	}

	if c.disk != "" {
		if total, used, err := c.statfs(c.disk); err != nil {
			problems = append(problems, fmt.Sprintf("measure %s: %v", c.disk, err))
		} else {
			host.DiskTotalBytes, host.DiskUsedBytes = total, used
		}
	}

	if len(problems) > 0 {
		return host, fmt.Errorf("read the host: %s", strings.Join(problems, "; "))
	}
	return host, nil
}

// cpuPercent is the share of every core that was busy since the last sample.
func (c *HostCollector) cpuPercent() (float64, error) {
	now, err := readCPU(filepath.Join(c.proc, "stat"))
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.last
	c.last = now

	// No previous reading, or a counter that went backwards because the host
	// rebooted underneath us. Zero is the honest answer for a window that does
	// not exist, and the next sample has a window again.
	if !previous.valid || now.total <= previous.total {
		return 0, nil
	}
	busy := float64(now.busy - previous.busy)
	total := float64(now.total - previous.total)
	return clampPercent(busy / total * 100), nil
}

func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		// Two decimals: a chart cannot draw more, and the extra digits only
		// make the JSON longer.
		return float64(int64(v*100+0.5)) / 100
	}
}

// readCPU sums /proc/stat's aggregate line.
//
// Only the first eight fields are counted. The two after them — guest and
// guest_nice — are already included in user and nice, so adding them would
// inflate the total on any host that runs virtual machines, which is every
// host this daemon is for.
func readCPU(path string) (cpuCounters, error) {
	file, err := os.Open(path)
	if err != nil {
		return cpuCounters{}, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var counters cpuCounters
		for i, field := range fields[1:] {
			if i >= 8 {
				break
			}
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuCounters{}, fmt.Errorf("read %s: %q is not a number", path, field)
			}
			counters.total += value
			// Fields three and four are idle and iowait. Everything else is
			// work of one kind or another.
			if i != 3 && i != 4 {
				counters.busy += value
			}
		}
		counters.valid = true
		return counters, nil
	}
	if err := scanner.Err(); err != nil {
		return cpuCounters{}, fmt.Errorf("read %s: %w", path, err)
	}
	return cpuCounters{}, fmt.Errorf("read %s: no aggregate cpu line", path)
}

// readMemory reports what the host has and what is spoken for.
//
// Used is total minus MemAvailable, not minus MemFree. Free memory on a busy
// Linux host is close to zero by design — the page cache takes the rest and
// gives it back on demand — so a meter drawn from MemFree would show every
// healthy host as full.
func readMemory(path string) (total, used int64, err error) {
	values, err := readKeyedKB(path)
	if err != nil {
		return 0, 0, err
	}
	total, ok := values["MemTotal"]
	if !ok {
		return 0, 0, fmt.Errorf("read %s: no MemTotal", path)
	}
	available, ok := values["MemAvailable"]
	if !ok {
		// Kernels before 3.14 do not publish it. The estimate below is what
		// MemAvailable replaced, and it is better than nothing.
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if available > total {
		available = total
	}
	return total, total - available, nil
}

func readKeyedKB(path string) (map[string]int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()

	values := map[string]int64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		amount, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// Everything in meminfo is in kibibytes except a handful of counts,
		// which are not read here.
		values[key] = amount * 1024
	}
	return values, scanner.Err()
}

func readLoad(path string) (one, five, fifteen float64, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("read %s: %q is not a load average", path, strings.TrimSpace(string(raw)))
	}
	one, _ = strconv.ParseFloat(fields[0], 64)
	five, _ = strconv.ParseFloat(fields[1], 64)
	fifteen, _ = strconv.ParseFloat(fields[2], 64)
	return one, five, fifteen, nil
}
