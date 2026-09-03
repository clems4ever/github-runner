package api

import (
	"net/http"

	"github.com/clems4ever/github-runner/internal/model"
)

// The layers repositories have asked their pools for, and the one decision an
// operator makes about each.
//
// It is deliberately a small surface: list, approve, refuse. There is no
// endpoint that edits what a repository asked for, because the ask belongs to
// the repository — an operator who wants different packages puts them in the
// pool's own recipe, where they apply to everything the pool runs and are
// answered for by whoever wrote them.

// listLayers is every ask this host knows about, most recently seen first.
//
// ?pool= narrows it to one pool, which is what the pool's own page asks for;
// without it this is the fleet's queue of things waiting for a decision.
func (s *Server) listLayers(w http.ResponseWriter, r *http.Request) {
	layers, err := s.store.ListRepoLayers(r.Context(), r.URL.Query().Get("pool"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, layers)
}

// decideLayer is the decision itself.
//
// The digest is required in the body as well as the id in the path. They name
// the same row, and that is the point: an operator approves the thing they were
// shown. A repository that changed its file between the page being drawn and
// the button being pressed is a different row, and this refuses rather than
// attaching a decision to a script nobody read.
func (s *Server) decideLayer(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}

	var body struct {
		Approval string `json:"approval"`
		Digest   string `json:"digest"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, err)
		return
	}

	approval := model.LayerApproval(body.Approval)
	if approval != model.LayerApproved && approval != model.LayerRefused {
		writeError(w, errBadRequest("a layer is approved or refused, not %q", body.Approval))
		return
	}

	layer, err := s.store.RepoLayerByID(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if body.Digest == "" {
		writeError(w, errBadRequest(
			"say which definition this decision is about: send the digest that was shown"))
		return
	}
	if body.Digest != layer.Digest {
		writeError(w, errBadRequest(
			"%s has changed what it is asking for since this was shown. Read it again before deciding",
			layer.Repo))
		return
	}

	decided, err := s.store.DecideRepoLayer(r.Context(), id, approval, whoDecided(r))
	if err != nil {
		writeError(w, err)
		return
	}

	// The resolver holds its answer for minutes at a time so that it is not
	// reading GitHub on every pass. Without this, a decision made now would
	// take effect at some point in the next few — which reads as the button
	// not having worked.
	if s.layers != nil {
		s.layers.Forget(decided.Pool)
	}
	s.nudge()
	writeJSON(w, http.StatusOK, decided)
}

// whoDecided is the operator's name, for the question that gets asked months
// later. There is one account here, so this is thin — but a row that says
// "admin" is still better than a row that says nothing, and it will mean more
// the day there is more than one.
func whoDecided(r *http.Request) string {
	if user, _, ok := r.BasicAuth(); ok && user != "" {
		return user
	}
	return "operator"
}
