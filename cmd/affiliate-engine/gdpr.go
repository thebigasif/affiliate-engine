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
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
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

// readDeploymentSecret finds the deployment secret the server uses
// (AE_TOKEN_SECRET or the persisted file) without ever creating one: a
// new secret would open nothing.
func readDeploymentSecret(dsn string) ([]byte, error) {
	if s := os.Getenv("AE_TOKEN_SECRET"); s != "" {
		if err := validateTokenSecret(s); err != nil {
			return nil, err
		}
		return []byte(s), nil
	}
	if dsn == "" {
		dsn = "sqlite:affiliate-engine.db"
	}
	b, err := readSecretFile(secretFilePath(dsn))
	if errors.Is(err, os.ErrNotExist) {
		if lb, lerr := readSecretFile(legacySecretFile); lerr == nil {
			return lb, nil
		}
	}
	return b, err
}

// subjectStore builds the privacy store with the payout-details opener
// when the deployment secret is available.
func subjectStore(h *db.Handles, dsn, cmd string) *privacy.Store {
	store := privacy.NewStore(h.DB)
	if secret, err := readDeploymentSecret(dsn); err == nil {
		store.WithPayoutOpener(httpapi.PayoutDetailsOpener(httpapi.DeriveTaxDocKey(secret)))
	} else {
		fmt.Fprintf(os.Stderr, "%s: payout details stay sealed (no deployment secret: %v)\n", cmd, err)
	}
	return store
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

	// Payout details are sealed under the deployment secret. The export
	// opens them because they are the affiliate's own data; without the
	// secret they are listed as not readable.
	b, err := subjectStore(h, dsn, "gdpr-export").Export(ctx, *affID)
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
	allowSealed := fs.Bool("allow-sealed-payout", false,
		"erase even when the payout details cannot be opened (their emails then stay in stored events)")
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

	// With the secret, the payout emails are also redacted from stored
	// billing events before the sealed row is deleted.
	store := subjectStore(h, dsn, "gdpr-erase")
	if *allowSealed {
		store.AllowSealedPayout()
	}
	rep, err := store.Erase(ctx, *affID)
	if err != nil {
		if errors.Is(err, privacy.ErrUnknownAffiliate) {
			return fmt.Errorf("gdpr-erase: no affiliate with id %d", *affID)
		}
		if errors.Is(err, privacy.ErrPayoutDetailsSealed) {
			return fmt.Errorf("gdpr-erase: nothing was changed. The affiliate's payout details cannot be opened " +
				"(set AE_TOKEN_SECRET or AE_SECRET_FILE to the server's secret), so their payout emails cannot be " +
				"removed from stored billing events. Re-run with -allow-sealed-payout to erase anyway")
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
	for _, kind := range rep.RedactedInPayloads {
		fmt.Printf("  redacted in stored events:     %s\n", kind)
	}
	for _, kind := range rep.NotRedactedInPayloads {
		fmt.Printf("  NOT redacted in stored events: %s\n", kind)
	}
	for table, n := range rep.Deleted {
		fmt.Printf("  deleted   %-24s %d rows\n", table, n)
	}
	for table, n := range rep.Scrubbed {
		fmt.Printf("  scrubbed  %-24s %d rows\n", table, n)
	}
	fmt.Printf("  preserved: %s\n", rep.PreservedNote)
	return nil
}

// buyerRefFlags parses the buyer selector shared by the buyer commands.
func buyerRefFlags(fs *flag.FlagSet) (*string, *string) {
	email := fs.String("email", "", "buyer email")
	customer := fs.String("customer", "", "buyer's provider customer id")
	return email, customer
}

func runGDPRExportBuyer(dsn string, args []string) error {
	fs := flag.NewFlagSet("gdpr-export-buyer", flag.ContinueOnError)
	email, customer := buyerRefFlags(fs)
	out := fs.String("out", "", "write the bundle here instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" && *customer == "" {
		return fmt.Errorf("gdpr-export-buyer: -email or -customer is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := openDSN(ctx, dsn)
	if err != nil {
		return err
	}
	defer h.DB.Close()

	b, err := privacy.NewStore(h.DB).ExportBuyer(ctx, privacy.BuyerRef{Email: *email, CustomerID: *customer})
	if err != nil {
		if errors.Is(err, privacy.ErrUnknownBuyer) {
			return fmt.Errorf("gdpr-export-buyer: no stored data for that buyer")
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
	fmt.Printf("gdpr-export-buyer: -> %s (%d bytes)\n", *out, len(js))
	return nil
}

func runGDPREraseBuyer(dsn string, args []string) error {
	fs := flag.NewFlagSet("gdpr-erase-buyer", flag.ContinueOnError)
	email, customer := buyerRefFlags(fs)
	yes := fs.Bool("yes", false, "confirm the erasure (required: destructive, irreversible)")
	jsonOut := fs.Bool("json", false, "machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" && *customer == "" {
		return fmt.Errorf("gdpr-erase-buyer: -email or -customer is required")
	}
	if !*yes {
		return fmt.Errorf("gdpr-erase-buyer: destructive and irreversible. Re-run with -yes to confirm. " +
			"The buyer's email is removed from commissions, stored events and fraud notes; the " +
			"provider customer id, its lifetime binding and commission amounts stay. Run gdpr-export-buyer first if a copy is needed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	h, err := openDSN(ctx, dsn)
	if err != nil {
		return err
	}
	defer h.DB.Close()

	rep, err := privacy.NewStore(h.DB).EraseBuyer(ctx, privacy.BuyerRef{Email: *email, CustomerID: *customer})
	if err != nil {
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
	if rep.Total() == 0 {
		fmt.Println("gdpr-erase-buyer: no stored data matched; nothing changed")
		return nil
	}
	fmt.Printf("gdpr-erase-buyer: buyer erased at %s\n", rep.At)
	for table, n := range rep.Deleted {
		fmt.Printf("  deleted   %-24s %d rows\n", table, n)
	}
	for table, n := range rep.Scrubbed {
		fmt.Printf("  scrubbed  %-24s %d rows\n", table, n)
	}
	fmt.Printf("  preserved: %s\n", rep.PreservedNote)
	return nil
}
