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
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/imagebuild"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/reconcile"
	"github.com/clems4ever/github-runner/internal/resources"
	"github.com/clems4ever/github-runner/internal/store"
	"github.com/clems4ever/github-runner/internal/template"
)

// Store is what the API needs from the database.
type Store interface {
	SettingsStore
	ListPools(ctx context.Context) ([]model.Pool, error)
	Pool(ctx context.Context, id int64) (model.Pool, error)
	CreatePool(ctx context.Context, p model.Pool) (model.Pool, error)
	UpdatePool(ctx context.Context, p model.Pool) (model.Pool, error)
	DeletePool(ctx context.Context, id int64) error
	ImportPools(ctx context.Context, pools []model.Pool, replaceExisting, dryRun bool) ([]store.ImportOutcome, error)
	Activity(ctx context.Context, since, until time.Time, buckets int, filter store.ActivityFilter) ([]model.ActivityPoint, error)
	ActivityScopes(ctx context.Context, since, until time.Time) ([]string, error)
	JobHistory(ctx context.Context, since, until time.Time) ([]model.JobDay, error)
	HostHistory(ctx context.Context, since, until time.Time, buckets int) ([]model.HostPoint, error)
	ListCredentials(ctx context.Context) ([]model.Credential, error)
	CreateCredential(ctx context.Context, credential model.Credential, secret string) (model.Credential, error)
	ReplaceCredentialSecret(ctx context.Context, id int64, secret string) error
	DeleteCredential(ctx context.Context, id int64) error
	ListRepoLayers(ctx context.Context, pool string) ([]model.RepoLayer, error)
	RepoLayerByID(ctx context.Context, id int64) (model.RepoLayer, error)
	DecideRepoLayer(ctx context.Context, id int64, approval model.LayerApproval, by string) (model.RepoLayer, error)
}

// Fleet is what the API needs from the reconciler.
type Fleet interface {
	Status(ctx context.Context) ([]reconcile.RunnerStatus, []string)
	Once(ctx context.Context) reconcile.Result
	Scaling() map[string]reconcile.Scale
}

// Resources is what the API needs from the resource sampler: the most recent
// reading, and whether one has been taken yet.
type Resources interface {
	Latest() (resources.Report, bool)
}

// Images is what the API needs to show a pool's golden image: where it stands,
// what every attempt at it did, and how to ask for another.
//
// A machine pool has no runners until its image is built, so this is not
// decoration — it is the answer to "why is this pool empty", and it belongs to
// the pool rather than to the host.
type Images interface {
	Status(pool model.Pool) imagebuild.Status
	// Forget drops a deleted pool's builds, and the consoles they left on the
	// host.
	Forget(ctx context.Context, pool string) error
	History(ctx context.Context, pool string, limit int) ([]imagebuild.Build, error)
	Log(ctx context.Context, id int64, maxBytes int64) (string, error)
	Rebuild(ctx context.Context, pool model.Pool) (imagebuild.Build, error)
}

// Layers is what the API needs from the layer resolver.
//
// One method, and it is a cache invalidation: the resolver reads a repository's
// definition a few times an hour rather than on every pass, and a decision made
// in the UI has to take effect now rather than whenever that interval next
// comes round.
type Layers interface {
	Forget(pool string)
}

// Server is the HTTP surface.
type Server struct {
	store     Store
	fleet     Fleet
	resources Resources
	images    Images
	layers    Layers
	auth      *Authenticator
	ui        fs.FS
	version   string
	check     CheckAccess
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
	Store Store
	Fleet Fleet
	// Resources may be nil, which is a daemon that serves everything else and
	// says it has not measured the host.
	Resources Resources
	// Images may be nil, which is a daemon that builds nothing: every pool
	// reports that there is no image of its own to build, which is what a
	// container-only fleet is.
	Images Images
	// Layers may be nil, which is a daemon that reads no repository
	// definitions: the endpoints still serve what the database remembers, and
	// a decision simply takes effect at the next pass.
	Layers      Layers
	UI          fs.FS
	Version     string
	Nudge       func()
	CheckAccess CheckAccess
}

