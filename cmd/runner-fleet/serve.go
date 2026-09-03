package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/clems4ever/github-runner/internal/agent"
	"github.com/clems4ever/github-runner/internal/api"
	"github.com/clems4ever/github-runner/internal/executor/docker"
	"github.com/clems4ever/github-runner/internal/executor/systemd"
	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/imagebuild"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/reconcile"
	"github.com/clems4ever/github-runner/internal/resources"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
	"github.com/clems4ever/github-runner/internal/ui"
)

func newAuthenticator(db *store.Store) *api.Authenticator { return api.NewAuthenticator(db) }

func serveCommand(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:8080", "where to listen")
	root := flags.String("root", "", "put everything under this directory")
	interval := flags.Duration("interval", 30*time.Second, "how often to reconcile")
	sampleInterval := flags.Duration("resource-interval", 15*time.Second,
		"how often to measure what the host and its runners are using")
	collectInterval := flags.Duration("collect-interval", 10*time.Minute,
		"how often to delete golden images no pool wants and nothing is booting")
	user := flags.String("user", "runner-fleet", "the service user the runners' units run as")
	if err := flags.Parse(args); err != nil {
		return err
	}

	log := logger()
	layout := layoutFor(*root)

	// The runners run as somebody else, and everything they read is written
	// here. Resolving that account is the first thing done, and failing to
	// resolve it is a warning rather than an error: container pools do not need
	// it, and a developer running the daemon as themselves has nobody to hand
	// anything to.
	owner, err := paths.LookupOwner(*user)
	if err != nil {
		log.Warn("machine pools will not be able to read their credential", "error", err)
	}
	if err := layout.EnsureDirs(owner); err != nil {
		return err
	}

	ring, err := secrets.LoadOrCreateKey(layout.MasterKey())
	if err != nil {
		return err
	}
	db, err := store.Open(layout.Database(), ring)
	if err != nil {
		return err
	}
	defer db.Close()

	binary, err := os.Executable()
	if err != nil {
		return err
	}

	vm := systemd.New(layout, binary, *user)
	if err := vm.EnsureUnit(context.Background()); err != nil {
		// Not fatal: a developer running this without root still gets the UI
		// and the API, and container pools work regardless.
		log.Warn("could not install the runner unit; VM pools will not start", "error", err)
	}
	containers := docker.New(layout, binary)

	// What the host and its runners are actually using, as opposed to what
	// their pools were promised. The disk watched is the state directory's,
	// because that is the one the fleet fills: golden images and every
	// machine's disk live under it.
	sampler := resources.NewSampler(
		resources.NewReporter(resources.NewHostCollector(layout.State), vm, containers),
		db, log)

	// Golden images are built here, in the daemon, before the runners that
	// would boot them exist. That is what lets a build be reported while it
	// happens, kept afterwards, and attempted once rather than retried for
	// ever by a unit that cannot know any better.
	builder := imagebuild.New(imagebuild.Options{
		ImagesDir: layout.ImagesDir(),
		SSHKey:    layout.SSHKey(),
		Store:     db,
		Owner:     owner,
		Log:       log,
	})
	// Before anything can ask for a build, so that builds interrupted by this
	// restart are settled and not confused with the ones about to be queued.
	if err := builder.Adopt(context.Background()); err != nil {
		log.Warn("could not read what this host has built", "error", err)
	}

	// Working directories left behind by machines that were killed rather than
	// stopped. A machine deletes its own on the way out, which does not happen
	// when the process is killed, when the host crashes, or when the OOM killer
	// picks it — so they accumulated, several gigabytes at a time, and nothing
	// ever looked at them. Startup is the right moment: it is the one point at
	// which no machine this daemon started is running yet, so anything with a
	// file open in there is a survivor of the last run and is left alone.
	sweep(layout, log)

	// The reconciler is told how to reach GitHub and how to hand a credential
	// to a runner. Both are injected rather than reached for directly, which
	// is what lets the fleet's rules be tested without either.
	reconciler := reconcile.New(db,
		[]reconcile.Executor{vm, containers},
		func(secret model.Secret) (reconcile.GitHubClient, error) {
			return github.NewFromSecret(github.Secret{
				IsAppCredential: secret.IsApp(),
				Token:           secret.Token,
				AppID:           secret.AppID,
				InstallationID:  secret.InstallationID,
			})
		},
		credentialWriter(layout, owner),
		log).
		// A machine pool gets no runners until the image they would boot has
		// been built, and asking is what starts the first build.
		WithImages(func(ctx context.Context, pool model.Pool) (bool, string) {
			status := builder.Ensure(ctx, pool)
			return status.Ready, status.Summary
		})

	// A buffered channel of one: several changes arriving together collapse
	// into a single extra pass rather than queueing up.
	nudge := make(chan struct{}, 1)
	newGitHub := func(secret model.Secret) (*github.Client, error) {
		return github.NewFromSecret(github.Secret{
			IsAppCredential: secret.IsApp(),
			Token:           secret.Token,
			AppID:           secret.AppID,
			InstallationID:  secret.InstallationID,
		})
	}

	server := api.New(api.Options{
		Store:     db,
		Fleet:     reconciler,
		Resources: sampler,
		Images:    builder,
		UI:        uiAssets(log),
		Version:   version,
		// Asked when a pool is saved, so a credential that cannot serve it is
		// caught while someone is looking at the form rather than a minute
		// later in a log.
		CheckAccess: func(ctx context.Context, credentialID int64, scope github.Scope) error {
			secret, err := db.Secret(ctx, credentialID)
			if err != nil {
				return err
			}
			client, err := newGitHub(secret)
			if err != nil {
				return err
			}
			return client.CheckAccess(ctx, scope)
		},
		Nudge: func() {
			select {
			case nudge <- struct{}{}:
			default:
			}
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if !server.Auth().Configured(ctx) {
		log.Warn("no password is set, so nothing can log in yet. Set one with: runner-fleet passwd")
	}

	// Bound before anything else starts, and reported as a failure if it
	// cannot be. A daemon that logs "address already in use" and then exits
	// zero tells systemd it finished successfully, which is how a service that
	// never came up ends up reading as "Deactivated successfully" — and it is
	// what the unit's restart policy sees, so it never retries either.
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w\n"+
			"  Something else on this host has that address. Pick another with --addr,\n"+
			"  or reinstall with ADDR=127.0.0.1:8181 to write it into the unit.\n"+
			"  To see what has it:  sudo ss -ltnp | grep %s", *addr, err, portOf(*addr))
	}

	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serving := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "version", version)
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serving <- err
			return
		}
		serving <- nil
	}()

	// One image at a time, in a goroutine of its own: a build takes minutes
	// and the fleet has to go on being reconciled while it happens.
	go builder.Run(ctx)

	go reconcileLoop(ctx, reconciler, *interval, nudge, log)
	// On a clock of its own rather than on the reconciler's. A pass that scaled
	// up comes back in three seconds and the host's history should not be
	// dense wherever the fleet was busy and sparse everywhere else — an evenly
	// spaced record is the one a chart can be read off.
	go sampler.Run(ctx, *sampleInterval)
	// On a clock of its own too, and a slow one. Collecting is minutes of
	// nothing followed by deleting a few gigabytes, and there is no reason for
	// it to share the reconciler's cadence or to hold up a pass while it walks
	// the disk.
	go collectLoop(ctx, db, imagebuild.NewGC(layout.ImagesDir(),
		filepath.Join(layout.State, "vms"), 0), *collectInterval, log)

	select {
	case err := <-serving:
		// The listener was taken away underneath it. Failing loudly is the
		// point: the runners are fine, but nobody can see or change anything.
		if err != nil {
			return fmt.Errorf("the web server stopped: %w", err)
		}
	case <-ctx.Done():
	}

	// Stopping the daemon stops the daemon. The runners are units and
	// containers of their own and are deliberately left exactly as they are:
	// an upgrade is a restart of this process and must not cost a single job.
	log.Info("shutting down; the runners keep running")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// sweep deletes the working directories of machines that no longer exist.
