package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ProductFilter limits Stripe commissions to sales of listed products,
// prices or payment links (AE_STRIPE_PRODUCTS). One Stripe account often
// sells several products; without a filter every sale on the account is
// commissionable. The zero value is inactive: every sale counts.
type ProductFilter struct {
	IDs map[string]bool
	// LineItems fetches a checkout session's price and product ids when
	// the webhook payload carries none (Stripe leaves line items out
	// unless expanded). nil = such sessions are skipped (fail closed).
	LineItems func(ctx context.Context, sessionID string) ([]string, error)
}

var productIDRe = regexp.MustCompile(`^(prod|price|plink)_[A-Za-z0-9]{1,200}$`)

// ParseProductFilter reads "prod_A, price_B plink_C" (commas or spaces).
// Anything that is not a Stripe product, price or payment link id is an
// error: a typo must not silently count (or drop) every sale.
func ParseProductFilter(list string) (ProductFilter, error) {
	f := ProductFilter{IDs: map[string]bool{}}
	for _, id := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' }) {
		if !productIDRe.MatchString(id) {
			return ProductFilter{}, fmt.Errorf("billing: AE_STRIPE_PRODUCTS: %q is not a Stripe product (prod_), price (price_) or payment link (plink_) id", id)
		}
		f.IDs[id] = true
	}
	return f, nil
}

// Active reports whether the filter restricts anything.
func (f ProductFilter) Active() bool { return len(f.IDs) > 0 }

func (f ProductFilter) match(ids ...string) bool {
	for _, id := range ids {
		if id != "" && f.IDs[id] {
			return true
		}
	}
	return false
}

// matchLines: any line whose price or product is listed makes the sale
// count (a bundle containing one of ours is ours).
func (f ProductFilter) matchLines(lines []lineItem) bool {
	for _, l := range lines {
		if f.match(l.priceID(), l.productID()) {
			return true
		}
	}
	return false
}

// checkoutAllowed decides a payment-mode session: its payment link, then
// the line items in the payload, then (if configured) line items fetched
// from Stripe. With nothing to match on, the sale does not count.
func (a *StripeAdapter) checkoutAllowed(ctx context.Context, cs checkoutSession) (bool, error) {
	f := a.Products
	if f.match(string(cs.PaymentLink)) || f.matchLines(cs.LineItems) {
		return true, nil
	}
	if len(cs.LineItems) > 0 {
		return false, nil // the payload named its products, none listed
	}
	if f.LineItems == nil {
		log.Printf("billing: stripe checkout %s: no line items in the webhook and no AE_STRIPE_API_KEY to look them up; not counted", cs.ID)
		return false, nil
	}
	ids, err := f.LineItems(ctx, cs.ID)
	if err != nil {
		return false, fmt.Errorf("billing: stripe checkout %s line items: %w", cs.ID, err)
	}
	return f.match(ids...), nil
}

// stripeAPIVersion pins the response shape the line-item reader parses.
const stripeAPIVersion = "2024-06-20"

var sessionIDRe = regexp.MustCompile(`^cs_[A-Za-z0-9_]{1,200}$`)

// StripeLineItems returns a ProductFilter.LineItems reader that asks the
// Stripe API with key (a restricted key with Checkout Sessions: read is
// enough). base "" = https://api.stripe.com.
func StripeLineItems(key, base string) func(ctx context.Context, sessionID string) ([]string, error) {
	if base == "" {
		base = "https://api.stripe.com"
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context, sessionID string) ([]string, error) {
		if !sessionIDRe.MatchString(sessionID) {
			return nil, errors.New("not a checkout session id")
		}
		u := base + "/v1/checkout/sessions/" + url.PathEscape(sessionID) + "/line_items?limit=100"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Stripe-Version", stripeAPIVersion)
		resp, err := client.Do(req) // #nosec G704 -- fixed Stripe API host (or a test server); the path is a validated session id
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("stripe API %d", resp.StatusCode)
		}
		var out struct {
			Data []lineItem `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		var ids []string
		for _, l := range out.Data {
			ids = append(ids, l.priceID(), l.productID())
		}
		return ids, nil
	}
}
