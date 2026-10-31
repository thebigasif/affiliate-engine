package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
)

// bootstrapNameRE bounds AE_BOOTSTRAP_ADMIN_KEY: it becomes the key's label.
var bootstrapNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// bootstrapAdminKey creates one admin key named name and prints it to out
// (the container log), for installs with no shell to run `keygen` in: the
// Docker image is distroless. It is opt-in (AE_BOOTSTRAP_ADMIN_KEY=<name>)
// and idempotent: a restart with the same name finds the key (revoked or
// not) and creates nothing, so the plaintext is printed exactly once. To
// get another key, set a new name. Anyone who can set the engine's env can
// already read its database, so this grants nothing new; the key does sit
// in the log, so remove the variable once you have it.
func bootstrapAdminKey(ctx context.Context, db *sql.DB, name string, out io.Writer) error {
	if name == "" {
		return nil
	}
	if !bootstrapNameRE.MatchString(name) {
		return fmt.Errorf("AE_BOOTSTRAP_ADMIN_KEY must be 1-64 of A-Z a-z 0-9 . _ - (it names the key)")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE name = ? AND scope = ?`,
		name, apikeys.ScopeAdmin).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		fmt.Fprintf(out, "bootstrap admin key %q already exists; nothing printed. Remove AE_BOOTSTRAP_ADMIN_KEY, or set a new name for another key.\n", name)
		return nil
	}
	key, err := apikeys.Store(ctx, db, name, apikeys.ScopeAdmin)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "bootstrap admin key %q created. It is shown once, here:\n  %s\nStore it now, then remove AE_BOOTSTRAP_ADMIN_KEY from the environment.\n", name, key)
	return nil
}
