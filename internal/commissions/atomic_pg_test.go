package commissions_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// helperDSNEnv hands the database to the helper process.
const helperDSNEnv = "AE_ATOMIC_HELPER_DSN"

// Two replicas on one Postgres: the in-process moneylock serializes
// nothing between them, so the database must. A maturation in another
// process is slowed down inside its journal post (a trigger sleeps on the
// maturation journal insert) and a partial refund of the same commission
// runs in this process while it sleeps.
//
// Before the fix the refund read the commission as 'pending' with nothing
// clawed, clawed its half out of HOLD, and the maturation — which had
// read "nothing clawed" before its post — then moved the FULL amount
// HOLD -> payable: HOLD over-drained to +500 and the affiliate owed 1000
// for a commission worth 500. With the commission row locked (SELECT ...
// FOR UPDATE) for the whole money transaction, the refund waits for the
// maturation to commit and claws its half from the payable.
func TestAtomic_CrossProcessRefundVsMature(t *testing.T) {
	if os.Getenv(helperDSNEnv) != "" {
		t.Skip("helper process")
	}
	if !dbtest.IsPostgres() {
		t.Skip("cross-process serialization is a Postgres concern: SQLite's BEGIN IMMEDIATE takes a file lock that already serializes processes")
	}
	ctx := context.Background()
	h, dsn := dbtest.OpenDSN(t)
	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
	}); err != nil {
		t.Fatal(err)
	}
	seed(t, h.DB)
	e := &env{h: h, p: p, e: commissions.NewEngine(h.DB, p)}
	cid := dueCommission(t, e, "xproc")

	if _, err := h.DB.Exec(`CREATE OR REPLACE FUNCTION ae_slow() RETURNS trigger AS $$
		BEGIN PERFORM pg_sleep(1.5); RETURN NEW; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`CREATE TRIGGER ae_slow BEFORE INSERT ON journal_entries
		FOR EACH ROW WHEN (NEW.source = 'maturation') EXECUTE FUNCTION ae_slow()`); err != nil {
		t.Fatal(err)
	}

	// #nosec G204 -- re-executes this test binary with a fixed test filter.
	cmd := exec.Command(os.Args[0], "-test.run=^TestAtomicHelper_Mature$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperDSNEnv+"="+dsn)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait until the helper's maturation is asleep inside its journal post.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var sleeping int
		if err := h.DB.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event = 'PgSleep'`).Scan(&sleeping); err != nil {
			t.Fatal(err)
		}
		if sleeping > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("helper maturation never reached its journal post:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := e.e.ProcessRefund(ctx, refundEvent("xproc-ref"), cid, 5000, 10000); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out.String())
	}

	hold, payable := balances(t, e)
	if hold != 0 || payable != -500 {
		t.Errorf("HOLD=%d payable=%d, want 0/-500: a half-refunded 1000 commission owes the affiliate 500, once", hold, payable)
	}
	if n, sum := clawRows(t, e, cid); n != 1 || sum != -500 {
		t.Errorf("clawback rows=%d sum=%d, want 1/-500", n, sum)
	}
	if s, err := p.GlobalIntegrity(ctx); err != nil || s != 0 {
		t.Errorf("integrity=%d err=%v", s, err)
	}
}

// TestAtomicHelper_Mature is the other replica: it runs one Mature pass
// against the database the parent test hands it, then exits.
func TestAtomicHelper_Mature(t *testing.T) {
	dsn := os.Getenv(helperDSNEnv)
	if dsn == "" {
		t.Skip("runs only as TestAtomic_CrossProcessRefundVsMature's helper process")
	}
	ctx := context.Background()
	h, err := db.Open(ctx, dsn, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Ledger.Close(); _ = h.DB.Close() }()
	n, err := commissions.NewEngine(h.DB, ledger.NewPoster(h.Ledger)).Mature(ctx)
	if err != nil || n != 1 {
		t.Fatalf("helper mature: n=%d err=%v", n, err)
	}
}

// lockedBuffer collects the helper's output; exec writes it from its own
// goroutines while the test may read it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
