package model

import (
	"fmt"
	"time"
)

// A repository declares what its jobs need and the daemon builds it a layer.
// What is in this file is the part that decides whether it is allowed to.
//
// The thing being decided is not small. A recipe is a root shell on a build
// machine on the operator's host, and a package list is written into that
// machine's cloud-config. Anybody who can merge to the repository's default
// branch can change both. That is a reasonable thing to allow — it is the same
// set of people who can already change what a job runs — but it is not
// something to arrive at by accident, so it is off until an operator turns it
// on, per pool.

// LayerPolicy is how much a pool trusts the repositories it serves.
type LayerPolicy string

const (
	// LayersOff ignores repository definitions entirely. The default, and what
	// every pool that existed before this did.
	LayersOff LayerPolicy = "off"
	// LayersApprove builds a repository's layer once an operator has approved
	// that exact definition. A changed definition is a new one and waits
	// again; until then the pool keeps using the layer that was approved.
	//
	// This is the setting to want. It costs one click per change to a file
	// that changes rarely, and what it buys is that no merge to a repository
	// runs anything on the host that a human has not read.
	LayersApprove LayerPolicy = "approve"
	// LayersTrust builds whatever the repository asks for, unread.
	//
	// For a pool serving a repository whose default branch is already trusted
	// to the same degree as the host — which is a real situation and not a
	// rare one, since a workflow on that branch can already run anything on
	// the runner. What it gives up is the boundary between "can run a job" and
	// "can change the image every future job boots from".
	LayersTrust LayerPolicy = "trust"
)

// Valid reports whether a policy is one of the three.
func (p LayerPolicy) Valid() bool {
	switch p {
	case LayersOff, LayersApprove, LayersTrust, "":
		return true
	}
	return false
}

// LayersAllowed reports whether a pool may build layers at all, which is not
// only a question of policy.
//
// A layer is a repository's, and a runner has to be built from it before it
// knows which job it will take. That works when the pool serves one
// repository, because then there is only one answer. An organisation pool's
// runner has no idea which of the organisation's repositories will claim it,
// so there is no layer it could have been built from — the question is not
// answerable at boot, which is when it has to be answered.
//
// Containers are excluded for a duller reason: a layer is a qcow2 backing
// chain, and a container has no disk of that shape.
func (p *Pool) LayersAllowed() bool {
	return p.Runtime == RuntimeVM && p.ScopeKind == ScopeRepository
}

// LayerApproval is what an operator has said about one definition.
type LayerApproval string

const (
	// LayerPending has been read from the repository and not yet decided on.
	// The pool goes on using whatever it was using.
	LayerPending LayerApproval = "pending"
	// LayerApproved is built and booted.
	LayerApproved LayerApproval = "approved"
	// LayerRefused is not built, and is remembered so that it does not come
	// back as a new question every time the daemon reads the file.
	LayerRefused LayerApproval = "refused"
)

// RepoLayer is one definition a repository has published, and what became of
// it.
type RepoLayer struct {
	ID   int64  `json:"id"`
	Pool string `json:"pool"`
	Repo string `json:"repo"`
	// Digest identifies the definition by what it will do. See
	// internal/repospec: it is over the effective package list and the recipe,
	// not over the file, so that an operator approves what runs rather than
	// how it was typed.
	Digest   string   `json:"digest"`
	Packages []string `json:"packages"`
	Recipe   string   `json:"recipe"`
	// Image is the layer's file name once it has been built.
	Image    string        `json:"image"`
	Approval LayerApproval `json:"approval"`
	// DecidedBy is whoever approved or refused it, for the question that comes
	// afterwards.
	DecidedBy string    `json:"decidedBy"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	DecidedAt time.Time `json:"decidedAt"`
}

// Buildable reports whether this definition may be turned into a layer now.
func (l RepoLayer) Buildable() bool { return l.Approval == LayerApproved }

// ValidateLayerPolicy checks a pool's policy against what the pool is.
func ValidateLayerPolicy(p Pool) error {
	if !p.Layers.Valid() {
		return fmt.Errorf("layers %q: want off, approve or trust", p.Layers)
	}
	if p.Layers != "" && p.Layers != LayersOff && !p.LayersAllowed() {
		// Said rather than quietly ignored: a pool configured for something it
		// cannot do is a pool somebody is waiting on.
		if p.Runtime != RuntimeVM {
			return fmt.Errorf("a %s pool has no image to layer on", p.Runtime)
		}
		return fmt.Errorf("an organisation pool cannot use repository layers: " +
			"a runner is built before it knows which repository's job it will take, " +
			"so there is no one repository whose layer it could have")
	}
	return nil
}
