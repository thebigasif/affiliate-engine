// Package payouts.stripeconnect executes payouts via Stripe Connect
// transfers (the hosted-tier rail). Money moves only through the runner's
// Approve executor hook — this file never touches the ledger directly.
package payouts

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbutil"
)

// StripeConnect executes payouts as Connect transfers to the affiliate's
// connected account.
type StripeConnect struct {
	SecretKey string       // sk_live_... / sk_test_...
	Client    *http.Client // timeout-enforced
	Now       func() time.Time
	BaseURL   string // default https://api.stripe.com (tests override)

	// DB, when set, persists each transfer id on payouts.provider_ref and
	// refuses to create a transfer for a payout that already carries one.
	DB *sql.DB

	// InstallID scopes transfer groups and idempotency keys to this
	// installation (payout ids restart after a reset and repeat across
	// installs sharing a Stripe key). Empty = read from install_identity
	// through DB; with neither, Execute refuses.
	InstallID string
}

// StripeError is a classified Stripe API failure.
type StripeError struct {
	Status int
	Code   string
	Msg    string
}

func (e *StripeError) Error() string {
	return fmt.Sprintf("stripe: %d %s: %s", e.Status, e.Code, e.Msg)
}

// Retryable: 5xx and rate limits retry; 4xx (except rate) are permanent.
func (e *StripeError) Retryable() bool {
	if e.Status == 429 {
		return true
	}
	return e.Status >= 500
}

// NewStripeConnect builds the executor from AE_STRIPE_SECRET_KEY.
func NewStripeConnect() *StripeConnect {
	return &StripeConnect{
		SecretKey: stripeKey(),
		Client:    &http.Client{Timeout: 20 * time.Second},
		Now:       time.Now,
	}
}

// Enabled reports whether the rail is configured.
func (s *StripeConnect) Enabled() bool { return s.SecretKey != "" }

// TransferGroup is the Stripe transfer_group stamped on a payout's
// transfer: the reconciliation handle that outlives Stripe's 24h
// idempotency-key window (it doubles as the Idempotency-Key). It carries
// the install id: payout ids alone repeat after a database reset and
// across installs sharing a Stripe key.
func TransferGroup(installID string, payoutID int64) string {
	return fmt.Sprintf("ae_%s_payout_%d", installID, payoutID)
}

// LegacyTransferGroup is the group releases before the install id used.
// Execute still adopts a matching transfer under it (one made before the
// upgrade) but never creates one.
func LegacyTransferGroup(payoutID int64) string { return fmt.Sprintf("ae_payout_%d", payoutID) }

// ErrTransferMismatch reports a transfer under this payout's own transfer
// group whose destination, amount or currency differ from the payout's.
// Nothing is adopted or sent; the operator reconciles.
var ErrTransferMismatch = errors.New("stripeconnect: transfer group holds a different transfer")

// Execute transfers `amount` minor units to the connected account, at most
// once per payout. Stripe's idempotency key only dedupes retries within
// 24h; beyond that a retry would create a SECOND transfer. So before
// creating, Execute (1) refuses when the payout row already carries a
// provider_ref, and (2) looks the transfer up by transfer_group (Find) and
// adopts it if one exists. Either way the transfer is reported done and
// the runner settles the ledger — money never leaves twice.
func (s *StripeConnect) Execute(payoutID, amount int64, connectedAccount, currency string) error {
	if !s.Enabled() {
		return fmt.Errorf("stripeconnect: not configured")
	}
	if connectedAccount == "" {
		return fmt.Errorf("stripeconnect: affiliate has no connected account")
	}
	if amount <= 0 {
		return fmt.Errorf("stripeconnect: non-positive amount")
	}
	id, err := s.Find(payoutID, amount, connectedAccount, currency)
	if err != nil {
		return err
	}
	if id == "" {
		inst, err := s.installID()
		if err != nil {
			return err
		}
		if id, err = s.createTransfer(payoutID, amount, connectedAccount, currency, TransferGroup(inst, payoutID)); err != nil {
			return err
		}
	}
	s.recordRef(payoutID, id)
	return nil // transferred
}

