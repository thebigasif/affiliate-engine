package httpapi

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

func TestAutoCodeBase(t *testing.T) {
	for _, tc := range []struct{ name, email, want string }{
		{"Jane Smith", "j@x.dev", "janesmith"},
		{"  O'Brien-Lee  ", "o@x.dev", "obrienlee"},
		{"Maximilian Alexander Featherstonehaugh", "m@x.dev", "maximilianalexanderf"}, // 20
		{"", "Jane.Doe+aff@x.dev", "janedoeaff"},
		{"12345", "99@x.dev", "partner"}, // digits only would shadow /r/{id}
		{"李小龙", "bruce@x.dev", "bruce"},
		{"J", "", "partner"},
	} {
		if got := autoCodeBase(tc.name, tc.email); got != tc.want {
			t.Errorf("autoCodeBase(%q, %q) = %q, want %q", tc.name, tc.email, got, tc.want)
		}
	}
}

func TestAutoCodeCandidate(t *testing.T) {
	if got := autoCodeCandidate("janesmith", 1); got != "janesmith" {
		t.Fatal(got)
	}
	if got := autoCodeCandidate("janesmith", 2); got != "janesmith2" {
		t.Fatal(got)
	}
	long := "maximilianalexanderf" // 20
	if got := autoCodeCandidate(long, 12); got != "maximilianalexande12" || len(got) != autoCodeMax {
		t.Fatalf("%q", got)
	}
}

// Dual-engine: a taken candidate must not poison the transaction (PG aborts
// a tx on a failed statement; ON CONFLICT DO NOTHING avoids that).
func TestEnsurePrimaryCode_Engines(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	var a1, a2 int64
	if err := h.DB.QueryRowContext(ctx, `INSERT INTO affiliates (email, name) VALUES ('a@x.dev', 'Jane Smith') RETURNING id`).Scan(&a1); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.QueryRowContext(ctx, `INSERT INTO affiliates (email, name) VALUES ('b@x.dev', 'Jane Smith') RETURNING id`).Scan(&a2); err != nil {
		t.Fatal(err)
	}
	// janesmith and janesmith2 already belong to a1, so a2 must land on 3.
	for _, c := range []string{"janesmith", "janesmith2"} {
		if _, err := h.DB.ExecContext(ctx, `INSERT INTO affiliate_codes (affiliate_id, code) VALUES (?, ?)`, a1, c); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := ensurePrimaryCode(ctx, tx, a2, "Jane Smith", "b@x.dev")
	if err != nil || got != "janesmith3" {
		t.Fatalf("ensurePrimaryCode = %q, %v", got, err)
	}
	again, err := ensurePrimaryCode(ctx, tx, a2, "Jane Smith", "b@x.dev")
	if err != nil || again != "janesmith3" {
		t.Fatalf("second call minted another code: %q, %v", again, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after skipped candidates: %v", err)
	}
}
