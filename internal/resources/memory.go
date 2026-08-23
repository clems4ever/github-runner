package resources

// WorkingSet is what a cgroup is really using: everything charged to it, less
// the file cache the kernel would drop rather than run out of memory.
//
// The subtraction is not cosmetic. What the kernel charges a cgroup — cgroup
// v2's `memory.current`, which is also what systemd reports as MemoryCurrent,
// and what Docker reports as `usage` — includes the page cache. Anything that
// has read a lot of disk therefore reports most of its limit used and looks
// about to die when it is fine, and a machine booting a whole distribution off
// a qcow2 reads a lot of disk. Docker's own CLI subtracts the inactive file
// cache before printing, under whichever of the two names the cgroup version in
// use gives it, and so does this.
//
// Both runtimes go through here, and that is the point of it being one
// function. A container and a machine are shown side by side in one table under
// one heading, so they have to be the same quantity — and they were not: the
// container path took its cache out and the machine path did not, which made
// every machine on the page look like it was holding an order of magnitude more
// than the container beside it. Part of that gap is real. Part of it was the
// two columns not measuring the same thing.
func WorkingSet(charged int64, stats map[string]int64) int64 {
	for _, key := range []string{"inactive_file", "total_inactive_file"} {
		if cache, ok := stats[key]; ok {
			charged -= cache
			break
		}
	}
	// A reading and its cache come from two places and can disagree by a hair
	// when the kernel moves pages between them mid-sample. Negative memory is
	// not a thing to render.
	if charged < 0 {
		return 0
	}
	return charged
}
