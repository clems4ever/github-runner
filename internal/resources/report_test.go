package resources

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

type stubSource struct {
	usage []RunnerUsage
	err   error
}

func (s stubSource) Usage(context.Context) ([]RunnerUsage, error) { return s.usage, s.err }

type stubRecorder struct {
	samples []model.HostSample
	err     error
}

func (r *stubRecorder) RecordHostSample(_ context.Context, _ time.Time, sample model.HostSample) error {
	r.samples = append(r.samples, sample)
	return r.err
}

func testReporter(t *testing.T, sources ...Source) *Reporter {
	t.Helper()
	return NewReporter(collector(t, fakeProc(t, "cpu  1 0 1 8 0 0 0 0\n", meminfo, loadavg)), sources...)
}

func TestTheFleetIsOneListWhateverRuntimeItIs(t *testing.T) {
	// Two runtimes measured two entirely different ways, sorted together, so
	// the page reads as a fleet rather than as two tables.
	reporter := testReporter(t,
		stubSource{usage: []RunnerUsage{{Name: "web-2", Runtime: "vm"}}},
		stubSource{usage: []RunnerUsage{{Name: "api-1", Runtime: "container"}}},
	)

	report := reporter.Report(context.Background())
	if len(report.Runners) != 2 {
		t.Fatalf("got %d runners", len(report.Runners))
	}
	if report.Runners[0].Name != "api-1" || report.Runners[1].Name != "web-2" {
		t.Fatalf("want a stable order the UI does not shuffle, got %v", report.Runners)
	}
}

// A runtime that could answer for nine runners and not the tenth has still
// answered for nine. Throwing the nine away because of the tenth would leave a
// page that says a busy host is empty.
func TestAPartialAnswerIsKeptAlongsideItsWarning(t *testing.T) {
	reporter := testReporter(t, stubSource{
		usage: []RunnerUsage{{Name: "api-1"}},
		err:   errors.New("docker would not say what these containers are using: api-2"),
	})

	report := reporter.Report(context.Background())
	if len(report.Runners) != 1 {
		t.Fatalf("the rows that were readable were lost: %v", report.Runners)
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("want the missing part reported, got %v", report.Warnings)
	}
}

// "No container is using anything" and "Docker did not answer" look identical
// on a dashboard unless the second one says so.
func TestARuntimeThatCannotAnswerSaysSo(t *testing.T) {
	reporter := testReporter(t, stubSource{err: errors.New("is dockerd running?")})

	report := reporter.Report(context.Background())
	if len(report.Warnings) != 1 || report.Warnings[0] != "is dockerd running?" {
		t.Fatalf("want the reason surfaced, got %v", report.Warnings)
	}
}

func TestNothingIsReportedBeforeTheFirstReading(t *testing.T) {
	sampler := NewSampler(testReporter(t), nil, quietLog())

	if _, ready := sampler.Latest(); ready {
		// A host with no processors and no memory is not a measurement, and a
		// page that drew one would be lying for the first second after every
		// restart.
		t.Fatal("a sampler that has never sampled says it has")
	}

	sampler.once(context.Background())
	report, ready := sampler.Latest()
	if !ready {
		t.Fatal("want the reading available once it has been taken")
	}
	if report.At.IsZero() {
		t.Fatal("want the reading stamped, so the page can say how fresh it is")
	}
}

func TestEachReadingIsWrittenDown(t *testing.T) {
	store := &stubRecorder{}
	sampler := NewSampler(testReporter(t), store, quietLog())

	sampler.once(context.Background())
	sampler.once(context.Background())

	if len(store.samples) != 2 {
		t.Fatalf("want the history recorded on every reading, got %d", len(store.samples))
	}
	if store.samples[0].MemoryTotalBytes != 16000*1024 {
		t.Fatalf("the sample does not carry what was measured: %+v", store.samples[0])
	}
}

// History is worth having and not worth stopping the sampler over: a database
// that cannot be written must not cost the operator the live view as well.
func TestAFailedWriteDoesNotStopTheReading(t *testing.T) {
	store := &stubRecorder{err: errors.New("database is locked")}
	sampler := NewSampler(testReporter(t), store, quietLog())

	sampler.once(context.Background())

	if _, ready := sampler.Latest(); !ready {
		t.Fatal("the live reading was lost because the history could not be written")
	}
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