// New builds the server.
func New(opts Options) *Server {
	s := &Server{
		store:     opts.Store,
		fleet:     opts.Fleet,
		resources: opts.Resources,
		images:    opts.Images,
		layers:    opts.Layers,
		auth:      NewAuthenticator(opts.Store),
		ui:        opts.UI,
		version:   opts.Version,
		nudge:     opts.Nudge,
		check:     opts.CheckAccess,
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
	api.HandleFunc("POST /api/pools/import", s.importPools)
	api.HandleFunc("GET /api/pools/export", s.exportPools)
	api.HandleFunc("GET /api/pools/{id}", s.getPool)
	api.HandleFunc("PUT /api/pools/{id}", s.updatePool)
	api.HandleFunc("DELETE /api/pools/{id}", s.deletePool)
	api.HandleFunc("GET /api/runners", s.listRunners)
	api.HandleFunc("POST /api/reconcile", s.reconcileNow)
	api.HandleFunc("GET /api/activity", s.activity)
	api.HandleFunc("GET /api/jobs", s.jobs)
	api.HandleFunc("GET /api/pool-images", s.poolImages)
	api.HandleFunc("GET /api/pools/{id}/image", s.poolImage)
	api.HandleFunc("POST /api/pools/{id}/image/builds", s.buildPoolImage)
	api.HandleFunc("GET /api/image-builds/{id}/log", s.imageBuildLog)
	api.HandleFunc("GET /api/layers", s.listLayers)
	api.HandleFunc("POST /api/layers/{id}/decision", s.decideLayer)
	api.HandleFunc("GET /api/resources", s.resourceReport)
	api.HandleFunc("GET /api/resources/history", s.resourceHistory)
	api.HandleFunc("GET /api/credentials", s.listCredentials)
	api.HandleFunc("POST /api/credentials", s.createCredential)
	api.HandleFunc("PUT /api/credentials/{id}/secret", s.rotateCredential)
	api.HandleFunc("DELETE /api/credentials/{id}", s.deleteCredential)
	api.HandleFunc("PUT /api/settings/auth", s.setPassword)
	api.HandleFunc("PUT /api/settings/budget", s.setBudget)
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
	pool, err := s.poolFor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeletePool(r.Context(), pool.ID); err != nil {
		writeError(w, err)
		return
	}
	// Its image builds go with it. They are filed under the pool, so once it
	// is gone there is nowhere left to read them from, and their logs are
	// consoles worth megabytes each.
	if s.images != nil {
		if err := s.images.Forget(r.Context(), pool.Name); err != nil {
			// Not a reason to report the deletion as failed: the pool is gone,
			// which is what was asked for.
			slog.Warn("could not forget a deleted pool's image builds",
				"pool", pool.Name, "error", err)
		}
	}
	// The runners are not touched here. The next pass drains them, which is
	// what keeps deleting a pool from failing a job that is in flight.
	s.nudge()
	w.WriteHeader(http.StatusNoContent)
}

// importPools writes a template's pools into the fleet.
//
// The document arrives as it was written — the daemon does not accept a list of
// pools, it accepts a template — so a file someone was handed can be pasted
// whole, and anything wrong with it is reported in the template's own terms.
func (s *Server) importPools(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Document json.RawMessage `json:"document"`
		// CredentialID is what the imported pools register with. A template
		// cannot name one: credentials are local to the host they were sealed
		// on.
		CredentialID int64 `json:"credentialId"`
		// Scope, when given, replaces the scope of every pool in the document.
		Scope     string          `json:"scope"`
		ScopeKind model.ScopeKind `json:"scopeKind"`
		// ReplaceExisting imports over pools of the same name instead of
		// refusing.
		ReplaceExisting bool `json:"replaceExisting"`
		// DryRun reports what would happen and writes nothing.
		DryRun bool `json:"dryRun"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if len(body.Document) == 0 {
		writeError(w, errBadRequest("there is no template here: paste one, or choose a file"))
		return
	}

	doc, err := template.Parse(body.Document)
	if err != nil {
		writeError(w, errBadRequest("%v", err))
		return
	}
	pools, err := template.Apply(doc, template.Options{
		CredentialID: body.CredentialID,
		Scope:        body.Scope,
		ScopeKind:    body.ScopeKind,
	})
	if err != nil {
		writeError(w, errBadRequest("%v", err))
		return
	}

	// Asked before anything is written, and during a dry run too: a preview
	// that says "create" for a pool GitHub will refuse is a preview that lied.
	asked := map[string]bool{}
	for _, pool := range pools {
		key := string(pool.ScopeKind) + ":" + pool.Scope
		if asked[key] {
			continue
		}
		asked[key] = true
		if err := s.reachable(r.Context(), pool); err != nil {
			writeError(w, err)
			return
		}
	}

	outcomes, err := s.store.ImportPools(r.Context(), pools, body.ReplaceExisting, body.DryRun)
	if err != nil {
		writeError(w, err)
		return
	}
	if !body.DryRun {
		s.nudge()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pools":       outcomes,
		"dryRun":      body.DryRun,
		"name":        doc.Name,
		"description": doc.Description,
	})
}

// exportPools writes the fleet out as a template someone can keep.
//
// Indented, because the point of it is to be read, edited and committed
// somewhere rather than handed straight back to a machine.
func (s *Server) exportPools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.store.ListPools(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	body, err := json.MarshalIndent(template.Export(pools), "", "  ")
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="runner-fleet-pools.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
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
	since, until, err := window(r)
	if err != nil {
		writeError(w, err)
		return
	}

	// An unknown pool or scope is not an error: it comes back empty, which is
	// what a pool created a moment ago honestly has.
	filter := store.ActivityFilter{
		Pool:  r.URL.Query().Get("pool"),
		Scope: r.URL.Query().Get("scope"),
	}

	points, err := s.store.Activity(r.Context(), since, until, buckets, filter)
	if err != nil {
		writeError(w, err)
		return
	}
	// The scopes are what the window has history for, sent whatever the filter
	// is: a reader narrowed to one repository still needs the others in the
	// list to be able to leave it.
	scopes, err := s.store.ActivityScopes(r.Context(), since, until)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"points": points,
		"pool":   filter.Pool,
		"scope":  filter.Scope,
		"scopes": scopes,
		"since":  since,
		"until":  until,
	})
}

// jobs is what each pool has run: how many jobs, and how much runner-time went
// on them, a day at a time.
//
// Separate from the activity history above, and asked in days rather than
// hours, because it answers a different question. Activity is what the fleet
// was doing this afternoon; this is what a pool has cost over a quarter, which
// is the evidence somebody brings to the decision to make it bigger.
//
// The counts are observations, not GitHub's own accounting: see the reconciler
// for what that costs in accuracy.
func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	since, until, err := dayWindow(r)
	if err != nil {
		writeError(w, err)
		return
	}

	days, err := s.store.JobHistory(r.Context(), since, until)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pools": totalPerPool(days),
		"days":  days,
		"since": since,
		"until": until,
	})
}

// totalPerPool adds a window up per pool, the hungriest first — the order
// somebody scanning for what to resize is reading in.
func totalPerPool(days []model.JobDay) []model.PoolJobs {
	index := map[string]int{}
	out := []model.PoolJobs{}
	for _, day := range days {
		at, seen := index[day.Pool]
		if !seen {
			at = len(out)
			index[day.Pool] = at
			out = append(out, model.PoolJobs{Pool: day.Pool})
		}
		out[at].Jobs += day.Jobs
		out[at].Seconds += day.Seconds
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seconds != out[j].Seconds {
			return out[i].Seconds > out[j].Seconds
		}
		return out[i].Pool < out[j].Pool
	})
	return out
}

// maxJobDays is as far back as a jobs window may reach: everything the daemon
// still has, and nothing beyond it, so the two never drift apart.
const maxJobDays = int(store.JobRetention / (24 * time.Hour))

// dayWindow is the span the job accounting is asked about.
//
// Whole UTC days, unlike the charts' rolling window of hours, because the tally
// itself is per day: a window that started at half past two would take in a day
// the daemon can only report whole, and report two thirds of it as all of it.
func dayWindow(r *http.Request) (since, until time.Time, err error) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxJobDays {
			return time.Time{}, time.Time{},
				errBadRequest("days must be a whole number from 1 to %d", maxJobDays)
		}
		days = parsed
	}
	until = time.Now().UTC()
	// Today counts, so seven days is today and the six before it rather than
	// today and the seven before it.
	today := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
	return today.AddDate(0, 0, -(days - 1)), until, nil
}

// buckets is how many points a history window is reduced to: enough for a
// chart to have shape, few enough that the browser is drawing a picture rather
// than a spreadsheet.
const buckets = 180

// window is the span a history request is asking about. It is shared by every
// chart, so that "6h" means the same thing and lines up on all of them.
func window(r *http.Request) (since, until time.Time, err error) {
	hours := 6
	if raw := r.URL.Query().Get("hours"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 48 {
			return time.Time{}, time.Time{}, errBadRequest("hours must be a whole number from 1 to 48")
		}
		hours = parsed
	}
	until = time.Now().UTC()
	return until.Add(-time.Duration(hours) * time.Hour), until, nil
}

func (s *Server) reconcileNow(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.fleet.Once(r.Context()))
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

// poolImages says where every pool's image stands, which is what the pools
// table shows against each row.
func (s *Server) poolImages(w http.ResponseWriter, r *http.Request) {
	pools, err := s.store.ListPools(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]imagebuild.Status, 0, len(pools))
	for _, pool := range pools {
		out = append(out, s.imageStatus(pool))
	}
	writeJSON(w, http.StatusOK, out)
}

// poolImage is one pool's image and every attempt this host has made at it.
//
// The history is the part that used to be missing. A build that failed was
// replaced by the next attempt at the same thing, so the account of what a
// recipe did survived only until something tried again — which, when a unit
// was retrying every two seconds, was no time at all.
func (s *Server) poolImage(w http.ResponseWriter, r *http.Request) {
	pool, err := s.poolFor(r)
	if err != nil {
		writeError(w, err)
		return
	}

	body := map[string]any{"status": s.imageStatus(pool), "builds": []imagebuild.Build{}}
	if s.images == nil {
		writeJSON(w, http.StatusOK, body)
		return
	}
	history, err := s.images.History(r.Context(), pool.Name, 0)
	if err != nil {
		writeError(w, err)
		return
	}
	if history != nil {
		body["builds"] = history
	}
	writeJSON(w, http.StatusOK, body)
}

// buildPoolImage is somebody asking for a build: the first one for a pool that
// is switched off, or another after one failed.
//
// Asked for rather than automatic, because a build that failed is not tried
// again on its own. A recipe that cannot work should say so once and wait for
// somebody to change it.
func (s *Server) buildPoolImage(w http.ResponseWriter, r *http.Request) {
	pool, err := s.poolFor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.images == nil {
		writeError(w, errBadRequest("this daemon does not build images"))
		return
	}
	build, err := s.images.Rebuild(r.Context(), pool)
	if errors.Is(err, imagebuild.ErrBusy) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	// The fleet is told, so a pool waiting on this image picks its runners up
	// as soon as it finishes rather than at the next tick.
	s.nudge()
	writeJSON(w, http.StatusAccepted, build)
}

// imageBuildLog is the whole account of one build: what the daemon did, and
// everything the build machine printed, in order.
//
// Text rather than JSON, and the end of it rather than all of it. This is a
// console — it is read, not parsed — and a browser has no business holding the
// eight megabytes an apt run can produce.
func (s *Server) imageBuildLog(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.images == nil {
		writeError(w, errBadRequest("this daemon does not build images"))
		return
	}
	// How much of the end of it to send. The default is generous; a page
	// polling a build in progress asks for less.
	var bytes int64
	if raw := r.URL.Query().Get("bytes"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, errBadRequest("bytes must be a number of bytes"))
			return
		}
		bytes = int64(parsed)
	}

	log, err := s.images.Log(r.Context(), id, bytes)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(log))
}

// imageStatus is where a pool's image stands, or the answer a daemon that
// builds nothing gives.
func (s *Server) imageStatus(pool model.Pool) imagebuild.Status {
	if s.images == nil {
		return imagebuild.Status{
			Pool: pool.Name, State: imagebuild.StateNone, Ready: true,
			Summary: "this daemon does not build images",
		}
	}
	return s.images.Status(pool)
}

// poolFor is the pool a request names.
func (s *Server) poolFor(r *http.Request) (model.Pool, error) {
	id, err := pathID(r)
	if err != nil {
		return model.Pool{}, err
	}
	return s.store.Pool(r.Context(), id)
}

// ---------------------------------------------------------------------------
// Resources
// ---------------------------------------------------------------------------

// resourceReport is what the host and its runners are using now, next to what
// the pools have promised.
//
// The reading is the sampler's most recent one rather than a fresh one taken
// here. A percentage is the difference between two readings at a known
// cadence, so a request that took its own would be measuring a window it had
// just stolen from the next one — and every open browser tab would be doing
// it. The response says when the reading was taken, so nobody has to guess how
// fresh it is.
func (s *Server) resourceReport(w http.ResponseWriter, r *http.Request) {
	if s.resources == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ready": false})
		return
	}
	report, ready := s.resources.Latest()
	if !ready {
		// Not an error: the daemon has been up for less than one sample. The
		// UI says so rather than drawing a host with no memory.
		writeJSON(w, http.StatusOK, map[string]any{"ready": false})
		return
	}

	pools, err := s.store.ListPools(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	// The budget goes out beside the commitment deliberately: one is what the
	// pools would take at full stretch and the other is what they are allowed
	// to, and the interesting number is the gap between them.
	budget, err := s.budget(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":     true,
		"at":        report.At,
		"host":      report.Host,
		"runners":   report.Runners,
		"warnings":  report.Warnings,
		"committed": model.Commit(pools),
		"budget":    budget,
	})
}

// resourceHistory is what the host has been using, over a window.
func (s *Server) resourceHistory(w http.ResponseWriter, r *http.Request) {
	since, until, err := window(r)
	if err != nil {
		writeError(w, err)
		return
	}
	points, err := s.store.HostHistory(r.Context(), since, until, buckets)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"points": points,
		"since":  since,
		"until":  until,
	})
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
	budget, err := s.budget(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authUser": user, "version": s.version, "budget": budget,
	})
}

// budget is what the whole fleet may take from this host.
//
// A stored budget that cannot be read is an error here, unlike in the
// reconciler, where it is a warning and the fleet carries on. The difference is
// who is asking: a daemon maintaining a fleet must not stop over a settings
// row, and a person who has opened the settings page to look at the budget is
// owed the truth about it.
func (s *Server) budget(ctx context.Context) (model.Budget, error) {
	stored, err := s.store.Setting(ctx, model.SettingFleetBudget)
	if err != nil {
		return model.Budget{}, err
	}
	return model.ParseBudget(stored)
}

func (s *Server) setBudget(w http.ResponseWriter, r *http.Request) {
	var budget model.Budget
	if err := decode(r, &budget); err != nil {
		writeError(w, err)
		return
	}
	if err := budget.Validate(); err != nil {
		writeError(w, errBadRequest("%s", err))
		return
	}
	encoded, err := budget.Encode()
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.SetSetting(r.Context(), model.SettingFleetBudget, encoded); err != nil {
		writeError(w, err)
		return
	}
	// The budget reaches the host on a reconcile pass — the slice is written
	// there, and the pools are rationed there — so ask for one now rather than
	// leaving the operator to wonder for half a minute whether it took.
	s.nudge()
	writeJSON(w, http.StatusOK, budget)
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
