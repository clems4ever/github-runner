// Package api serves the fleet: a small REST surface and the UI that drives
// it.
//
// It listens on the loopback address by default. The daemon can create virtual
// machines and holds a credential that administers repositories, so the first
// question is not who the user is but whether anything else on the host can
// reach it at all.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/reconcile"
	"github.com/clems4ever/github-runner/internal/store"
)

// Store is what the API needs from the database.
type Store interface {
	SettingsStore
	ListPools(ctx context.Context) ([]model.Pool, error)
	Pool(ctx context.Context, id int64) (model.Pool, error)
	CreatePool(ctx context.Context, p model.Pool) (model.Pool, error)
	UpdatePool(ctx context.Context, p model.Pool) (model.Pool, error)
	DeletePool(ctx context.Context, id int64) error
	Activity(ctx context.Context, since, until time.Time, buckets int, pool string) ([]model.ActivityPoint, error)
	ListCredentials(ctx context.Context) ([]model.Credential, error)
	CreateCredential(ctx context.Context, credential model.Credential, secret string) (model.Credential, error)
	ReplaceCredentialSecret(ctx context.Context, id int64, secret string) error
	DeleteCredential(ctx context.Context, id int64) error
}

// Fleet is what the API needs from the reconciler.
type Fleet interface {
	Status(ctx context.Context) ([]reconcile.RunnerStatus, []string)
	Once(ctx context.Context) reconcile.Result
	Scaling() map[string]reconcile.Scale
}

// Server is the HTTP surface.
type Server struct {
	store   Store
	fleet   Fleet
	auth    *Authenticator
	ui      fs.FS
	version string
	check   CheckAccess
	// nudge asks the daemon to reconcile now rather than at the next tick, so
	// a change made in the UI takes effect while the operator is still looking
	// at it.
	nudge func()
}

// CheckAccess asks GitHub whether a credential can do what a pool will need,
// before the pool is created. Injected, so the API can be tested without one.
type CheckAccess func(ctx context.Context, credentialID int64, scope github.Scope) error

// Options configures the server.
type Options struct {
	Store       Store
	Fleet       Fleet
	UI          fs.FS
	Version     string
	Nudge       func()
	CheckAccess CheckAccess
}

// New builds the server.
func New(opts Options) *Server {
	s := &Server{
		store:   opts.Store,
		fleet:   opts.Fleet,
		auth:    NewAuthenticator(opts.Store),
		ui:      opts.UI,
		version: opts.Version,
		nudge:   opts.Nudge,
		check:   opts.CheckAccess,
	}
	if s.nudge == nil {
		s.nudge = func() {}
	}
	return s
}

// Auth exposes the authenticator so the daemon can set the first password.
func (s *Server) Auth() *Authenticator { return s.auth }

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/pools", s.listPools)
	api.HandleFunc("POST /api/pools", s.createPool)
	api.HandleFunc("GET /api/pools/{id}", s.getPool)
	api.HandleFunc("PUT /api/pools/{id}", s.updatePool)
	api.HandleFunc("DELETE /api/pools/{id}", s.deletePool)
	api.HandleFunc("GET /api/runners", s.listRunners)
	api.HandleFunc("POST /api/reconcile", s.reconcileNow)
	api.HandleFunc("GET /api/activity", s.activity)
	api.HandleFunc("GET /api/credentials", s.listCredentials)
	api.HandleFunc("POST /api/credentials", s.createCredential)
	api.HandleFunc("PUT /api/credentials/{id}/secret", s.rotateCredential)
	api.HandleFunc("DELETE /api/credentials/{id}", s.deleteCredential)
	api.HandleFunc("PUT /api/settings/auth", s.setPassword)
	api.HandleFunc("GET /api/settings", s.getSettings)

	root := http.NewServeMux()
	// Health is the one route without authentication: it says whether the
	// daemon is up, and nothing about the fleet.
	root.HandleFunc("GET /api/health", s.health)
	root.Handle("/api/", s.auth.Middleware(api))
	root.Handle("/", s.auth.Middleware(s.uiHandler()))
	return securityHeaders(root)
}

// securityHeaders are the ones that matter for a single-page app served over a
// loopback connection.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		// Whether anyone can log in yet, so the UI can tell a fresh install
		// from a locked one.
		"configured": s.auth.Configured(r.Context()),
	})
}

