package importer

// history.go — the rest of a rival export: referrals and commissions.
//
// Referrals with a customer id become lifetime customer bindings on a real
// run, so renewals from customers an affiliate already brought keep
// earning here. Commissions are READ ONLY: they feed the review (counts,
// conflicts, fraud) and the side-by-side compare, and are never written
// to the ledger (see the package comment).

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Referral is one normalized row of a rival's referrals export.
type Referral struct {
	ExternalID     string
	AffiliateEmail string
	AffiliateCode  string
	AffiliateExtID string
	CustomerEmail  string
	CustomerID     string // billing customer id (Stripe cus_..., Paddle ctm_...)
	Coupon         string
	Status         string // lowercased as exported
	At             time.Time
}

// Commission is one normalized row of a rival's commissions export.
type Commission struct {
	ExternalID     string
	AffiliateEmail string
	AffiliateCode  string
	AffiliateExtID string
	CustomerEmail  string
	CustomerID     string
	Coupon         string
	Amount         int64 // minor units, signed
	Currency       string
	Status         string
	At             time.Time
}

// DayCount is one day of a rival's click export.
type DayCount struct {
	Day   time.Time // UTC midnight
	Count int64
}

// FileInfo describes one parsed export file.
type FileInfo struct {
	Kind    string          `json:"kind"`
	Rows    int             `json:"rows"`
	Kept    int             `json:"kept"`
	Skipped int             `json:"skipped"`
	Columns []string        `json:"columns"` // semantic columns found
	has     map[string]bool // same, as a set
	Issues  Issues          `json:"issues,omitempty"`
}

// Has reports whether the file carried a semantic column.
func (f *FileInfo) Has(seat string) bool { return f != nil && f.has[seat] }

// Issue is one kind of conflict found in the input, with a few examples.
type Issue struct {
	Count    int      `json:"count"`
	Examples []string `json:"examples,omitempty"`
}

// Issues groups conflicts by kind.
type Issues map[string]*Issue

const maxExamples = 5

func (is Issues) add(kind, example string) {
	it := is[kind]
	if it == nil {
		it = &Issue{}
		is[kind] = it
	}
	it.Count++
	if example != "" && len(it.Examples) < maxExamples {
		it.Examples = append(it.Examples, example)
	}
}

func (is Issues) merge(o Issues) {
	for k, v := range o {
		it := is[k]
		if it == nil {
			it = &Issue{}
			is[k] = it
		}
		it.Count += v.Count
		for _, e := range v.Examples {
			if len(it.Examples) < maxExamples {
				it.Examples = append(it.Examples, e)
			}
		}
	}
}

// Kinds returns the issue kinds in a stable order.
func (is Issues) Kinds() []string {
	out := make([]string, 0, len(is))
	for k := range is {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IssueText explains each conflict kind in plain words (CLI output).
var IssueText = map[string]string{
	"dup_email":               "email appears more than once in the affiliates file (first row kept)",
	"existing_email":          "affiliate already exists here (row untouched, codes and coupons attach)",
	"code_taken":              "referral code already belongs to another affiliate (imported with a suffix)",
	"coupon_taken":            "coupon already belongs to another affiliate (not attached)",
	"invalid_email":           "row has no valid email (skipped)",
	"unknown_affiliate":       "row names an affiliate that is not in the affiliates file or here (skipped)",
	"unknown_currency":        "currency is not an ISO 4217 code (left out of totals)",
	"missing_currency":        "no currency on the row (assumed USD)",
	"bad_amount":              "amount could not be read (row skipped)",
	"bad_date":                "date could not be read (left out of date-based checks)",
	"dup_id":                  "the same row id appears more than once (first row kept)",
	"no_customer_id":          "referral has no customer id: renewals from it cannot be credited here",
	"customer_bound":          "customer is already bound to a different affiliate here (kept as is)",
	"customer_multi_referral": "customer was referred by more than one affiliate in the export (earliest kept)",
}

// normHeader: lowercase, '_' and '-' to spaces, collapse whitespace.
func normHeader(h string) string {
	h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))
	h = strings.NewReplacer("_", " ", "-", " ").Replace(h)
	return strings.Join(strings.Fields(h), " ")
}

// seatSpec maps a semantic column to header candidates, in priority order.
type seatSpec struct {
	seat       string
	candidates []string
}

