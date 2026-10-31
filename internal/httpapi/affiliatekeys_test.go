package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/privacy"
)

func patchAffiliateStatus(t *testing.T, f *apiFixture, id, status string) {
	t.Helper()
	req, _ := http.NewRequest("PATCH", f.srv.URL+"/api/v1/admin/affiliates/"+id,
		bytes.NewReader([]byte(`{"status":"`+status+`"}`)))
	req.Header.Set("Authorization", "Bearer "+f.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set status %s: code=%d", status, resp.StatusCode)
	}
}

// affiliateKeyCodes returns the status codes a key gets on one read
// route and one write route. For a working key the write only has to
// pass authentication (not 401): the route may still refuse API keys.
func affiliateKeyCodes(t *testing.T, f *apiFixture, key string) (read, write int) {
	t.Helper()
	read, _ = getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", key)
	write, _ = putPayoutDetails(t, f, key, `{"method":"paypal","paypal_email":"pp@x.test"}`)
	return read, write
}

// A frozen partner's API key stops working at once, on reads and writes.
// Freezing revokes the key, so resuming the partner does NOT bring it
// back: the owner issues a new key. Admin keys are unaffected.
func TestAffiliateKey_FrozenPartnerLocksOut(t *testing.T) {
	f := setupAPI(t)
	if r, w := affiliateKeyCodes(t, f, f.aff1Key); r != http.StatusOK || w == http.StatusUnauthorized {
		t.Fatalf("active partner key: read=%d write=%d want 200/authenticated", r, w)
	}

	patchAffiliateStatus(t, f, "1", "frozen")
	if r, w := affiliateKeyCodes(t, f, f.aff1Key); r != http.StatusUnauthorized || w != http.StatusUnauthorized {
		t.Fatalf("frozen partner key: read=%d write=%d want 401/401", r, w)
	}
	// The other partner and the admin key keep working.
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", f.aff2Key); code != http.StatusOK {
		t.Fatalf("other partner key: code=%d want 200", code)
	}
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", f.adminKey); code != http.StatusOK {
		t.Fatalf("admin key: code=%d want 200", code)
	}
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings?affiliate_id=1", f.adminKey); code != http.StatusOK {
		t.Fatalf("admin read of frozen partner: code=%d want 200", code)
	}

	// Resume: the revoked key stays dead; a newly issued key works.
	patchAffiliateStatus(t, f, "1", "active")
	if r, w := affiliateKeyCodes(t, f, f.aff1Key); r != http.StatusUnauthorized || w != http.StatusUnauthorized {
		t.Fatalf("revoked key after resume: read=%d write=%d want 401/401", r, w)
	}
	fresh, err := apikeys.Store(context.Background(), f.db.DB, "aff1 new", apikeys.AffiliateScope(1))
	if err != nil {
		t.Fatal(err)
	}
	if r, w := affiliateKeyCodes(t, f, fresh); r != http.StatusOK || w == http.StatusUnauthorized {
		t.Fatalf("new key after resume: read=%d write=%d want 200/authenticated", r, w)
	}
}

// A key that was not revoked (status changed outside the freeze paths,
// for example by an import) is refused while the partner is frozen and
// works again once the partner is active.
func TestAffiliateKey_StatusGateWithoutRevocation(t *testing.T) {
	f := setupAPI(t)
	if _, err := f.db.DB.Exec(`UPDATE affiliates SET status = 'frozen' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if r, w := affiliateKeyCodes(t, f, f.aff1Key); r != http.StatusUnauthorized || w != http.StatusUnauthorized {
		t.Fatalf("frozen partner key: read=%d write=%d want 401/401", r, w)
	}
	if _, err := f.db.DB.Exec(`UPDATE affiliates SET status = 'active' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if r, _ := affiliateKeyCodes(t, f, f.aff1Key); r != http.StatusOK {
		t.Fatalf("unrevoked key after resume: read=%d want 200", r)
	}
}

// An erased (removed) partner's key is refused on reads and writes, even
// if the key row somehow escaped revocation.
func TestAffiliateKey_RemovedPartnerLocksOut(t *testing.T) {
	f := setupAPI(t)
	if _, err := privacy.NewStore(f.db.DB).Erase(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if r, w := affiliateKeyCodes(t, f, f.aff1Key); r != http.StatusUnauthorized || w != http.StatusUnauthorized {
		t.Fatalf("removed partner key: read=%d write=%d want 401/401", r, w)
	}
	// Even an unrevoked key for a removed partner is refused.
	if _, err := f.db.DB.Exec(`UPDATE api_keys SET revoked_at = NULL`); err != nil {
		t.Fatal(err)
	}
	if r, w := affiliateKeyCodes(t, f, f.aff1Key); r != http.StatusUnauthorized || w != http.StatusUnauthorized {
		t.Fatalf("unrevoked removed partner key: read=%d write=%d want 401/401", r, w)
	}
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", f.adminKey); code != http.StatusOK {
		t.Fatalf("admin key: code=%d want 200", code)
	}
}