//
// What counts as existing is answered from the environment files rather than
// from systemd: a unit that failed is still a runner this host is responsible
// for, and its disk is not garbage. A directory something still has open is
// left alone whatever the environment files say — that is the one case they
// cannot cover, a daemon that went down between asking a machine to stop and
// seeing it stop.
func sweep(layout paths.Layout, log *slog.Logger) {
	known := func(runner string) bool {
		_, err := os.Stat(layout.RunnerEnv(runner))
		return err == nil
	}
	swept, freed, err := agent.SweepVMDirs(layout.State, known, agent.InUseByProcess)
	if err != nil {
		// Reported and carried on with. A directory that could not be deleted
		// is disk this host does not get back, which is worth saying and is not
		// worth refusing to start over.
		log.Warn("could not sweep every abandoned machine directory", "error", err)
	}
	if len(swept) > 0 {
		log.Info("swept abandoned machine directories",
			"runners", strings.Join(swept, ","), "freed", human(freed))
	}
}

// collectLoop deletes golden images nothing wants, for ever.
//
// The set of images the fleet wants is rebuilt from the pools on every pass
// rather than remembered, because it changes whenever somebody edits a recipe —
// and the whole reason images accumulate is that editing one mints another.
func collectLoop(ctx context.Context, db *store.Store, gc *imagebuild.GC,
	interval time.Duration, log *slog.Logger) {

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	pass := func() {
		pools, err := db.ListPools(ctx)
		if err != nil {
			log.Warn("could not read the pools to collect images", "error", err)
			return
		}
		// Every pool's image, including the pools that are switched off: a pool
		// somebody disabled for the afternoon should not have to rebuild.
		wanted := map[string]bool{}
		for _, pool := range pools {
			if pool.Runtime == model.RuntimeVM {
				wanted[imagebuild.Image(pool)] = true
			}
		}

		// The fleet's disk ceiling, read each pass so that lowering it in the
		// UI takes effect at the next collection rather than at the next
		// restart.
		var ceiling int64
		if stored, err := db.Setting(ctx, model.SettingFleetBudget); err == nil {
			if budget, err := model.ParseBudget(stored); err == nil {
				ceiling = int64(budget.DiskGB) * 1024 * 1024 * 1024
			}
		}
		gc.WithCeiling(ceiling)

		collected, err := gc.Collect(ctx, wanted)
		if err != nil {
			log.Warn("could not collect golden images", "error", err)
			return
		}
		if len(collected.Deleted) > 0 {
			log.Info("collected golden images",
				"images", strings.Join(collected.Deleted, ","),
				"freed", human(collected.Freed), "kept", collected.Kept,
				"onDisk", human(collected.Total))
		}
	}

	pass()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass()
		}
	}
}