// mapHeader assigns each seat the first candidate the header carries. A
// header column is never given to two seats.
func mapHeader(header []string, specs []seatSpec) map[string]int {
	norm := make([]string, len(header))
	for i, h := range header {
		norm[i] = normHeader(h)
	}
	used := map[int]bool{}
	col := map[string]int{}
	for _, s := range specs {
	cands:
		for _, c := range s.candidates {
			c = normHeader(c)
			for i, h := range norm {
				if h == c && !used[i] {
					col[s.seat] = i
					used[i] = true
					break cands
				}
			}
		}
	}
	return col
}

var affLinkSeats = []seatSpec{
	{"aff_email", []string{"affiliate email", "promoter email", "partner email", "affiliate email address", "affiliate"}},
	{"aff_ext", []string{"affiliate id", "promoter id", "partner id", "affiliate uuid"}},
	{"aff_code", []string{"affiliate code", "referral code", "affiliate token", "link token", "ref id", "token", "via"}},
}

var customerSeats = []seatSpec{
	{"cust_id", []string{"stripe customer id", "stripe customer", "customer id", "customer uid", "external customer id", "uid"}},
	{"coupon", []string{"coupon", "coupon code", "promo code", "coupons"}},
	{"status", []string{"status", "state", "conversion state"}},
}

func referralSeats() []seatSpec {
	s := append([]seatSpec{}, affLinkSeats...)
	s = append(s, customerSeats...)
	return append(s,
		seatSpec{"id", []string{"id", "referral id", "uuid"}},
		seatSpec{"cust_email", []string{"customer email", "lead email", "referral email", "buyer email", "email"}},
		seatSpec{"at", []string{"converted at", "conversion date", "became customer at", "created at", "created", "date", "signup date"}},
	)
}

func commissionSeats() []seatSpec {
	s := []seatSpec{{"amount", []string{"amount", "commission amount", "commission", "reward amount", "amount cents", "commission amount cents"}}}
	s = append(s, affLinkSeats...)
	s = append(s, customerSeats...)
	return append(s,
		seatSpec{"id", []string{"id", "commission id", "reward id"}},
		// No bare "email": in a commissions export it is as often the
		// affiliate's as the buyer's.
		seatSpec{"cust_email", []string{"customer email", "referral email", "lead email", "buyer email"}},
		seatSpec{"currency", []string{"currency", "currency code", "commission currency"}},
		seatSpec{"at", []string{"created at", "created", "date", "earned at", "commission date"}},
	)
}

// csvRows reads a header + records with the incumbent-tolerant settings.
func csvRows(r io.Reader) ([]string, *csv.Reader, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	cr.LazyQuotes = true
	header, err := cr.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("importer: read header: %w", err)
	}
	return header, cr, nil
}

func newInfo(kind string, header []string, col map[string]int) *FileInfo {
	fi := &FileInfo{Kind: kind, has: map[string]bool{}, Issues: Issues{}}
	for seat := range col {
		fi.has[seat] = true
		fi.Columns = append(fi.Columns, seat)
	}
	sort.Strings(fi.Columns)
	return fi
}

func getter(col map[string]int, rec []string) func(string) string {
	return func(seat string) string {
		i, ok := col[seat]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
}

// ParseReferrals reads a rival's referrals export.
func ParseReferrals(src Source, r io.Reader) ([]Referral, *FileInfo, error) {
	header, cr, err := csvRows(r)
	if err != nil {
		return nil, nil, err
	}
	col := mapHeader(header, referralSeats())
	if !hasAny(col, "aff_email", "aff_ext", "aff_code") {
		return nil, nil, fmt.Errorf("importer: %s referrals: no affiliate column (affiliate email, affiliate id or referral code) in header %v", src, header)
	}
	fi := newInfo("referrals", header, col)
	seen := map[string]bool{}
	var out []Referral
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fi, fmt.Errorf("importer: referrals row %d: %w", fi.Rows+1, err)
		}
		fi.Rows++
		get := getter(col, rec)
		ref := Referral{
			ExternalID:     get("id"),
			AffiliateEmail: strings.ToLower(get("aff_email")),
			AffiliateCode:  normalizeSlug(get("aff_code")),
			AffiliateExtID: get("aff_ext"),
			CustomerEmail:  strings.ToLower(get("cust_email")),
			CustomerID:     get("cust_id"),
			Coupon:         strings.ToLower(get("coupon")),
			Status:         strings.ToLower(get("status")),
		}
		if ref.ExternalID != "" {
			if seen[ref.ExternalID] {
				fi.Issues.add("dup_id", "referral "+ref.ExternalID)
				fi.Skipped++
				continue
			}
			seen[ref.ExternalID] = true
		}
		if v := get("at"); v != "" {
			t, ok := parseWhen(v)
			if !ok {
				fi.Issues.add("bad_date", fmt.Sprintf("referral row %d: %q", fi.Rows, v))
			}
			ref.At = t
		}
		out = append(out, ref)
	}
	fi.Kept = len(out)
	return out, fi, nil
}

