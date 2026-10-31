package commissions_test

import (
	"context"
	"testing"
)

// NFR-9 pin: every commission status transition lands in the append-only
// audit_log with old->new; the table refuses UPDATE and DELETE.
func TestAuditLog_TransitionsAndAppendOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	cid, err := e.e.ProcessPurchase(ctx, purchase("aud-evt", 10000))
	if err != nil {
		t.Fatal(err)
	}
	// pending -> approved via maturation.
	e.h.DB.Exec(`UPDATE commissions SET matures_at = '2000-01-01T00:00:00Z' WHERE id = ?`, cid)
	if _, err := e.e.Mature(ctx); err != nil {
		t.Fatal(err)
	}

	var detail string
	if err := e.h.DB.QueryRow(`
		SELECT detail FROM audit_log WHERE table_name='commissions' AND row_id=?
		ORDER BY id DESC LIMIT 1`, cid).Scan(&detail); err != nil {
		t.Fatalf("no audit row: %v", err)
	}
	if detail != "pending->approved" {
		t.Fatalf("audit detail=%q want pending->approved", detail)
	}

	// Append-only enforcement.
	if _, err := e.h.DB.Exec(`UPDATE audit_log SET detail='tampered'`); err == nil {
		t.Fatal("audit_log UPDATE must be refused")
	}
	if _, err := e.h.DB.Exec(`DELETE FROM audit_log`); err == nil {
		t.Fatal("audit_log DELETE must be refused")
	}
}