// uiHandler serves the built single-page app, falling back to index.html so a
// deep link the browser asks for directly still loads.
func (s *Server) uiHandler() http.Handler {
	if s.ui == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "the UI was not built into this binary", http.StatusNotFound)
		})
	}
	files := http.FileServer(http.FS(s.ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(s.ui, path); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Pools
// ---------------------------------------------------------------------------

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.store.ListPools(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pools)
}

func (s *Server) getPool(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	pool, err := s.store.Pool(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pool)
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	var pool model.Pool
	if err := decode(r, &pool); err != nil {
		writeError(w, err)
		return
	}
	pool.ID = 0
	if err := s.reachable(r.Context(), pool); err != nil {
		writeError(w, err)
		return
	}
	created, err := s.store.CreatePool(r.Context(), pool)
	if err != nil {
		writeError(w, err)
		return
	}
	s.nudge()
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) updatePool(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var pool model.Pool
	if err := decode(r, &pool); err != nil {
		writeError(w, err)
		return
	}
	pool.ID = id
	if err := s.reachable(r.Context(), pool); err != nil {
		writeError(w, err)
		return
	}
	updated, err := s.store.UpdatePool(r.Context(), pool)
	if err != nil {
		writeError(w, err)
		return
	}
	s.nudge()
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeletePool(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	// The runners are not touched here. The next pass drains them, which is
	// what keeps deleting a pool from failing a job that is in flight.
	s.nudge()
	w.WriteHeader(http.StatusNoContent)
}

// reachable refuses a pool whose credential GitHub says cannot serve it.
//
// Only when GitHub gave a definite answer. A daemon that cannot reach GitHub at
// all must not stop someone configuring their fleet — the pool will work when
// the network does, and the reconcile loop reports what it finds either way.
func (s *Server) reachable(ctx context.Context, pool model.Pool) error {
	if s.check == nil || !pool.Enabled {
		return nil
	}
	err := s.check(ctx, pool.CredentialID, github.Scope{Kind: pool.ScopeKind, Path: pool.Scope})
	var apiErr *github.Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return nil
}

// ---------------------------------------------------------------------------
// Runners
// ---------------------------------------------------------------------------

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	runners, errs := s.fleet.Status(r.Context())
	// The scaling decisions go with them: a pool that resized itself should
	// never leave anyone wondering what it reacted to.
	writeJSON(w, http.StatusOK, map[string]any{
		"runners":  runners,
		"warnings": errs,
		"scaling":  s.fleet.Scaling(),
	})
}

// activity is the fleet's history: how many runners existed and how many were
// working, over a window.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	hours := 6
	if raw := r.URL.Query().Get("hours"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 48 {
			writeError(w, errBadRequest("hours must be a whole number from 1 to 48"))
			return
		}
		hours = parsed
	}

	// Enough points for a chart to have shape, few enough that the browser is
	// drawing a picture rather than a spreadsheet.
	const buckets = 180
	until := time.Now().UTC()
	since := until.Add(-time.Duration(hours) * time.Hour)

	// An unknown pool is not an error: it comes back empty, which is what a
	// pool created a moment ago honestly has.
	pool := r.URL.Query().Get("pool")

	points, err := s.store.Activity(r.Context(), since, until, buckets, pool)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"points": points,
		"pool":   pool,
		"since":  since,
		"until":  until,
	})
}

func (s *Server) reconcileNow(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.fleet.Once(r.Context()))
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	credentials, err := s.store.ListCredentials(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, credentials)
}

func (s *Server) createCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		// Secret is the personal access token, or the app's PEM private key.
		Secret         string `json:"secret"`
		AppID          int64  `json:"appId"`
		InstallationID int64  `json:"installationId"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, err)
		return
	}
	kind := model.CredentialKind(body.Kind)
	if kind == "" {
		kind = model.CredentialPAT
	}
	created, err := s.store.CreateCredential(r.Context(), model.Credential{
		Name:           body.Name,
		Kind:           kind,
		AppID:          body.AppID,
		InstallationID: body.InstallationID,
	}, body.Secret)
	if err != nil {
		writeError(w, err)
		return
	}
	// The response carries the name and a hint, never the secret: it goes in
	// once and only ever comes back out as a runner's registration.
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) rotateCredential(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Secret string `json:"secret"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.ReplaceCredentialSecret(r.Context(), id, body.Secret); err != nil {
		writeError(w, err)
		return
	}
	// Rotating changes every affected pool's generation, so the next pass
	// replaces their runners gracefully rather than leaving them holding a
	// token that no longer works.
	s.nudge()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteCredential(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	user, err := s.store.Setting(r.Context(), SettingAuthUser)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authUser": user, "version": s.version})
}

func (s *Server) setPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if err := s.auth.SetPassword(r.Context(), body.User, body.Password); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

type badRequest struct{ message string }

func (e *badRequest) Error() string { return e.message }

func errBadRequest(format string, args ...any) error {
	return &badRequest{message: fmt.Sprintf(format, args...)}
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, errBadRequest("%q is not an id", r.PathValue("id"))
	}
	return id, nil
}

func decode(r *http.Request, into any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	// Unknown fields are rejected: a typo in a field name that silently did
	// nothing would be a setting the operator believes is applied.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errBadRequest("could not read the request: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError maps the store's vocabulary onto status codes, so the UI can tell
// "you asked for something that is not there" from "that name is taken" from
// "this is not going to work".
func writeError(w http.ResponseWriter, err error) {
	// GitHub refusing is the operator's problem to fix, not a fault of this
	// daemon, and sometimes there is a page that fixes it.
	var apiErr *github.Error
	if errors.As(err, &apiErr) {
		body := map[string]string{"error": apiErr.Error()}
		if apiErr.GrantURL != "" {
			body["grantUrl"] = apiErr.GrantURL
		}
		writeJSON(w, http.StatusBadRequest, body)
		return
	}

	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrInUse):
		status = http.StatusConflict
	default:
		var bad *badRequest
		if errors.As(err, &bad) {
			status = http.StatusBadRequest
		} else if isValidation(err) {
			status = http.StatusBadRequest
		}
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// isValidation recognises the model's own complaints, which are written for a
// person and should reach one rather than becoming a 500.
func isValidation(err error) bool {
	message := err.Error()
	for _, prefix := range []string{
		"name ", "scope ", "runtime ", "replicas ", "cpus ", "memory ", "disk ", "label ",
		"a credential is required", "the secret is empty", "a credential needs a name",
		"an app needs its app id", "the app's private key", "credential kind ",
	} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return false
}
