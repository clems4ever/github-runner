package resources

import (
	"testing"
	"time"
)

var epoch = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func TestARateNeedsTwoReadings(t *testing.T) {
	rate := &Rate{cores: 4, last: map[string]reading{}}

	// First sight. Nothing is reported rather than a zero, which on a machine
	// that is busily booting would be a lie the dashboard cannot take back.
	if got := rate.percentAt("web-1", 1_000_000_000, epoch); got != nil {
		t.Fatalf("want no figure from one reading, got %v", *got)
	}

	// One second later it has used two seconds of processor time on a
	// four-core host: half of one core, an eighth of the machine.
	got := rate.percentAt("web-1", 3_000_000_000, epoch.Add(time.Second))
	if got == nil {
		t.Fatal("want a figure once there are two readings")
	}
	if *got != 50 {
		t.Fatalf("want 50%% of a four-core host, got %v", *got)
	}
}

func TestPercentIsAShareOfTheWholeHost(t *testing.T) {
	// Two cores fully consumed on an eight-core host is a quarter of it, not
	// two hundred per cent. The host's own meter is on the same scale, and a
	// bar longer than its track is how a dashboard stops being read.
	rate := &Rate{cores: 8, last: map[string]reading{}}
	rate.percentAt("web-1", 0, epoch)

	got := rate.percentAt("web-1", 2*nanos(time.Second), epoch.Add(time.Second))
	if got == nil || *got != 25 {
		t.Fatalf("want 25%%, got %v", got)
	}
}

func TestAReusedNameStartsAgain(t *testing.T) {
	// The fleet rebuilds a machine under the same name every time it starts
	// one, so the counter goes back to nearly nothing. The window before this
	// runner existed is not its to account for.
	rate := &Rate{cores: 2, last: map[string]reading{}}
	rate.percentAt("web-1", 60_000_000_000, epoch)

	if got := rate.percentAt("web-1", 1_000_000, epoch.Add(time.Second)); got != nil {
		t.Fatalf("want nothing reported across a rebuild, got %v", *got)
	}
	// And it measures again from there.
	if got := rate.percentAt("web-1", 1_000_000+nanos(time.Second), epoch.Add(2*time.Second)); got == nil {
		t.Fatal("want the next window measured normally")
	}
}

func TestPercentIsCappedAtWhatTheHostHas(t *testing.T) {
	rate := &Rate{cores: 1, last: map[string]reading{}}
	rate.percentAt("web-1", 0, epoch)

	// More processor time than wall time on one core cannot happen, but a
	// clock that stepped could make it look that way.
	got := rate.percentAt("web-1", 9*nanos(time.Second), epoch.Add(time.Second))
	if got == nil || *got != 100 {
		t.Fatalf("want a figure a meter can draw, got %v", got)
	}
}

// A fleet that has churned through a thousand ephemeral machines should not
// carry a thousand readings for runners that no longer exist.
func TestKeepForgetsRunnersThatAreGone(t *testing.T) {
	rate := &Rate{cores: 2, last: map[string]reading{}}
	rate.percentAt("web-1", 1, epoch)
	rate.percentAt("web-2", 1, epoch)

	rate.Keep([]string{"web-2"})

	if _, held := rate.last["web-1"]; held {
		t.Fatal("a runner that is gone is still remembered")
	}
	if _, held := rate.last["web-2"]; !held {
		t.Fatal("a runner that is still here was forgotten")
	}
}

func nanos(d time.Duration) uint64 { return uint64(d.Nanoseconds()) }
