package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
)

// apikeyCommand manages the keys machines call the API with, from the host.
//
// The same job as the Settings page, without a browser. That is not a
// convenience: a host provisioned by a script has no browser, and this is how it
// gets a key for its monitoring without anyone opening the UI to set a password
// first — a key stands on its own.
//
// It is also the way back in when the UI password has been lost, alongside
// `passwd`: both work on the database directly, and being able to run either
// means having the host, which is a higher bar than either credential.
func apikeyCommand(args []string) error {
	action := ""
	if len(args) > 0 && args[0][0] != '-' {
		action, args = args[0], args[1:]
	}

	switch action {
	case "create":
		return apikeyCreate(args)
	case "list", "ls":
		return apikeyList(args)
	case "revoke", "delete", "rm":
		return apikeyRevoke(args)
	default:
		return fmt.Errorf("apikey needs create, list or revoke")
	}
}

// openStore is the bootstrap the commands that work on the database share: the
// same layout, key and database the daemon uses, opened in place.
//
// EnsureDirs before opening, because a key or a password may well be set on a
// host the daemon has not yet run on. It creates nothing the runners read, so
// nothing is handed over here: the daemon does that when it starts.
func openStore(root string) (*store.Store, error) {
	layout := layoutFor(root)
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
		return nil, err
	}
	ring, err := secrets.LoadOrCreateKey(layout.MasterKey())
	if err != nil {
		return nil, err
	}
	return store.Open(layout.Database(), ring)
}

func apikeyCreate(args []string) error {
	flags := flag.NewFlagSet("apikey create", flag.ContinueOnError)
	name := flags.String("name", "", "what this key is for, to tell it from the others")
	scope := flags.String("scope", string(model.APIKeyRead), "read or admin")
	days := flags.Int("expires-days", 0, "expire after this many days; 0 never expires")
	root := flags.String("root", "", "put everything under this directory")
	if err := flags.Parse(args); err != nil {
		return err
	}

	db, err := openStore(*root)
	if err != nil {
		return err
	}
	defer db.Close()

	key := model.APIKey{Name: *name, Scope: model.APIKeyScope(*scope)}
	if *days > 0 {
		at := time.Now().UTC().AddDate(0, 0, *days)
		key.ExpiresAt = &at
	}

	created, secret, err := db.CreateAPIKey(context.Background(), key)
	if err != nil {
		return err
	}

	// The key on a line of its own with nothing else on it, so that piping this
	// into a file or a secret manager gets the key and not a sentence about it.
	// Everything else goes to stderr for the same reason.
	fmt.Fprintf(os.Stderr, "%s api key %q created. It is shown once and cannot be recovered:\n",
		created.Scope, created.Name)
	fmt.Println(secret)
	if created.ExpiresAt != nil {
		fmt.Fprintf(os.Stderr, "It stops working on %s.\n", created.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

func apikeyList(args []string) error {
	flags := flag.NewFlagSet("apikey list", flag.ContinueOnError)
	root := flags.String("root", "", "put everything under this directory")
	if err := flags.Parse(args); err != nil {
		return err
	}

	db, err := openStore(*root)
	if err != nil {
		return err
	}
	defer db.Close()

	keys, err := db.ListAPIKeys(context.Background())
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("no api keys")
		return nil
	}

	out := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(out, "NAME\tSCOPE\tPREFIX\tEXPIRES\tLAST USED")
	for _, key := range keys {
		fmt.Fprintf(out, "%s\t%s\t%s%s…\t%s\t%s\n", key.Name, key.Scope,
			secrets.APIKeyPrefix, key.Hint, stamp(key.ExpiresAt, "never"), stamp(key.LastUsedAt, "never"))
	}
	return out.Flush()
}

// stamp is a date, or a word saying there is not one. A blank column reads as a
// value that failed to load.
func stamp(at *time.Time, absent string) string {
	if at == nil {
		return absent
	}
	return at.Format(time.RFC3339)
}

func apikeyRevoke(args []string) error {
	flags := flag.NewFlagSet("apikey revoke", flag.ContinueOnError)
	name := flags.String("name", "", "the key to revoke")
	root := flags.String("root", "", "put everything under this directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("which key: pass --name")
	}

	db, err := openStore(*root)
	if err != nil {
		return err
	}
	defer db.Close()

	// By name rather than by id, because a name is what the operator has: the
	// ids are not shown anywhere they would have seen.
	keys, err := db.ListAPIKeys(context.Background())
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.Name == *name {
			if err := db.DeleteAPIKey(context.Background(), key.ID); err != nil {
				return err
			}
			fmt.Printf("api key %q no longer works\n", *name)
			return nil
		}
	}
	return fmt.Errorf("no api key called %q", *name)
}
