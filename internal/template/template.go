// Package template is the portable form of a fleet's pools: a document that can
// be written by hand, checked into a repository, and imported into any
// installation of the daemon.
//
// What it deliberately does not carry is anything local to one installation —
// pool ids, credential ids, timestamps. A credential belongs to the host it was
// sealed on, and a template that named one would be a template that only works
// in the place it came from. The import asks which credential to use instead,
// and may override the scope, so the same document serves several repositories.
package template

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/clems4ever/github-runner/internal/model"
)

// Version is the format this build writes and the only one it reads.
//
// It is required rather than assumed. Without it, pasting the output of
// /api/pools — which looks similar and is not the same thing — would half-work,
// and a format that grows a second version later would have no way to tell the
// two apart.
const Version = 1

// Document is a set of pools, as written down.
type Document struct {
	Version int `json:"version"`
	// Name and Description are for whoever reads the file or the import
	// preview. Nothing in the daemon depends on them.
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Pools       []Pool `json:"pools"`
}

// Pool is one pool in a template.
//
// The three switches are pointers because their zero value is a real answer
// that differs from the sensible default: a template that says nothing about
// "enabled" means an enabled pool, and plain bools would import every pool
// switched off.
type Pool struct {
	Name string `json:"name"`
	// ScopeKind and Scope may be left out, and then the import supplies them.
	// A template written for one repository can name it; one meant to be
	// reused should not.
	ScopeKind   model.ScopeKind `json:"scopeKind,omitempty"`
	Scope       string          `json:"scope,omitempty"`
	Runtime     model.Runtime   `json:"runtime,omitempty"`
	Nested      *bool           `json:"nested,omitempty"`
	Ephemeral   *bool           `json:"ephemeral,omitempty"`
	MinReplicas int             `json:"minReplicas,omitempty"`
	MaxReplicas int             `json:"maxReplicas,omitempty"`
	// Sleeps travels, unlike layers below: a pool that goes to zero when its
	// repository is quiet is a shape somebody chose for the pool, not a trust
	// decision about a host. A plain bool because false is both the zero value
	// and the default, so a template that says nothing means a pool that stays
	// up — which is what every template written before this existed meant.
	Sleeps   bool     `json:"sleeps,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	CPUs     int      `json:"cpus,omitempty"`
	MemoryMB int      `json:"memoryMb,omitempty"`
	DiskGB   int      `json:"diskGb,omitempty"`
	Image    string   `json:"image,omitempty"`
	// What a machine pool bakes into its image. A template is the portable
	// form of a pool, and a pool whose runners have the toolchain baked in is
	// not portable without them.
	Packages []string `json:"packages,omitempty"`
	Recipe   string   `json:"recipe,omitempty"`
	Enabled  *bool    `json:"enabled,omitempty"`
}

// Options are the answers the document does not carry.
type Options struct {
	// CredentialID is the credential every imported pool registers with. It is
	// required: a pool cannot mint a registration token without one.
	CredentialID int64
	// Scope, when set, replaces the scope of every pool in the document —
	// which is how one template serves more than one repository.
	Scope     string
	ScopeKind model.ScopeKind
}

// local names the fields a template must not carry, and says why, because the
// obvious mistake is to paste what /api/pools returned.
var local = map[string]string{
	"id":           "pool ids belong to one database",
	"credentialId": "a credential belongs to the host it was sealed on — the import asks which one to use",
	"createdAt":    "timestamps are recorded by the daemon",
	"updatedAt":    "timestamps are recorded by the daemon",
	// Deliberately not portable. Letting a repository add to a pool's image is
	// a decision about *that* repository on *this* host — a template that
	// carried "trust" would install an approval nobody made, on a scope the
	// importer may be overriding anyway. It is one control in the pool editor.
	"layers": "whether a repository may add to a pool's image is decided on the host, per pool",
}

// Parse reads a document and rejects anything that is not one.
//
// Unknown fields are refused rather than ignored: a misspelt "ephemeral" that
// silently did nothing would be a setting the operator believes is applied.
func Parse(raw []byte) (Document, error) {
	var doc Document
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, readable(err)
	}
	// One document per file. Trailing content is usually two templates
	// concatenated, and taking the first silently would drop the rest.
	if decoder.More() {
		return Document{}, errors.New("there is more than one document in this file")
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// readable turns the encoding/json vocabulary into something an operator can
// act on.
func readable(err error) error {
	message := err.Error()
	if field, ok := strings.CutPrefix(message, "json: unknown field "); ok {
		name := strings.Trim(field, `"`)
		if why, isLocal := local[name]; isLocal {
			return fmt.Errorf("a template does not carry %q: %s. Remove it and import again", name, why)
		}
		return fmt.Errorf("%q is not a field of a pool template", name)
	}
	return fmt.Errorf("this is not a pool template: %v", err)
}

