package main

// runGDPRExport / runGDPRErase: the NFR-7 subject-rights operator surface
// over internal/privacy. The Store is engine-agnostic (`?` placeholders are
// rewritten to $N by the dialect connector), so both commands accept any
// -db DSN — self-hosted SQLite installs and hosted PG alike.
//
//	affiliate-engine -db sqlite:ae.db gdpr-export -affiliate 7 -out subject-7.json
//	affiliate-engine -db sqlite:ae.db gdpr-erase   -affiliate 7 -yes
//
// Erase is destructive and irreversible: it refuses to run without the
// explicit -yes flag (the same confirmation convention as restore --force).
import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/privacy"
)

// openDSN opens any supported DSN (sqlite:<path> or postgres://) with the
// CLI self-migrate convention. Mirrors import.go's opener.
func openDSN(ctx context.Context, dsn string) (*db.Handles, error) {
	if dsn == "" {
		dsn = "sqlite:affiliate-engine.db"
	}
	h, err := db.Open(ctx, dsn, db.Options{LedgerSyncFull: true})
	if err != nil {
		return nil, err
	}
	eng := migrations.SQLite
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		eng = migrations.Postgres
	}
	if err := migrations.Apply(ctx, h.DB, eng); err != nil {
		_ = h.DB.Close()
		return nil, err
	}
	return h, nil
}

func runGDPRExport(dsn string, args []string) error {
	fs := flag.NewFlagSet("gdpr-export", flag.ContinueOnError)
	affID := fs.Int64("affiliate", 0, "affiliate id (required)")
	out := fs.String("out", "", "write the bundle here instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *affID <= 0 {
		return fmt.Errorf("gdpr-export: -affiliate is required (see /api/v1/admin/affiliates)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := openDSN(ctx, dsn)
	if err != nil {
		return err
	}
	defer h.DB.Close()

	b, err := privacy.NewStore(h.DB).Export(ctx, *affID)
	if err != nil {
		if errors.Is(err, privacy.ErrUnknownAffiliate) {
			return fmt.Errorf("gdpr-export: no affiliate with id %d", *affID)
		}
		return err
	}
	js, err := b.JSON()
	if err != nil {
		return err
	}
	if *out == "" {
		fmt.Println(string(js))
		return nil
	}
	if err := os.WriteFile(*out, js, 0o600); err != nil { // subject PII: owner-only
		return err
	}
	fmt.Printf("gdpr-export: affiliate %d -> %s (%d bytes)\n", *affID, *out, len(js))
	return nil
}

func runGDPRErase(dsn string, args []string) error {
	fs := flag.NewFlagSet("gdpr-erase", flag.ContinueOnError)
	affID := fs.Int64("affiliate", 0, "affiliate id (required)")
	yes := fs.Bool("yes", false, "confirm the erasure (required: destructive, irreversible)")
	jsonOut := fs.Bool("json", false, "machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *affID <= 0 {
		return fmt.Errorf("gdpr-erase: -affiliate is required")
	}
	if !*yes {
		return fmt.Errorf("gdpr-erase: destructive and irreversible — re-run with -yes to confirm. " +
			"Identity is scrubbed and access revoked; booked commissions/payouts/ledger rows remain " +
			"under the pseudonymous id (accounting retention). Run gdpr-export first if a copy is needed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	h, err := openDSN(ctx, dsn)
	if err != nil {
		return err
	}
	defer h.DB.Close()

	rep, err := privacy.NewStore(h.DB).Erase(ctx, *affID)
	if err != nil {
		if errors.Is(err, privacy.ErrUnknownAffiliate) {
			return fmt.Errorf("gdpr-erase: no affiliate with id %d", *affID)
		}
		return err
	}
	if *jsonOut {
		js, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(js))
		return nil
	}
	fmt.Printf("gdpr-erase: affiliate %d erased at %s\n", rep.AffiliateID, rep.At)
	for table, n := range rep.Deleted {
		fmt.Printf("  deleted   %-24s %d rows\n", table, n)
	}
	for table, n := range rep.Scrubbed {
		fmt.Printf("  scrubbed  %-24s %d rows\n", table, n)
	}
	fmt.Printf("  preserved: %s\n", rep.PreservedNote)
	return nil
}
