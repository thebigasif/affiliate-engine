package httpapi

// Cloud plan gates (AE_PLAN): a workspace still on the retired Starter
// plan is limited to one program and refused the other conveniences with
// a 402 and an upgrade hint naming the Cloud plan; the Cloud plan
// (standard), the retired Business plan and self-host (nil plan) have
// no such gates.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/plan"
)

func planServer(t *testing.T, planName string) (*httptest.Server, string) {
	t.Helper()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse(planName)
	if err != nil {
		t.Fatal(err)
	}
	key, err := apikeys.Store(context.Background(), h.DB, "admin", apikeys.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewAPIHandler(APIDeps{DB: h.DB, Plan: p}))
	t.Cleanup(srv.Close)
	return srv, key
}

func send(t *testing.T, method, url, key, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPlanGate_Starter(t *testing.T) {
	srv, key := planServer(t, "starter")
	u := srv.URL + "/api/v1/admin"

	if code, body := send(t, "POST", u+"/campaigns", key, `{"name":"Main"}`); code != http.StatusCreated {
		t.Fatalf("first program: %d %s", code, body)
	}
	code, body := send(t, "POST", u+"/campaigns", key, `{"name":"Second"}`)
	if code != http.StatusPaymentRequired || !strings.Contains(body, "Cloud plan") {
		t.Fatalf("second program on starter: %d %s", code, body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/branding?campaign_id=1", `{"brand_name":"X"}`},
		{"POST", "/mlm/run", `{}`},
		{"POST", "/mlm/place", `{}`},
		{"POST", "/usage", `{"events":[]}`},
		{"PATCH", "/affiliates/1", `{"postback_url":"https://example.com/pb"}`},
	} {
		if code, body := send(t, c.method, u+c.path, key, c.body); code != http.StatusPaymentRequired {
			t.Fatalf("%s %s on starter: %d %s", c.method, c.path, code, body)
		}
	}
	// Reads and money paths stay open on every plan.
	if code, body := send(t, "GET", u+"/branding?campaign_id=1", key, ""); code == http.StatusPaymentRequired {
		t.Fatalf("branding read gated: %s", body)
	}
	if code, body := send(t, "GET", u+"/dashboard", key, ""); code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", code, body)
	}
	// Clearing a postback URL is always allowed.
	if code, body := send(t, "PATCH", u+"/affiliates/1", key, `{"postback_url":""}`); code == http.StatusPaymentRequired {
		t.Fatalf("clearing postback gated: %s", body)
	}
}

func TestPlanGate_StandardBusinessAndSelfHost(t *testing.T) {
	for _, name := range []string{"standard", "business", ""} {
		srv, key := planServer(t, name)
		u := srv.URL + "/api/v1/admin"
		for i, n := range []string{"A", "B", "C"} {
			if code, body := send(t, "POST", u+"/campaigns", key, `{"name":"`+n+`"}`); code != http.StatusCreated {
				t.Fatalf("plan %q program %d: %d %s", name, i, code, body)
			}
		}
		if code, body := send(t, "PUT", u+"/branding?campaign_id=1", key, `{"brand_name":"X"}`); code == http.StatusPaymentRequired {
			t.Fatalf("plan %q branding gated: %s", name, body)
		}
		if code, body := send(t, "PATCH", u+"/affiliates/1", key, `{"postback_url":"https://example.com/pb"}`); code == http.StatusPaymentRequired {
			t.Fatalf("plan %q postbacks gated: %s", name, body)
		}
	}
}