// Find returns the transfer already made for a payout ("" = none): the
// payout's recorded provider_ref, else a transfer under its transfer group
// (or the legacy group) that pays exactly this destination, amount and
// currency, created no earlier than the payout row. A transfer under the
// payout's own group that does not match is ErrTransferMismatch; one under
// the legacy group belongs to another install or a reset database and is
// ignored. The payout runner asks Find before restaging a payout whose
// rail was attempted (Runner.RailLookup).
func (s *StripeConnect) Find(payoutID, amount int64, connectedAccount, currency string) (string, error) {
	if !s.Enabled() {
		return "", fmt.Errorf("stripeconnect: not configured")
	}
	if s.Client == nil {
		s.Client = &http.Client{Timeout: 20 * time.Second}
	}
	ref, err := s.providerRef(payoutID)
	if err != nil {
		return "", err
	}
	if ref != "" {
		return ref, nil // already transferred: never re-send
	}
	inst, err := s.installID()
	if err != nil {
		return "", err
	}
	want := transferMatch{
		destination: connectedAccount, amount: amount,
		currency: strings.ToLower(currency), notBefore: s.payoutCreated(payoutID),
	}
	id, foreign, err := s.findTransfer(TransferGroup(inst, payoutID), want)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if foreign != "" {
		return "", fmt.Errorf("%w: payout %d: %s", ErrTransferMismatch, payoutID, foreign)
	}
	id, _, err = s.findTransfer(LegacyTransferGroup(payoutID), want)
	return id, err
}

// installID resolves the install id (field, else install_identity).
func (s *StripeConnect) installID() (string, error) {
	if s.InstallID != "" {
		return s.InstallID, nil
	}
	if s.DB == nil {
		return "", fmt.Errorf("stripeconnect: no install id (set InstallID or DB)")
	}
	var id string
	if err := s.DB.QueryRow(`SELECT install_id FROM install_identity WHERE id = 1`).Scan(&id); err != nil {
		return "", fmt.Errorf("stripeconnect: install id: %w", err)
	}
	if id == "" {
		return "", fmt.Errorf("stripeconnect: install id is empty")
	}
	s.InstallID = id
	return id, nil
}

// payoutCreated is when the payout row was created (zero = unknown). A
// transfer older than its payout cannot be that payout's.
func (s *StripeConnect) payoutCreated(payoutID int64) time.Time {
	if s.DB == nil {
		return time.Time{}
	}
	var raw any
	if err := s.DB.QueryRow(`SELECT created_at FROM payouts WHERE id = ?`, payoutID).Scan(&raw); err != nil {
		return time.Time{}
	}
	t, _ := dbutil.AsTime(raw)
	return t
}

// providerRef returns the transfer id already recorded for a payout ("" =
// none, or no DB wired).
func (s *StripeConnect) providerRef(payoutID int64) (string, error) {
	if s.DB == nil {
		return "", nil
	}
	var ref sql.NullString
	err := s.DB.QueryRow(`SELECT provider_ref FROM payouts WHERE id = ?`, payoutID).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stripeconnect: provider_ref lookup: %w", err)
	}
	return ref.String, nil
}

// recordRef persists the transfer id on the payout (first write wins). A
// failed write is logged, not returned: the transfer exists either way and
// the transfer_group lookup still stops a re-send.
func (s *StripeConnect) recordRef(payoutID int64, transferID string) {
	if s.DB == nil || transferID == "" {
		return
	}
	if _, err := s.DB.Exec(`
		UPDATE payouts SET provider_ref = ?
		WHERE id = ? AND (provider_ref IS NULL OR provider_ref = '')`, transferID, payoutID); err != nil {
		log.Printf("stripeconnect: persist provider_ref payout=%d: %v", payoutID, err)
	}
}

// transferMatch is what a transfer must pay to be adopted for a payout.
type transferMatch struct {
	destination string
	amount      int64
	currency    string    // lower case, as Stripe reports it
	notBefore   time.Time // payout created_at; zero = not checked
}

// createdSkew tolerates clock drift between this server and Stripe when
// comparing a transfer's creation time with its payout row's.
const createdSkew = 10 * time.Minute

