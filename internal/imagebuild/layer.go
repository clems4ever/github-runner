package imagebuild

import (
	"context"
	"fmt"

	"github.com/clems4ever/github-runner/internal/agent"
	"github.com/clems4ever/github-runner/internal/model"
)

// LayerImage is the file a repository's layer would be built as.
//
// Pure: it names the image without building anything or looking at the disk,
// which is what lets the reconciler ask "is this one built yet" on every pass
// for the cost of a hash.
func LayerImage(pool model.Pool, layer model.RepoLayer) string {
	return layerSpec(pool, layer).Name()
}

// LayerBuilt reports whether a repository's layer is on this host already.
func (b *Builder) LayerBuilt(pool model.Pool, layer model.RepoLayer) bool {
	_, built := agent.BuiltLayer(layerSpec(pool, layer), b.imagesDir)
	return built
}

// EnsureLayer asks for a repository's layer to be built, if it is not built
// and nothing is building it.
//
// It returns the image's name whether or not it is ready, and whether it is —
// the caller wants both: the name to boot, and the readiness to decide whether
// it may. Nothing here waits: a layer takes minutes and a reconcile pass takes
// a second.
func (b *Builder) EnsureLayer(ctx context.Context, pool model.Pool, layer model.RepoLayer) (image string, ready bool, err error) {
	spec := layerSpec(pool, layer)
	image = spec.Name()

	if _, built := agent.BuiltLayer(spec, b.imagesDir); built {
		return image, true, nil
	}

	// The pool's own image is what this sits on. A layer built before it
	// exists would be a qcow2 whose backing file is not there.
	if _, built := agent.GoldenImage(specFor(pool), b.imagesDir); !built {
		return image, false, fmt.Errorf("its pool's own image is not built yet")
	}

	b.mu.Lock()
	if last, known := b.latest[image]; known && last.Unfinished() {
		b.mu.Unlock()
		return image, false, nil // already on its way
	}
	b.mu.Unlock()

	build, err := b.store.StartImageBuild(ctx, model.ImageBuild{
		Pool: pool.Name, Image: image, Phase: model.ImageQueued,
		Trigger: model.ImageAutomatic, StartedAt: b.now(),
	})
	if err != nil {
		return image, false, err
	}

	b.mu.Lock()
	b.latest[image] = build
	b.pending = append(b.pending, queued{id: build.ID, layer: &spec, repo: layer.Repo})
	b.mu.Unlock()

	select {
	case b.wake <- struct{}{}:
	default:
	}
	return image, false, nil
}

// layerSpec is what to build, from the pool and what the repository asked for.
//
// The pool contributes only its image — the base this sits on — and not its
// name: two pools serving the same repository with the same base and the same
// ask share one layer, because they would produce byte-identical disks.
func layerSpec(pool model.Pool, layer model.RepoLayer) agent.LayerSpec {
	return agent.LayerSpec{
		Base:     Image(pool),
		Packages: layer.Packages,
		Recipe:   layer.Recipe,
	}
}