// Validate reports why a document cannot be imported, before anything is
// written.
func (d Document) Validate() error {
	if d.Version == 0 {
		return fmt.Errorf(`a pool template needs "version": %d at the top level`, Version)
	}
	if d.Version != Version {
		return fmt.Errorf("this template is version %d, and this daemon reads version %d", d.Version, Version)
	}
	if len(d.Pools) == 0 {
		return errors.New("this template has no pools in it")
	}

	seen := map[string]bool{}
	for i, pool := range d.Pools {
		if pool.Name == "" {
			return fmt.Errorf("pool %d has no name", i+1)
		}
		if seen[pool.Name] {
			return fmt.Errorf("pool %q appears twice in this template", pool.Name)
		}
		seen[pool.Name] = true
	}
	return nil
}

// Apply turns a document into pools this daemon can store, filling in what the
// document leaves to the importer and checking every one of them.
//
// Nothing is written here. A document that would be refused halfway through is
// refused before the first pool is created, so an import is all of it or none.
func Apply(doc Document, opts Options) ([]model.Pool, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	if opts.CredentialID == 0 {
		return nil, errors.New("choose a credential: imported pools register with it, and a runner registers afresh every time it starts")
	}

	out := make([]model.Pool, 0, len(doc.Pools))
	for _, entry := range doc.Pools {
		pool := model.Pool{
			Name:         entry.Name,
			ScopeKind:    entry.ScopeKind,
			Scope:        entry.Scope,
			Runtime:      entry.Runtime,
			Nested:       value(entry.Nested, false),
			Ephemeral:    value(entry.Ephemeral, true),
			MinReplicas:  entry.MinReplicas,
			MaxReplicas:  entry.MaxReplicas,
			Sleeps:       entry.Sleeps,
			Labels:       entry.Labels,
			CPUs:         entry.CPUs,
			MemoryMB:     entry.MemoryMB,
			DiskGB:       entry.DiskGB,
			Image:        entry.Image,
			Packages:     entry.Packages,
			Recipe:       entry.Recipe,
			CredentialID: opts.CredentialID,
			Enabled:      value(entry.Enabled, true),
		}

		// An override replaces what the document says, for every pool. Half a
		// fleet on one repository and half on another is not something anyone
		// asks for by typing one scope into an import dialog.
		if opts.Scope != "" {
			pool.Scope = opts.Scope
			pool.ScopeKind = opts.ScopeKind
		}
		if pool.Scope == "" {
			return nil, fmt.Errorf("pool %q has no scope, and the import gave none: say which repository or organisation these runners are for", pool.Name)
		}

		pool.Defaults()
		if err := pool.Validate(); err != nil {
			return nil, fmt.Errorf("pool %q: %w", pool.Name, err)
		}
		out = append(out, pool)
	}
	return out, nil
}

// Export writes pools out as a template, dropping everything local to this
// installation so the result imports anywhere.
func Export(pools []model.Pool) Document {
	doc := Document{Version: Version, Pools: make([]Pool, 0, len(pools))}
	for _, pool := range pools {
		nested, ephemeral, enabled := pool.Nested, pool.Ephemeral, pool.Enabled
		entry := Pool{
			Name:        pool.Name,
			ScopeKind:   pool.ScopeKind,
			Scope:       pool.Scope,
			Runtime:     pool.Runtime,
			Nested:      &nested,
			Ephemeral:   &ephemeral,
			MinReplicas: pool.MinReplicas,
			MaxReplicas: pool.MaxReplicas,
			Sleeps:      pool.Sleeps,
			Labels:      pool.Labels,
			CPUs:        pool.CPUs,
			MemoryMB:    pool.MemoryMB,
			Image:       pool.Image,
			Packages:    pool.Packages,
			Recipe:      pool.Recipe,
			Enabled:     &enabled,
		}
		// A container has no disk of its own, and a disk size on one would be
		// a number that means nothing.
		if pool.Runtime == model.RuntimeVM {
			entry.DiskGB = pool.DiskGB
		}
		if len(entry.Labels) == 0 {
			entry.Labels = nil
		}
		if len(entry.Packages) == 0 {
			entry.Packages = nil
		}
		doc.Pools = append(doc.Pools, entry)
	}
	return doc
}

func value(pointer *bool, fallback bool) bool {
	if pointer == nil {
		return fallback
	}
	return *pointer
}