// human is a byte count as somebody would say it, for a log line.
func human(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exponent := float64(bytes)/unit, 0
	for value >= unit && exponent < 3 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGT"[exponent])
}

// portOf is the last colon-separated part of a listen address, for a message
// that tells someone what to grep for.
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

func reconcileLoop(ctx context.Context, reconciler *reconcile.Reconciler, interval time.Duration, nudge <-chan struct{}, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// A pool grows by one runner per pass, so at the normal interval a burst of
	// ten jobs would take five minutes to be met. When a pass scaled up, the
	// next one comes almost immediately instead, and the fleet climbs in
	// seconds while demand lasts.
	const rampInterval = 3 * time.Second
	next := interval

	pass := func() {
		result := reconciler.Once(ctx)
		for _, message := range result.Errors {
			log.Warn("reconcile", "problem", message)
		}
		for pool, scale := range result.Scaling {
			if scale.ScaledUp {
				log.Info("scaling up", "pool", pool, "target", scale.Target,
					"ceiling", scale.Ceiling, "reason", scale.Reason)
			}
		}
		if result.ScaledUp && rampInterval < interval {
			next = rampInterval
		} else {
			next = interval
		}
		ticker.Reset(next)
	}

	// Once at startup, which is where a restarted daemon adopts the fleet it
	// finds rather than rebuilding it.
	pass()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass()
		case <-nudge:
			// A change made in the UI takes effect while the operator is still
			// looking at it, rather than at the next tick.
			pass()
		}
	}
}

// credentialWriter hands a decrypted token to the runners.
//
// On tmpfs, 0600, owned by the account the runners run as: the database keeps
// credentials encrypted, but a machine has to be able to mint a registration
// token on its own — after a reboot, with the daemon still starting — so the
// clear copy has to exist somewhere. /run is the one place it can that never
// reaches a disk.
//
// The ownership is the part that was missing, and it broke every machine
// runner on a packaged install: the daemon is root, the agents are not, and
// 0600 root-owned is unreadable to them. "Written where a runner can read it"
// is the whole job of this function, so the handover happens here rather than
// being left to whoever creates the directory.
func credentialWriter(layout paths.Layout, owner paths.Owner) reconcile.CredentialWriter {
	return func(id int64, token string) error {
		if err := os.MkdirAll(layout.CredentialsDir(), 0o700); err != nil {
			return err
		}
		if err := owner.Give(layout.CredentialsDir()); err != nil {
			return err
		}
		path := layout.Credential(id)
		if current, err := os.ReadFile(path); err == nil && string(current) == token {
			// Unchanged, but the owner may not be: a daemon upgraded from a
			// version that wrote these as root finds them already correct and
			// would otherwise leave them unreadable for ever.
			return owner.Give(path)
		}
		// Written beside and renamed into place, so a runner reading it never
		// sees a half-written file — and handed over before the rename, so it
		// is never briefly there and unreadable.
		staged := path + ".new"
		if err := os.WriteFile(staged, []byte(token), 0o600); err != nil {
			return fmt.Errorf("write the credential: %w", err)
		}
		if err := owner.Give(staged); err != nil {
			return err
		}
		return os.Rename(staged, path)
	}
}

func uiAssets(log *slog.Logger) fs.FS {
	assets, err := ui.Assets()
	if err != nil {
		log.Warn("this binary was built without the web UI; the API still works", "error", err)
		return nil
	}
	return assets
}
