// secret.go — the attribution-token HMAC secret: validation of an operator
// supplied AE_TOKEN_SECRET, and the generate-once file fallback.
package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

// legacySecretFile is where builds before this one wrote the generated
// secret: the WORKING directory, whatever -db said. Still read (never
// written) so an upgrade keeps its existing cookies valid.
const legacySecretFile = "affiliate-engine.db.secret"

// loadOrCreateSecret returns AE_TOKEN_SECRET (validated) or the persisted
// generated secret, creating it on first run.
func loadOrCreateSecret(dsn string) ([]byte, error) {
	if s := os.Getenv("AE_TOKEN_SECRET"); s != "" {
		if err := validateTokenSecret(s); err != nil {
			return nil, err
		}
		// The raw string is the key (as before): re-deriving it from a
		// decoding would invalidate every cookie already issued.
		return []byte(s), nil
	}
	path := secretFilePath(dsn)
	b, err := readSecretFile(path)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if filepath.Clean(path) != legacySecretFile {
		if b, lerr := readSecretFile(legacySecretFile); lerr == nil {
			fmt.Printf("affiliate-engine: using legacy secret %s from the working directory (move it to %s or set AE_TOKEN_SECRET)\n",
				legacySecretFile, path)
			return b, nil
		}
	}
	secret, err := tokens.NewRandomSecret(tokens.MinSecretLen)
	if err != nil {
		return nil, err
	}
	// O_EXCL: never clobber an existing file (a concurrent first start, or
	// a file this process could not read, must not be silently replaced).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 G703 -- operator-configured secret location (AE_SECRET_FILE / -db path)
	if err != nil {
		return nil, fmt.Errorf("persist secret at %s: %w (set AE_TOKEN_SECRET, or AE_SECRET_FILE to a writable path)", path, err)
	}
	if _, err := f.Write(secret); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("persist secret: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("persist secret: %w", err)
	}
	fmt.Println("affiliate-engine: generated attribution secret at", path,
		"(set AE_TOKEN_SECRET in production)")
	return secret, nil
}

// secretFilePath: AE_SECRET_FILE wins; SQLite keeps the secret next to its
// database ("<db>.secret" — the old name for the default db path); a
// Postgres deployment has no database file, so it uses the /data volume.
func secretFilePath(dsn string) string {
	if p := os.Getenv("AE_SECRET_FILE"); p != "" {
		return p
	}
	if isPostgresDSN(dsn) {
		return "/data/affiliate-engine.secret"
	}
	return sqlitePath(dsn) + ".secret"
}

func readSecretFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path) // #nosec G304 G703 -- operator-configured secret location (AE_SECRET_FILE / -db path)
	if err != nil {
		return nil, err
	}
	if len(b) < tokens.MinSecretLen {
		return nil, fmt.Errorf("secret file %s holds %d bytes, need >= %d — remove it to regenerate, or set AE_TOKEN_SECRET",
			path, len(b), tokens.MinSecretLen)
	}
	return b, nil
}

// validateTokenSecret rejects the documented placeholder (and anything
// that looks like one) and secrets under 32 bytes of key material.
func validateTokenSecret(s string) error {
	if strings.Contains(strings.ToLower(s), "change-me") {
		return fmt.Errorf("AE_TOKEN_SECRET is a placeholder value — generate one with: openssl rand -hex 32")
	}
	if n := secretStrength(s); n < tokens.MinSecretLen {
		return fmt.Errorf("AE_TOKEN_SECRET carries %d bytes of key material, need >= %d (generate with: openssl rand -hex 32)",
			n, tokens.MinSecretLen)
	}
	return nil
}

// secretStrength estimates key bytes: a hex or base64 string counts its
// DECODED length (64 hex chars = 32 bytes, not 64); anything else its raw
// length.
func secretStrength(s string) int {
	if len(s)%2 == 0 {
		if b, err := hex.DecodeString(s); err == nil {
			return len(b)
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return len(b)
		}
	}
	return len(s)
}

// dsnSummary renders a DSN for logs: engine and host only — never the
// user, password, path or query (a DSN is a credential).
func dsnSummary(dsn string) string {
	if !isPostgresDSN(dsn) {
		return "engine=sqlite"
	}
	raw := strings.TrimPrefix(strings.TrimPrefix(dsn, "postgresql:"), "postgres:")
	u, err := url.Parse("postgres://" + strings.TrimPrefix(raw, "//"))
	if err != nil || u.Hostname() == "" {
		return "engine=postgres"
	}
	return "engine=postgres host=" + u.Hostname()
}
