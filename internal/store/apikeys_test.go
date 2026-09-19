package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/secrets"
)

func TestCreateAPIKeyReturnsTheKeyOnceAndFindsItAgain(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, secret, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci", Scope: model.APIKeyAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" {
		t.Fatal("no key came back, so there is nothing to give the client")
	}
	if created.ID == 0 {
		t.Error("the key was not given an id")
	}
	if created.Scope != model.APIKeyAdmin {
		t.Errorf("scope is %q, want admin", created.Scope)
	}
	if created.CreatedAt.IsZero() {
		t.Error("the key was not dated")
	}
	if created.LastUsedAt != nil {
		t.Error("a key that has never been used claims it has")
	}

	found, err := s.APIKeyByHash(ctx, secrets.HashAPIKey(secret))
	if err != nil {
		t.Fatalf("the key just created could not be found: %v", err)
	}
	if found.ID != created.ID || found.Name != "ci" {
		t.Errorf("found %+v, want the key just created", found)
	}
}

// The reason this whole table is safe to lose: the key is not in it. Asserted on
// the row itself rather than on the API, because this is where it would leak.
func TestCreateAPIKeyStoresNoRecoverableKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, secret, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci"})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, name, hash, hint, scope, created_at, expires_at, last_used_at FROM api_keys`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	body := strings.TrimPrefix(secret, secrets.APIKeyPrefix)
	for rows.Next() {
		var id int64
		var name, hash, hint, scope, created, expires, lastUsed string
		if err := rows.Scan(&id, &name, &hash, &hint, &scope, &created, &expires, &lastUsed); err != nil {
			t.Fatal(err)
		}
		for column, value := range map[string]string{
			"hash": hash, "hint": hint, "name": name, "scope": scope,
			"created_at": created, "expires_at": expires, "last_used_at": lastUsed,
		} {
			if strings.Contains(value, secret) || strings.Contains(value, body) {
				t.Errorf("column %s holds the key itself: %q", column, value)
			}
		}
		if hash != secrets.HashAPIKey(secret) {
			t.Error("the stored hash is not the hash of the key that was handed out")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// Two keys made from the same request must not collide, and the name is what
// tells them apart for a person.
func TestCreateAPIKeyRefusesADuplicateName(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, _, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci"})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("a second key called ci gave %v, want a conflict", err)
	}
}

func TestCreateAPIKeyDefaultsToRead(t *testing.T) {
	s := newStore(t)
	// The safer of the two, so a caller who said nothing about scope cannot
	// change the fleet with what they got.
	created, _, err := s.CreateAPIKey(context.Background(), model.APIKey{Name: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Scope != model.APIKeyRead {
		t.Errorf("scope defaulted to %q, want read", created.Scope)
	}
}

func TestCreateAPIKeyValidates(t *testing.T) {
	s := newStore(t)
	past := time.Now().Add(-time.Hour)

	for _, tc := range []struct {
		name string
		key  model.APIKey
		want string
	}{
		{"no name", model.APIKey{}, "needs a name"},
		{"blank name", model.APIKey{Name: "   "}, "needs a name"},
		{"unknown scope", model.APIKey{Name: "ci", Scope: "write"}, "api key scope"},
		// Refused here rather than accepted and then refused on first use, which
		// would read as the daemon being broken.
		{"already expired", model.APIKey{Name: "ci", ExpiresAt: &past}, "cannot expire in the past"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, secret, err := s.CreateAPIKey(context.Background(), tc.key)
			if err == nil {
				t.Fatalf("%+v was accepted", tc.key)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if secret != "" {
				t.Error("a key was minted for a request that was refused")
			}
		})
	}
}

func TestCreateAPIKeyTrimsTheName(t *testing.T) {
	s := newStore(t)
	created, _, err := s.CreateAPIKey(context.Background(), model.APIKey{Name: "  ci  "})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "ci" {
		t.Errorf("name is %q, want it trimmed", created.Name)
	}
}

func TestAPIKeyExpiryIsKept(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	at := time.Now().Add(48 * time.Hour)

	created, secret, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci", ExpiresAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	if created.ExpiresAt == nil {
		t.Fatal("the expiry was dropped on the way in")
	}

	// It has to survive the round trip, because expiry is enforced on what comes
	// back out, not on what went in.
	found, err := s.APIKeyByHash(ctx, secrets.HashAPIKey(secret))
	if err != nil {
		t.Fatal(err)
	}
	if found.ExpiresAt == nil {
		t.Fatal("the expiry did not come back")
	}
	if !found.ExpiresAt.Equal(at.UTC()) {
		t.Errorf("expiry came back as %s, want %s", found.ExpiresAt, at.UTC())
	}
	if found.Expired(at.Add(-time.Minute)) || !found.Expired(at.Add(time.Minute)) {
		t.Error("the expiry that came back does not decide the way the one that went in would")
	}
}

func TestAPIKeyByHashRefusesAnUnknownKey(t *testing.T) {
	s := newStore(t)
	_, err := s.APIKeyByHash(context.Background(), secrets.HashAPIKey("rf_nothing-like-a-real-key"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown key gave %v, want not found", err)
	}
	// A wrong guess must not be echoed into the logs, where it would be a
	// working key if the guess happened to be right.
	if err != nil && strings.Contains(err.Error(), "rf_") {
		t.Errorf("the error repeats what was presented: %q", err)
	}
}

func TestListAPIKeysIsOrderedAndCarriesNoSecret(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	secretsByName := map[string]string{}
	for _, name := range []string{"monitoring", "ci", "terraform"} {
		_, secret, err := s.CreateAPIKey(ctx, model.APIKey{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		secretsByName[name] = secret
	}

	listed, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d keys, want 3", len(listed))
	}
	for i, want := range []string{"ci", "monitoring", "terraform"} {
		if listed[i].Name != want {
			t.Errorf("key %d is %q, want %q", i, listed[i].Name, want)
		}
	}
	// Nothing on a listed key may be usable as one, and the hint is the only
	// field that even comes close.
	for _, key := range listed {
		for name, secret := range secretsByName {
			if key.Hint == strings.TrimPrefix(secret, secrets.APIKeyPrefix) {
				t.Errorf("the hint on %q is the whole key %q", key.Name, name)
			}
		}
		if key.Hint == "" {
			t.Errorf("%q has no hint, so nothing identifies it", key.Name)
		}
	}
}

func TestListAPIKeysIsEmptyNotNil(t *testing.T) {
	s := newStore(t)
	listed, err := s.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// An empty list, so the API answers [] rather than null and the UI has
	// nothing to special-case.
	if listed == nil {
		t.Error("no keys came back as nil rather than as an empty list")
	}
}

func TestTouchAPIKeyRecordsUse(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, secret, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci"})
	if err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := s.TouchAPIKey(ctx, created.ID, at); err != nil {
		t.Fatal(err)
	}

	found, err := s.APIKeyByHash(ctx, secrets.HashAPIKey(secret))
	if err != nil {
		t.Fatal(err)
	}
	if found.LastUsedAt == nil {
		t.Fatal("the key still says it has never been used")
	}
	if !found.LastUsedAt.Equal(at) {
		t.Errorf("last used is %s, want %s", found.LastUsedAt, at)
	}
}

// Revoking is a delete, and it has to be final: the whole reason keys are hashed
// is that there is nothing left to work with afterwards.
func TestDeleteAPIKeyEndsIt(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, secret, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAPIKey(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.APIKeyByHash(ctx, secrets.HashAPIKey(secret)); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked key still resolves: %v", err)
	}
	listed, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Errorf("%d keys left after revoking the only one", len(listed))
	}
}

// The name is freed, so the key somebody meant to replace can be made again
// under the name their scripts already refer to.
func TestDeleteAPIKeyFreesTheName(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, first, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAPIKey(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	_, second, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci"})
	if err != nil {
		t.Fatalf("the name was not freed: %v", err)
	}
	if first == second {
		t.Error("replacing a key handed out the same key again")
	}
	if _, err := s.APIKeyByHash(ctx, secrets.HashAPIKey(first)); !errors.Is(err, ErrNotFound) {
		t.Error("the replaced key still works")
	}
}

func TestDeleteAPIKeyRefusesOneThatIsNotThere(t *testing.T) {
	s := newStore(t)
	if err := s.DeleteAPIKey(context.Background(), 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a key that does not exist gave %v, want not found", err)
	}
}

// Keys are not credentials: nothing in the fleet refers to one, so revoking is
// never refused the way deleting a credential a pool needs is.
func TestDeleteAPIKeyIsNeverInUse(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	credential(t, s)

	created, _, err := s.CreateAPIKey(ctx, model.APIKey{Name: "ci", Scope: model.APIKeyAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAPIKey(ctx, created.ID); err != nil {
		t.Errorf("revoking a key was refused: %v", err)
	}
}
