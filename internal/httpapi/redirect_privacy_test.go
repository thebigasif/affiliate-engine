package httpapi_test

// NFR-7 write-time IP anonymization, verified at the HTTP surface: with
// IPModeTrunc24 the click's stored ip_hash must equal the identity hash
// of the TRUNCATED ip (81.2.3.0), not the raw one (81.2.3.4) — proving
// truncation happens before hashing on the live redirect path.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fingerprint"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/privacy"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

func privacyServer(t *testing.T, mode privacy.IPMode) (*httpapi.RedirectHandler, *db.Handles) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	h, err := db.Open(ctx, "sqlite:"+path, db.Options{LedgerSyncFull: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Ledger.Close(); _ = h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source)
		VALUES (1, 'asif', TRUE, 'native')`); err != nil {
		t.Fatal(err)
	}
	secret, _ := tokens.NewRandomSecret(32)
	signer, _ := tokens.New(secret)
	committer := clicks.NewCommitter(ctx, h.DB, 100, 20*time.Millisecond)
	t.Cleanup(committer.Close)

	cfg := httpapi.Config{
		Signer:              signer,
		AllowedDestSuffixes: []string{"example.com"},
		FallbackURL:         "https://example.com/",
		IPMode:              mode,
	}
	return httpapi.NewRedirectHandler(h.DB, cfg, committer), h
}

func waitForClickHash(t *testing.T, h *sql.DB) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var hash []byte
		err := h.QueryRow(`SELECT ip_hash FROM clicks LIMIT 1`).Scan(&hash)
		if err == nil {
			return hash
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("click not persisted")
	return nil
}

func TestRedirect_IPModeTruncatesBeforeHashing(t *testing.T) {
	handler, h := privacyServer(t, privacy.IPModeTrunc24)

	req := httptest.NewRequest("GET", "/r/asif?dest=https://example.com/x", nil)
	req.RemoteAddr = "81.2.3.4:54321" // the client ip under test
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d", rec.Code)
	}

	got := waitForClickHash(t, h.DB)
	wantTrunc := fingerprint.NewIdentity(fingerprint.ByIP, "81.2.3.0")
	rawHash := fingerprint.NewIdentity(fingerprint.ByIP, "81.2.3.4")

	if string(got) != string(wantTrunc[:]) {
		t.Fatalf("ip_hash is not the truncated-subnet hash (NFR-7 violated)")
	}
	if string(got) == string(rawHash[:]) {
		t.Fatal("truncated hash equals the raw-ip hash — test vectors collided")
	}
}

func TestRedirect_IPModeOffHashesRawIP(t *testing.T) {
	handler, h := privacyServer(t, privacy.IPModeOff)

	req := httptest.NewRequest("GET", "/r/asif?dest=https://example.com/x", nil)
	req.RemoteAddr = "81.2.3.4:54321"
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d", rec.Code)
	}

	got := waitForClickHash(t, h.DB)
	rawHash := fingerprint.NewIdentity(fingerprint.ByIP, "81.2.3.4")
	if string(got) != string(rawHash[:]) {
		t.Fatal("IPModeOff must hash the full-precision ip")
	}
}
