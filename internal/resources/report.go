package resources

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

// RunnerUsage is what one runner is consuming right now.
//
// Where it comes from depends on the runtime and neither source is this
// package's business: a container is measured by Docker, a machine by the
// accounting systemd already keeps for the unit QEMU runs in. Both end up
// here, in the same units, so the fleet can be read as one list.
type RunnerUsage struct {
	Name    string `json:"name"`
	Pool    string `json:"pool"`
	Runtime string `json:"runtime"`
	// CPUPercent is nil until the runner has been seen twice.
	//
	// Processor time is a counter, so a rate needs two readings, and a runner
	// that has only just been created has one. Reporting zero would be a
	// machine that is busily booting shown as idle, which is worse than an
	// honest dash — so the field is absent instead, and fills in a sample
	// later. Like the host's, it is a share of every core together.
	CPUPercent  *float64 `json:"cpuPercent"`
	MemoryBytes int64    `json:"memoryBytes"`
}

// Source is one runtime's account of what its runners are using. The executors
// implement it; a runtime that cannot answer simply does not.
type Source interface {
	Usage(ctx context.Context) ([]RunnerUsage, error)
}

// Report is the whole picture at one moment.
type Report struct {
	At      time.Time     `json:"at"`
	Host    Host          `json:"host"`
	Runners []RunnerUsage `json:"runners"`
	// Warnings are the parts that could not be read. They are reported rather
	// than swallowed, because "no container is using anything" and "Docker did
	// not answer" look identical otherwise.
	Warnings []string `json:"warnings"`
}

// Reporter puts the host and its runners together.
type Reporter struct {
	host    *HostCollector
	sources []Source
	now     func() time.Time
}

// NewReporter builds a reporter over one host collector and whichever runtimes
// can account for themselves.
func NewReporter(host *HostCollector, sources ...Source) *Reporter {
	return &Reporter{host: host, sources: sources, now: time.Now}
}

// Report reads everything once.
//
// A runtime that fails is a warning, not an error: a host with Docker stopped
// still has memory, a disk and a fleet of machines worth showing.
func (r *Reporter) Report(ctx context.Context) Report {
	report := Report{At: r.now().UTC(), Runners: []RunnerUsage{}}

	host, err := r.host.Sample()
	report.Host = host
	if err != nil {
		report.Warnings = append(report.Warnings, err.Error())
	}

	for _, source := range r.sources {
		// The rows are taken even when there is an error, because a runtime
		// that could answer for nine runners and not the tenth has still
		// answered for nine. The error says which part is missing.
		usage, err := source.Usage(ctx)
		report.Runners = append(report.Runners, usage...)
		if err != nil {
			report.Warnings = append(report.Warnings, err.Error())
		}
	}
	sort.Slice(report.Runners, func(i, j int) bool { return report.Runners[i].Name < report.Runners[j].Name })
	return report
}

// Recorder is the part of the store the sampler writes to.
type Recorder interface {
	RecordHostSample(ctx context.Context, at time.Time, sample model.HostSample) error
}

// Sampler reads the host on a timer, keeps the latest reading for the API and
// writes the host's share of it down as history.
//
// On a timer rather than per request, for two reasons that point the same way.
// A percentage is a difference between two readings, so something has to take
// them at a known cadence — and letting every open browser tab trigger its own
// would have them stealing each other's windows. And the history the chart
// draws has to be recorded whether or not anyone is looking.
type Sampler struct {
	reporter *Reporter
	store    Recorder
	log      *slog.Logger

	mu     sync.Mutex
	latest Report
	ready  bool
}

// NewSampler builds one. The store may be nil, which is a sampler that reports
// but keeps no history.
func NewSampler(reporter *Reporter, store Recorder, log *slog.Logger) *Sampler {
	if log == nil {
		log = slog.Default()
	}
	return &Sampler{reporter: reporter, store: store, log: log}
}

// firstSample is how long the sampler waits before its first reading.
//
// Not the full interval: the collector took a processor reading when it was
// built, so a second one a moment later is already a valid window, and it means
// a restarted daemon has something to show almost immediately rather than
// leaving the page empty for the length of a tick.
const firstSample = time.Second

// Run samples until the context is cancelled.
func (s *Sampler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timer := time.NewTimer(firstSample)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.once(ctx)
			timer.Reset(interval)
		}
	}
}

func (s *Sampler) once(ctx context.Context) {
	report := s.reporter.Report(ctx)

	s.mu.Lock()
	s.latest, s.ready = report, true
	s.mu.Unlock()

	for _, warning := range report.Warnings {
		s.log.Warn("resources", "problem", warning)
	}
	if s.store == nil {
		return
	}
	if err := s.store.RecordHostSample(ctx, report.At, model.HostSample{
		CPUPercent:       report.Host.CPUPercent,
		MemoryUsedBytes:  report.Host.MemoryUsedBytes,
		MemoryTotalBytes: report.Host.MemoryTotalBytes,
		DiskUsedBytes:    report.Host.DiskUsedBytes,
		DiskTotalBytes:   report.Host.DiskTotalBytes,
	}); err != nil {
		// History is worth having, not worth stopping the sampler over.
		s.log.Warn("record what the host is using", "error", err)
	}
}

// Latest is the most recent reading, and whether there has been one at all.
//
// The second return is what keeps a page opened in the first second after a
// restart from drawing a host with no processors and no memory as though that
// were a measurement.
func (s *Sampler) Latest() (Report, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest, s.ready
}
