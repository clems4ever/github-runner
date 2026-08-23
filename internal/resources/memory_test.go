package resources

import "testing"

// The page cache is not memory a runner is using in any sense that matters to
// somebody deciding whether the host is full. The kernel drops it rather than
// run out, so a machine reading a large disk holds no less memory available
// than one that has read nothing.
func TestWorkingSetLeavesOutTheCacheTheKernelWouldDrop(t *testing.T) {
	for _, tt := range []struct {
		name    string
		charged int64
		stats   map[string]int64
		want    int64
	}{
		{
			// The unified hierarchy's name for it, which is what any host this
			// runs on today reports.
			name:    "cgroup v2",
			charged: 1 << 30,
			stats:   map[string]int64{"inactive_file": 1 << 29},
			want:    1 << 29,
		},
		{
			// And v1's, which prefixes the same figure. Only one of the two is
			// ever present, and reading whichever it is means neither kind of
			// host is quietly reported differently from the other.
			name:    "cgroup v1",
			charged: 1000,
			stats:   map[string]int64{"total_inactive_file": 400},
			want:    600,
		},
		{
			// Nothing to subtract is not an error. Docker omits the key on some
			// versions and a stopped unit has no memory.stat at all — in both
			// cases the charge is the best available answer, and it is the
			// answer this reported before there was anything better.
			name:    "no cache figure",
			charged: 4096,
			stats:   nil,
			want:    4096,
		},
		{
			// The charge and the cache are two reads of a moving target. The
			// kernel can shift pages between them in the gap, and a negative
			// number is not something to put on a page.
			name:    "cache larger than the charge",
			charged: 100,
			stats:   map[string]int64{"inactive_file": 500},
			want:    0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := WorkingSet(tt.charged, tt.stats); got != tt.want {
				t.Errorf("WorkingSet(%d, %v) = %d, want %d", tt.charged, tt.stats, got, tt.want)
			}
		})
	}
}

// Both cgroup versions cannot be believed at once. A host that somehow reported
// both names would otherwise have the cache taken off twice, and the first key
// is the one the kernel this daemon supports actually uses.
func TestWorkingSetSubtractsTheCacheOnce(t *testing.T) {
	got := WorkingSet(1000, map[string]int64{"inactive_file": 300, "total_inactive_file": 300})
	if got != 700 {
		t.Errorf("got %d, want 700 — the cache was taken off more than once", got)
	}
}
