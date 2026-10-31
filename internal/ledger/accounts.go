package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Every money account holds exactly ONE currency, and its code says which:
//
//	HOLD:<CCY>                    pending commissions (liability)
//	EXPENSE:COMMISSION:<CCY>      commission expense
//	PAYOUT_CLEARING:<CCY>         money sent by payouts
//	AFFILIATE_PAYABLE:<id>:<CCY>  what we owe one affiliate in one currency
//	TAX_WITHHELD:<CCY>            tax withheld from payouts, owed by the merchant
//	                              to its tax office (FR-107; we never remit it)
//	EXPENSE:VAT:<CCY>             VAT paid on top of commission (FR-107)
//
// Balances are therefore per currency by construction: a yen receivable
// and a dollar payable are two accounts, never one mixed number. Every
// journal moves one currency (Post refuses a journal whose accounts
// disagree, and trg_journal_balanced refuses it in the database), so a
// journal that sums to zero balances in each currency.
//
// The accounts are created on first use (Post), for system codes of the
// shapes above with a well-formed currency; an affiliate payable only for
// an affiliate that exists.
const (
	holdPrefix       = "HOLD:"
	expensePrefix    = "EXPENSE:COMMISSION:"
	clearingPrefix   = "PAYOUT_CLEARING:"
	payablePrefix    = "AFFILIATE_PAYABLE:"
	receivablePrefix = "AFFILIATE_RECEIVABLE:"
	withheldPrefix   = "TAX_WITHHELD:"
	vatPrefix        = "EXPENSE:VAT:"
)

// TaxWithheld is the withheld-tax account for a currency (FR-107): a
// liability the merchant remits; never commission.
func TaxWithheld(currency string) string { return withheldPrefix + money.Normalize(currency) }

// VATExpense is the VAT-on-top account for a currency (FR-107): kept
// apart from EXPENSE:COMMISSION so commission totals stay clean.
func VATExpense(currency string) string { return vatPrefix + money.Normalize(currency) }

// Hold is the pending-commission account for a currency.
func Hold(currency string) string { return holdPrefix + money.Normalize(currency) }

// Expense is the commission-expense account for a currency.
func Expense(currency string) string { return expensePrefix + money.Normalize(currency) }

// Clearing is the payout-clearing account for a currency.
func Clearing(currency string) string { return clearingPrefix + money.Normalize(currency) }

// Payable is an affiliate's payable account in a currency.
func Payable(affiliateID int64, currency string) string {
	return fmt.Sprintf("%s%d:%s", payablePrefix, affiliateID, money.Normalize(currency))
}

// Receivable is an affiliate's receivable account in a currency: money a
// post-payout clawback took back beyond the payable balance (the
// affiliate owes it; the next payout nets against it before staging).
// PRD 12.5 (FR-56): AFFILIATE_RECEIVABLE:<id>:<CCY>.
func Receivable(affiliateID int64, currency string) string {
	return fmt.Sprintf("%s%d:%s", receivablePrefix, affiliateID, money.Normalize(currency))
}

// accountSpec describes a per-currency system account code.
type accountSpec struct {
	kind        string
	currency    string
	affiliateID int64 // payable accounts only
}

// parseAccount recognizes a per-currency system account code. Legacy
// currency-less codes ("HOLD", "AFFILIATE_PAYABLE:42") do not parse.
func parseAccount(code string) (accountSpec, bool) {
	i := strings.LastIndexByte(code, ':')
	if i < 0 || !money.Valid(code[i+1:]) {
		return accountSpec{}, false
	}
	head, cur := code[:i+1], code[i+1:]
	switch head {
	case holdPrefix:
		return accountSpec{kind: "liability", currency: cur}, true
	case expensePrefix:
		return accountSpec{kind: "expense", currency: cur}, true
	case clearingPrefix:
		return accountSpec{kind: "clearing", currency: cur}, true
	case withheldPrefix:
		return accountSpec{kind: "liability", currency: cur}, true
	case vatPrefix:
		return accountSpec{kind: "expense", currency: cur}, true
	}
	if strings.HasPrefix(head, payablePrefix) {
		id, err := strconv.ParseInt(head[len(payablePrefix):len(head)-1], 10, 64)
		if err == nil && id > 0 {
			return accountSpec{kind: "liability", currency: cur, affiliateID: id}, true
		}
	}
	if strings.HasPrefix(head, receivablePrefix) {
		id, err := strconv.ParseInt(head[len(receivablePrefix):len(head)-1], 10, 64)
		if err == nil && id > 0 {
			// Asset: the affiliate owes us after a post-payout clawback
			// (FR-56). Same affiliate-existence check as a payable.
			return accountSpec{kind: "asset", currency: cur, affiliateID: id}, true
		}
	}
	return accountSpec{}, false
}

// AccountCurrency returns the currency of a per-currency account code.
func AccountCurrency(code string) (string, bool) {
	s, ok := parseAccount(code)
	return s.currency, ok
}

// Querier is the read surface CurrencyIntegrity needs (*sql.DB, *sql.Tx).
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// CurrencyIntegrity returns the sum of all ledger entries per currency.
// Every currency must sum to exactly 0; a single global sum could hide
// +X in one currency and -X in another. Lines on a legacy currency-less
// account (only before the per-currency migration) report under "?".
func CurrencyIntegrity(ctx context.Context, q Querier) (money.Totals, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT COALESCE(la.currency, '?'), COALESCE(SUM(le.amount), 0)
		FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
		GROUP BY la.currency`)
	if err != nil {
		return nil, fmt.Errorf("ledger: integrity: %w", err)
	}
	defer rows.Close()
	out := money.Totals{}
	for rows.Next() {
		var c string
		var v int64
		if err := rows.Scan(&c, &v); err != nil {
			return nil, fmt.Errorf("ledger: integrity: %w", err)
		}
		out[c] += v
	}
	return out, rows.Err()
}

// Balanced reports whether every currency of a CurrencyIntegrity result
// sums to zero.
func Balanced(t money.Totals) bool {
	for _, v := range t {
		if v != 0 {
			return false
		}
	}
	return true
}

func isAffiliatePayable(code string) bool {
	s, ok := parseAccount(code)
	return ok && s.affiliateID > 0
}