// ParseCommissions reads a rival's commissions export.
func ParseCommissions(src Source, r io.Reader) ([]Commission, *FileInfo, error) {
	header, cr, err := csvRows(r)
	if err != nil {
		return nil, nil, err
	}
	col := mapHeader(header, commissionSeats())
	if _, ok := col["amount"]; !ok || !hasAny(col, "aff_email", "aff_ext", "aff_code") {
		return nil, nil, fmt.Errorf("importer: %s commissions: need an amount and an affiliate column in header %v", src, header)
	}
	cents := strings.Contains(normHeader(header[col["amount"]]), "cents")
	fi := newInfo("commissions", header, col)
	seen := map[string]bool{}
	var out []Commission
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fi, fmt.Errorf("importer: commissions row %d: %w", fi.Rows+1, err)
		}
		fi.Rows++
		get := getter(col, rec)
		c := Commission{
			ExternalID:     get("id"),
			AffiliateEmail: strings.ToLower(get("aff_email")),
			AffiliateCode:  normalizeSlug(get("aff_code")),
			AffiliateExtID: get("aff_ext"),
			CustomerEmail:  strings.ToLower(get("cust_email")),
			CustomerID:     get("cust_id"),
			Coupon:         strings.ToLower(get("coupon")),
			Status:         strings.ToLower(get("status")),
		}
		if c.ExternalID != "" {
			if seen[c.ExternalID] {
				fi.Issues.add("dup_id", "commission "+c.ExternalID)
				fi.Skipped++
				continue
			}
			seen[c.ExternalID] = true
		}
		c.Currency = strings.ToUpper(get("currency"))
		if c.Currency == "" {
			c.Currency = "USD"
			if fi.has["currency"] {
				fi.Issues.add("missing_currency", fmt.Sprintf("commission row %d", fi.Rows))
			}
		} else if !knownCurrency(c.Currency) {
			fi.Issues.add("unknown_currency", fmt.Sprintf("commission row %d: %q", fi.Rows, c.Currency))
		}
		amt, ok := parseAmount(get("amount"), c.Currency, cents)
		if !ok {
			fi.Issues.add("bad_amount", fmt.Sprintf("commission row %d: %q", fi.Rows, get("amount")))
			fi.Skipped++
			continue
		}
		c.Amount = amt
		if v := get("at"); v != "" {
			t, ok := parseWhen(v)
			if !ok {
				fi.Issues.add("bad_date", fmt.Sprintf("commission row %d: %q", fi.Rows, v))
			}
			c.At = t
		}
		out = append(out, c)
	}
	fi.Kept = len(out)
	return out, fi, nil
}

// ParseClicks reads a rival's click/visit export: one row per click, or
// one row per day with a count column.
func ParseClicks(src Source, r io.Reader) ([]DayCount, *FileInfo, error) {
	header, cr, err := csvRows(r)
	if err != nil {
		return nil, nil, err
	}
	col := mapHeader(header, []seatSpec{
		{"at", []string{"date", "day", "created at", "created", "visited at", "clicked at", "timestamp"}},
		{"count", []string{"clicks", "visits", "visitors", "count"}},
	})
	if _, ok := col["at"]; !ok {
		return nil, nil, fmt.Errorf("importer: %s clicks: no date column in header %v", src, header)
	}
	fi := newInfo("clicks", header, col)
	byDay := map[time.Time]int64{}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fi, fmt.Errorf("importer: clicks row %d: %w", fi.Rows+1, err)
		}
		fi.Rows++
		get := getter(col, rec)
		t, ok := parseWhen(get("at"))
		if !ok {
			fi.Issues.add("bad_date", fmt.Sprintf("clicks row %d: %q", fi.Rows, get("at")))
			fi.Skipped++
			continue
		}
		n := int64(1)
		if fi.has["count"] {
			v, err := strconv.ParseInt(strings.ReplaceAll(get("count"), ",", ""), 10, 64)
			if err != nil || v < 0 {
				fi.Issues.add("bad_amount", fmt.Sprintf("clicks row %d: %q", fi.Rows, get("count")))
				fi.Skipped++
				continue
			}
			n = v
		}
		byDay[dayOf(t)] += n
	}
	out := make([]DayCount, 0, len(byDay))
	for d, n := range byDay {
		out = append(out, DayCount{Day: d, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	fi.Kept = fi.Rows - fi.Skipped
	return out, fi, nil
}

func hasAny(col map[string]int, seats ...string) bool {
	for _, s := range seats {
		if _, ok := col[s]; ok {
			return true
		}
	}
	return false
}

func dayOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

var whenLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05 -0700 MST",
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"01/02/2006 15:04:05",
	"01/02/2006 15:04",
	"01/02/2006",
	"Jan 2, 2006 15:04",
	"Jan 2, 2006",
	"2 Jan 2006",
}

// parseWhen reads the date shapes rival exports use. Zone-less values are
// taken as UTC. Unix seconds are accepted too.
func parseWhen(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	for _, l := range whenLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t.UTC(), true
		}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 946684800 && n < 4102444800 {
		return time.Unix(n, 0).UTC(), true
	}
	return time.Time{}, false
}

