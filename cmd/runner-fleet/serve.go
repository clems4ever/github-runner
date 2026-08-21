package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/clems4ever/github-runner/internal/api"
	"github.com/clems4ever/github-runner/internal/executor/docker"
	"github.com/clems4ever/github-runner/internal/executor/systemd"
	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/reconcile"
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
	user := flags.String("user", "runner-fleet", "the service user the runners' units run as")
	if err := flags.Parse(args); err != nil {
		return err
	}

	log := logger()
	layout := layoutFor(*root)
	if err := layout.EnsureDirs(); err != nil {
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

	// The reconciler is told how to reach GitHub and how to hand a credential
	// to a runner. Both are injected rather than reached for directly, which
	// is what lets the fleet's rules be tested without either.
	reconciler := reconcile.New(db,
		[]reconcile.Executor{vm, containers},
		func(token string) reconcile.GitHubClient { return github.New(token) },
		credentialWriter(layout),
		log)

	// A buffered channel of one: several changes arriving together collapse
	// into a single extra pass rather than queueing up.
	nudge := make(chan struct{}, 1)
	server := api.New(api.Options{
		Store:   db,
		Fleet:   reconciler,
		UI:      uiAssets(log),
		Version: version,
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

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", *addr, "version", version)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the web server stopped", "error", err)
			stop()
		}
	}()

	go reconcileLoop(ctx, reconciler, *interval, nudge, log)

	<-ctx.Done()

	// Stopping the daemon stops the daemon. The runners are units and
	// containers of their own and are deliberately left exactly as they are:
	// an upgrade is a restart of this process and must not cost a single job.
	log.Info("shutting down; the runners keep running")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

func reconcileLoop(ctx context.Context, reconciler *reconcile.Reconciler, interval time.Duration, nudge <-chan struct{}, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	pass := func() {
		result := reconciler.Once(ctx)
		for _, message := range result.Errors {
			log.Warn("reconcile", "problem", message)
		}
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
// On tmpfs, 0600: the database keeps credentials encrypted, but a runner has
// to be able to mint a registration token on its own — after a reboot, with
// the daemon still starting — so the clear copy has to exist somewhere. /run
// is the one place it can that never reaches a disk.
func credentialWriter(layout paths.Layout) reconcile.CredentialWriter {
	return func(id int64, token string) error {
		if err := os.MkdirAll(layout.CredentialsDir(), 0o700); err != nil {
			return err
		}
		path := layout.Credential(id)
		if current, err := os.ReadFile(path); err == nil && string(current) == token {
			return nil
		}
		// Written beside and renamed into place, so a runner reading it never
		// sees a half-written file.
		staged := path + ".new"
		if err := os.WriteFile(staged, []byte(token), 0o600); err != nil {
			return fmt.Errorf("write the credential: %w", err)
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