// findTransfer returns the id of a transfer in the group that matches
// want ("" = none) and, when none matches, a description of the first
// transfer that did not (foreign; "" = the group is empty).
func (s *StripeConnect) findTransfer(group string, want transferMatch) (id, foreign string, err error) {
	q := url.Values{}
	q.Set("transfer_group", group)
	q.Set("limit", "10")
	req, err := http.NewRequest(http.MethodGet, s.base()+"/v1/transfers?"+q.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Data []struct {
			ID          string `json:"id"`
			Amount      int64  `json:"amount"`
			Currency    string `json:"currency"`
			Destination string `json:"destination"`
			Created     int64  `json:"created"`
		} `json:"data"`
	}
	if err := s.do(req, &out); err != nil {
		return "", "", err
	}
	for _, t := range out.Data {
		created := time.Unix(t.Created, 0)
		ok := t.Destination == want.destination && t.Amount == want.amount &&
			strings.EqualFold(t.Currency, want.currency) &&
			(want.notBefore.IsZero() || t.Created == 0 || !created.Before(want.notBefore.Add(-createdSkew)))
		if ok {
			return t.ID, "", nil
		}
		if foreign == "" {
			foreign = fmt.Sprintf("%s pays %d %s to %s", t.ID, t.Amount, t.Currency, t.Destination)
		}
	}
	return "", foreign, nil
}

// createTransfer creates the Connect transfer and returns its id.
func (s *StripeConnect) createTransfer(payoutID, amount int64, connectedAccount, currency, group string) (string, error) {
	form := url.Values{}
	form.Set("amount", fmt.Sprint(amount))
	form.Set("currency", strings.ToLower(currency))
	form.Set("destination", connectedAccount)
	form.Set("description", fmt.Sprintf("affiliate-engine payout %d", payoutID))
	form.Set("transfer_group", group)
	// metadata for reconciliation
	form.Set("metadata[payout_id]", fmt.Sprint(payoutID))

	req, err := http.NewRequest(http.MethodPost, s.base()+"/v1/transfers", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", group)
	var out struct {
		ID string `json:"id"`
	}
	if err := s.do(req, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		// A 2xx without an id is an API change or a MITM: retryable, and
		// the transfer_group lookup adopts the transfer if it did land.
		return "", &StripeError{Status: http.StatusBadGateway, Code: "missing_id", Msg: "transfer response without id"}
	}
	return out.ID, nil
}

func (s *StripeConnect) base() string {
	if s.BaseURL == "" {
		return "https://api.stripe.com"
	}
	return s.BaseURL
}

// do sends an authenticated Stripe request and decodes a 2xx body into out.
func (s *StripeConnect) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+s.SecretKey)
	req.Header.Set("Stripe-Version", "2024-06-20")

	resp, err := s.Client.Do(req)
	if err != nil {
		return &StripeError{Status: 0, Code: "network", Msg: err.Error()}
	}
	defer resp.Body.Close()

	body := bytes.NewBuffer(nil)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		return err
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body.Bytes(), &env); err != nil {
		// A 2xx with an unparsable body means an API change or a MITM —
		// surface it instead of silently reporting "not transferred".
		return &StripeError{Status: resp.StatusCode, Code: "bad_json", Msg: err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		se := &StripeError{Status: resp.StatusCode, Code: env.Error.Code, Msg: env.Error.Message}
		if se.Code == "" {
			se.Code = "http_" + fmt.Sprint(resp.StatusCode)
		}
		return se
	}
	if err := json.Unmarshal(body.Bytes(), out); err != nil {
		return &StripeError{Status: resp.StatusCode, Code: "bad_json", Msg: err.Error()}
	}
	return nil
}

// stripeKey resolves the Stripe API key from either env name (the
// webhooks name is usually the same restricted key). AE_STRIPE_SECRET
// wins if both are set.
func stripeKey() string {
	if k := os.Getenv("AE_STRIPE_SECRET_KEY"); k != "" {
		return k
	}
	return os.Getenv("AE_STRIPE_SECRET")
}
