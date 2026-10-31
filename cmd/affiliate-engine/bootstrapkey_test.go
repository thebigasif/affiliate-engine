package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

func TestBootstrapAdminKey(t *testing.T) {
	ctx := context.Background()
	h, err := db.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "b.db"), db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.DB.Close()
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}

	// Unset: nothing happens.
	var out bytes.Buffer
	if err := bootstrapAdminKey(ctx, h.DB, "", &out); err != nil || out.Len() != 0 {
		t.Fatalf("unset: err=%v out=%q", err, out.String())
	}
	// A bad name is refused.
	if err := bootstrapAdminKey(ctx, h.DB, "bad name!", &out); err == nil {
		t.Fatal("bad name accepted")
	}

	// First start: one admin key, printed once, and it authenticates as admin.
	if err := bootstrapAdminKey(ctx, h.DB, "setup-1", &out); err != nil {
		t.Fatal(err)
	}
	var key string
	for _, l := range strings.Split(out.String(), "\n") {
		if f := strings.TrimSpace(l); strings.HasPrefix(f, "ae_") {
			key = f
		}
	}
	if key == "" {
		t.Fatalf("no key printed: %q", out.String())
	}
	id, err := apikeys.Authenticate(ctx, h.DB, key)
	if err != nil || !id.IsAdmin {
		t.Fatalf("printed key: id=%+v err=%v", id, err)
	}

	// Restart with the same name: nothing new, no key printed.
	out.Reset()
	if err := bootstrapAdminKey(ctx, h.DB, "setup-1", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "ae_") {
		t.Fatalf("key printed twice: %q", out.String())
	}
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n)
	if n != 1 {
		t.Fatalf("keys=%d want 1", n)
	}
}
