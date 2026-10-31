package httpapi

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// The logo bytes round-trip on every engine (BLOB on SQLite, BYTEA on
// Postgres), and a replace keeps one row per program.
func TestProgramLogos_Engines(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'one')`); err != nil {
		t.Fatal(err)
	}
	up := `INSERT INTO program_logos (campaign_id, content_type, data, sha256, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (campaign_id) DO UPDATE SET content_type = excluded.content_type,
			data = excluded.data, sha256 = excluded.sha256, updated_at = excluded.updated_at`
	for _, d := range [][]byte{{0x89, 'P', 'N', 'G', 0, 1}, {0xff, 0xd8, 0xff, 0, 2, 3}} {
		if _, err := h.DB.ExecContext(ctx, up, 1, "image/png", d, "abcdef0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
	}
	var got []byte
	if err := h.DB.QueryRow(`SELECT data FROM program_logos WHERE campaign_id = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string([]byte{0xff, 0xd8, 0xff, 0, 2, 3}) {
		t.Fatalf("data = %v", got)
	}
	if uploadedLogo(ctx, h.DB, 1) != "/api/v1/portal/logo?campaign_id=1&v=abcdef012345" {
		t.Fatalf("path = %q", uploadedLogo(ctx, h.DB, 1))
	}
}
