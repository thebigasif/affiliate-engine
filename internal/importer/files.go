package importer

import (
	"context"
	"database/sql"
	"fmt"
	"io"
)

// ReadDataset parses an incumbent export: the affiliates file, plus the
// optional referrals and commissions files (nil = not supplied). The
// returned report carries the affiliates file's parse-level counts and
// conflicts; Run folds it into the import report.
func ReadDataset(src Source, affiliates, referrals, commissions io.Reader) (Dataset, *Report, error) {
	var ds Dataset
	affs, preport, err := Parse(src, affiliates)
	if err != nil {
		return ds, nil, fmt.Errorf("affiliates file: %w", err)
	}
	ds.Affiliates = affs
	if referrals != nil {
		refs, info, err := ParseReferrals(src, referrals)
		if err != nil {
			return ds, nil, fmt.Errorf("referrals file: %w", err)
		}
		ds.Referrals, ds.Files = refs, append(ds.Files, info)
	}
	if commissions != nil {
		coms, info, err := ParseCommissions(src, commissions)
		if err != nil {
			return ds, nil, fmt.Errorf("commissions file: %w", err)
		}
		ds.Commissions, ds.Files = coms, append(ds.Files, info)
	}
	return ds, preport, nil
}

// Run reviews (dryRun) or imports a dataset read by ReadDataset and folds
// the affiliates file's parse-level counts and conflicts into the report.
func Run(ctx context.Context, db *sql.DB, src Source, ds Dataset, preport *Report, dryRun bool) (*Report, error) {
	report, err := MigrateDataset(ctx, db, src, ds, dryRun)
	if err != nil {
		return nil, err
	}
	if preport == nil {
		return report, nil
	}
	if len(preport.Conflicts) > 0 {
		if report.Conflicts == nil {
			report.Conflicts = Issues{}
		}
		for k, v := range preport.Conflicts {
			report.Conflicts[k] = v
		}
	}
	report.Rows, report.Skipped, report.DupEmails = preport.Rows, preport.Skipped, preport.DupEmails
	return report, nil
}

// LinkParam is the query parameter the incumbent's affiliate links use
// (yoursite.com/?via=CODE); t.js data-param keeps those links working.
func LinkParam(src Source) string {
	return map[Source]string{Rewardful: "via", FirstPromoter: "fpr", Tolt: "ref", Tapfiliate: "ref"}[src]
}