// parseAmount reads "12.50", "$1,234.56", "-3", "12,50" into minor units.
// cents=true means the column is already in minor units.
func parseAmount(v, currency string, cents bool) (int64, bool) {
	v = strings.TrimSpace(v)
	neg := false
	if strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")") {
		neg, v = true, strings.Trim(v, "()")
	}
	var b strings.Builder
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9', c == '.', c == ',':
			b.WriteRune(c)
		case c == '-':
			neg = !neg
		}
	}
	s := b.String()
	if s == "" {
		return 0, false
	}
	// A lone comma followed by 1-2 digits is a decimal comma ("12,50").
	if !strings.Contains(s, ".") && strings.Count(s, ",") == 1 {
		if i := strings.Index(s, ","); len(s)-i-1 <= 2 {
			s = s[:i] + "." + s[i+1:]
		}
	}
	s = strings.ReplaceAll(s, ",", "")
	if cents {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		if neg {
			n = -n
		}
		return n, true
	}
	whole, frac, _ := strings.Cut(s, ".")
	if strings.Contains(frac, ".") {
		return 0, false
	}
	exp := money.Exponent(currency)
	if len(frac) > exp {
		// Round half up on the dropped digits.
		f, err := strconv.ParseFloat("0."+frac, 64)
		if err != nil {
			return 0, false
		}
		frac = strconv.FormatInt(int64(math.Round(f*math.Pow10(exp))), 10)
		if len(frac) > exp { // rounded up to a whole unit
			w, _ := strconv.ParseInt(or0(whole), 10, 64)
			whole, frac = strconv.FormatInt(w+1, 10), ""
		}
	}
	for len(frac) < exp {
		frac += "0"
	}
	n, err := strconv.ParseInt(or0(whole)+frac, 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

func or0(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// FormatMinor renders minor units in a currency's major unit ("12.50").
func FormatMinor(n int64, currency string) string {
	return money.Format(n, currency)
}

// iso4217 lists the active ISO 4217 currency codes.
const iso4217 = "AED AFN ALL AMD ANG AOA ARS AUD AWG AZN BAM BBD BDT BGN BHD BIF BMD BND BOB BOV BRL BSD BTN BWP BYN BZD " +
	"CAD CDF CHE CHF CHW CLF CLP CNY COP COU CRC CUC CUP CVE CZK DJF DKK DOP DZD EGP ERN ETB EUR FJD FKP GBP GEL GHS GIP " +
	"GMD GNF GTQ GYD HKD HNL HTG HUF IDR ILS INR IQD IRR ISK JMD JOD JPY KES KGS KHR KMF KPW KRW KWD KYD KZT LAK LBP LKR " +
	"LRD LSL LYD MAD MDL MGA MKD MMK MNT MOP MRU MUR MVR MWK MXN MXV MYR MZN NAD NGN NIO NOK NPR NZD OMR PAB PEN PGK PHP " +
	"PKR PLN PYG QAR RON RSD RUB RWF SAR SBD SCR SDG SEK SGD SHP SLE SLL SOS SRD SSP STN SVC SYP SZL THB TJS TMT TND TOP " +
	"TRY TTD TWD TZS UAH UGX USD USN UYI UYU UYW UZS VED VES VND VUV WST XAF XCD XCG XOF XPF YER ZAR ZMW ZWG ZWL"

var currencySet = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(iso4217) {
		m[c] = true
	}
	return m
}()

func knownCurrency(c string) bool { return currencySet[c] }
