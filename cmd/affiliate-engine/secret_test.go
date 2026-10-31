package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateTokenSecret(t *testing.T) {
	for _, bad := range []string{
		"change-me-to-32-plus-random-bytes",
		"CHANGE-ME-please-this-is-long-enough-to-pass-length",
		strings.Repeat("ab", 16),           // 32 hex chars = 16 bytes
		"BRAbJjE8R1JdaHN+iZSfqrXAy9bh7PcC", // base64 of 24 bytes (32 chars)
		"short",
	} {
		if err := validateTokenSecret(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{
		strings.Repeat("a1", 32),                       // openssl rand -hex 32
		"AwoRGB8mLTQ7QklQV15lbHN6gYiPlp2kq7K5wMfO1dw=", // base64 of 32 bytes
		strings.Repeat("x!", 20),                       // 40 raw non-hex, non-base64 bytes
	} {
		if err := validateTokenSecret(good); err != nil {
			t.Errorf("%q rejected: %v", good, err)
		}
	}
}

func TestLoadOrCreateSecret_EnvIsRawKey(t *testing.T) {
	v := strings.Repeat("a1", 32)
	t.Setenv("AE_TOKEN_SECRET", v)
	t.Setenv("AE_SECRET_FILE", "")
	b, err := loadOrCreateSecret("sqlite:" + filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != v {
		t.Fatal("env secret must be used verbatim (existing cookies stay valid)")
	}
	t.Setenv("AE_TOKEN_SECRET", "change-me-to-32-plus-random-bytes")
	if _, err := loadOrCreateSecret("sqlite:x.db"); err == nil {
		t.Fatal("placeholder accepted")
	}
}

// The generated secret lands next to the SQLite db (not the working
// directory), owner-only, and is reused on the next start.
func TestLoadOrCreateSecret_FileNextToDB(t *testing.T) {
	t.Setenv("AE_TOKEN_SECRET", "")
	t.Setenv("AE_SECRET_FILE", "")
	dir := t.TempDir()
	dsn := "sqlite:" + filepath.Join(dir, "engine.db")
	b1, err := loadOrCreateSecret(dsn)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "engine.db.secret")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("secret not next to the db: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("secret perms=%o want 600", st.Mode().Perm())
	}
	b2, err := loadOrCreateSecret(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) || len(b1) < 32 {
		t.Fatal("second start must reuse the persisted secret")
	}

	// A truncated secret file is an error, never silently regenerated.
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSecret(dsn); err == nil {
		t.Fatal("short secret file accepted")
	}
}

func TestSecretFilePath(t *testing.T) {
	t.Setenv("AE_SECRET_FILE", "")
	if got := secretFilePath(""); got != "affiliate-engine.db.secret" {
		t.Fatalf("default=%q (must match the legacy name for the default db)", got)
	}
	if got := secretFilePath("postgres://u:p@db.example/x"); got != "/data/affiliate-engine.secret" {
		t.Fatalf("postgres=%q", got)
	}
	t.Setenv("AE_SECRET_FILE", "/run/secrets/ae")
	if got := secretFilePath("sqlite:/data/a.db"); got != "/run/secrets/ae" {
		t.Fatalf("override=%q", got)
	}
}

func TestDSNSummaryNeverLeaksCredentials(t *testing.T) {
	s := dsnSummary("postgres://ae_user:hunter2@db.example.com:5432/ae?sslmode=verify-full")
	if strings.Contains(s, "hunter2") || strings.Contains(s, "ae_user") || strings.Contains(s, "sslmode") {
		t.Fatalf("summary leaks DSN parts: %q", s)
	}
	if s != "engine=postgres host=db.example.com" {
		t.Fatalf("summary=%q", s)
	}
	if s := dsnSummary("sqlite:/data/secret-name.db"); s != "engine=sqlite" {
		t.Fatalf("sqlite summary=%q", s)
	}
}
