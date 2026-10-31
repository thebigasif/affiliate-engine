package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// recurlyAPIAccept pins the Recurly API v3 version whose account shape
// (custom_fields [{name, value}]) the token reader parses.
const recurlyAPIAccept = "application/vnd.recurly.v2021-02-25+json"

// ErrRecurlyAPIKey: Recurly refused the API key (401/403). Retrying the
// webhook cannot fix it, so the adapter logs it and carries on without
// the token instead of erroring.
var ErrRecurlyAPIKey = errors.New("recurly: API key refused")

// recurlyAPIHosts are the only bases the API key is ever sent to.
var recurlyAPIHosts = map[string]bool{"https://v3.recurly.com": true, "https://v3.eu.recurly.com": true}

// RecurlyAccountToken returns a reader for an account's aff_token custom
// field, asking the Recurly API with key (a read-only API key is enough).
// base "" = https://v3.recurly.com (EU sites use https://v3.eu.recurly.com).
// Any other base is refused without a request (ErrRecurlyAPIKey: logged,
// the sale ACKed). An unknown account or an account without the field
// reads as "".
func RecurlyAccountToken(key, base string) func(ctx context.Context, accountCode string) (string, error) {
	if base == "" {
		base = "https://v3.recurly.com"
	}
	base = strings.TrimRight(base, "/")
	if !recurlyAPIHosts[base] {
		return func(context.Context, string) (string, error) {
			return "", fmt.Errorf("%w: AE_RECURLY_API_BASE %q is not a Recurly API host", ErrRecurlyAPIKey, base)
		}
	}
	return recurlyAccountToken(key, base)
}

// recurlyAccountToken asks base as given (tests point it at their own
// server through export_test.go).
func recurlyAccountToken(key, base string) func(ctx context.Context, accountCode string) (string, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		// The key rides on every request: never follow a redirect elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context, accountCode string) (string, error) {
		if accountCode == "" || len(accountCode) > 200 {
			return "", nil
		}
		u := base + "/accounts/code-" + url.PathEscape(accountCode)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		req.SetBasicAuth(key, "")
		req.Header.Set("Accept", recurlyAPIAccept)
		resp, err := client.Do(req) // #nosec G704 -- base is an allowlisted Recurly API host (recurlyAPIHosts; only tests reach recurlyAccountToken directly), redirects are not followed, the path is an escaped account code
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", err
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return "", nil
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return "", ErrRecurlyAPIKey
		case resp.StatusCode != http.StatusOK:
			return "", fmt.Errorf("recurly API %d", resp.StatusCode)
		}
		var acct struct {
			CustomFields []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"custom_fields"`
		}
		if err := json.Unmarshal(body, &acct); err != nil {
			return "", err
		}
		for _, f := range acct.CustomFields {
			if f.Name == "aff_token" {
				return strings.TrimSpace(f.Value), nil
			}
		}
		return "", nil
	}
}
