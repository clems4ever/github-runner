package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
)

// twoPools is the shape an import usually has: a fleet that only makes sense
// whole.
func twoPools(credentialID int64) []model.Pool {
	container := samplePool(credentialID)
	container.Name = "ci-container"
	container.Runtime = model.RuntimeContainer

	machine := samplePool(credentialID)
	machine.Name = "ci-vm"
	return []model.Pool{container, machine}
}

func names(t *testing.T, s *Store) []string {
	t.Helper()
	pools, err := s.ListPools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, pool := range pools {
		out = append(out, pool.Name)
	}
	return out
}

func TestImportCreatesEveryPool(t *testing.T) {
	s := newStore(t)
	c := credential(t, s)

	outcomes, err := s.ImportPools(context.Background(), twoPools(c.ID), false, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(outcomes))
	}
	for _, outcome := range outcomes {
		if outcome.Action != ImportCreate {
			t.Errorf("%s: got %q, want a create", outcome.Name, outcome.Action)
		}
		if outcome.Pool.ID == 0 {
			t.Errorf("%s: came back without an id", outcome.Name)
		}
	}
	if got := names(t, s); len(got) != 2 {
		t.Fatalf("the fleet has %v", got)
	}
}

// The point of the transaction. A document is imported whole or not at all, so
// nobody is left with half a fleet and an error message.
func TestImportWritesNothingWhenOnePoolIsRefused(t *testing.T) {
	s := newStore(t)
	c := credential(t, s)

	pools := twoPools(c.ID)
	pools[1].CPUs = 9000

	if _, err := s.ImportPools(context.Background(), pools, false, false); err == nil {
		t.Fatal("a pool with 9000 cpus was imported")
	}
	if got := names(t, s); len(got) != 0 {
		t.Fatalf("the first pool was left behind: %v", got)
	}
}

func TestImportWritesNothingWhenTheCredentialIsGone(t *testing.T) {
	s := newStore(t)
	c := credential(t, s)

	pools := twoPools(c.ID)
	pools[1].CredentialID = c.ID + 99

	if _, err := s.ImportPools(context.Background(), pools, false, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want a not-found credential, got %v", err)
	}
	if got := names(t, s); len(got) != 0 {
		t.Fatalf("the first pool was left behind: %v", got)
	}
}

func TestImportRefusesANameThatIsTaken(t *testing.T) {
	s := newStore(t)
	c := credential(t, s)
	ctx := context.Background()

	if _, err := s.ImportPools(ctx, twoPools(c.ID), false, false); err != nil {
		t.Fatal(err)
	}

	second := twoPools(c.ID)
	second[0].CPUs = 8
	_, err := s.ImportPools(ctx, second, false, false)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want a conflict, got %v", err)
	}
	// And says which pool, and what the way out is.
	if !strings.Contains(err.Error(), "ci-container") || !strings.Contains(err.Error(), "Import over") {
		t.Errorf("the message does not say what to do:\n%v", err)
	}

	pools, _ := s.ListPools(ctx)
	for _, pool := range pools {
		if pool.Name == "ci-container" && pool.CPUs == 8 {
			t.Fatal("the refused import changed the pool anyway")
		}
	}
}

func TestImportingOverAPoolKeepsItsIdentity(t *testing.T) {
	s := newStore(t)
	c := credential(t, s)
	ctx := context.Background()

	first, err := s.ImportPools(ctx, twoPools(c.ID), false, false)
	if err != nil {
		t.Fatal(err)
	}
	before := first[0].Pool

	again := twoPools(c.ID)
	again[0].CPUs = 8
	again[0].MaxReplicas = 5
	outcomes, err := s.ImportPools(ctx, again, true, false)
	if err != nil {
		t.Fatalf("import over: %v", err)
	}

	for _, outcome := range outcomes {
		if outcome.Action != ImportUpdate {
			t.Errorf("%s: got %q, want an update", outcome.Name, outcome.Action)
		}
	}
	after := outcomes[0].Pool
	// The same row, changed. A delete-and-recreate would give the pool a new
	// id, and anything holding the old one — a bookmarked page, a runner being
	// drained — would be pointing at nothing.
	if after.ID != before.ID {
		t.Errorf("the pool was replaced rather than updated: id %d became %d", before.ID, after.ID)
	}
	if after.CPUs != 8 || after.MaxReplicas != 5 {
		t.Errorf("the new configuration was not applied: %+v", after)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("the creation time moved: %s became %s", before.CreatedAt, after.CreatedAt)
	}
	if got := names(t, s); len(got) != 2 {
		t.Fatalf("importing over two pools left %v", got)
	}
}

// The preview is the real import, rolled back. That is what makes it worth
// reading: it cannot report a create that would have failed.
func TestADryRunReportsEverythingAndWritesNothing(t *testing.T) {
	s := newStore(t)
	c := credential(t, s)
	ctx := context.Background()

	outcomes, err := s.ImportPools(ctx, twoPools(c.ID), false, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(outcomes) != 2 || outcomes[0].Action != ImportCreate {
		t.Fatalf("got %+v", outcomes)
	}
	if got := names(t, s); len(got) != 0 {
		t.Fatalf("the dry run wrote %v", got)
	}

	// And the real import afterwards still works: the rollback left no trace
	// in the sequence or the table.
	if _, err := s.ImportPools(ctx, twoPools(c.ID), false, false); err != nil {
		t.Fatalf("the import after a dry run failed: %v", err)
	}
	if got := names(t, s); len(got) != 2 {
		t.Fatalf("the fleet has %v", got)
	}
}

func TestADryRunFailsForTheSameReasonsAsTheImport(t *testing.T) {
	s := newStore(t)
	c := credential(t, s)
	ctx := context.Background()

	if _, err := s.ImportPools(ctx, twoPools(c.ID), false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportPools(ctx, twoPools(c.ID), false, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("the dry run did not report the conflict the import would hit: %v", err)
	}
	// With the tick-box, the dry run says update — and still writes nothing.
	outcomes, err := s.ImportPools(ctx, twoPools(c.ID), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if outcomes[0].Action != ImportUpdate {
		t.Fatalf("got %q, want an update", outcomes[0].Action)
	}
}

func TestImportingNothingIsRefused(t *testing.T) {
	s := newStore(t)
	if _, err := s.ImportPools(context.Background(), nil, false, false); err == nil {
		t.Fatal("an empty import was accepted")
	}
}
