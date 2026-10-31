package plan

import (
	"strings"
	"testing"
)

var every = []string{FeatMultiProgram, FeatBranding, FeatMLM, FeatUsage, FeatPostbacks, FeatMCP, FeatWidget}

func TestSelfHostAllowsEverything(t *testing.T) {
	p, err := Parse("")
	if err != nil || p != nil {
		t.Fatalf("empty AE_PLAN: p=%v err=%v", p, err)
	}
	for _, f := range every {
		if !p.Allows(f) {
			t.Fatalf("self-host must allow %s", f)
		}
	}
	if p.MaxCampaigns() != 0 {
		t.Fatal("self-host has no program limit")
	}
}

// The one Cloud plan sold today includes everything self-host has.
func TestStandardAllowsEverything(t *testing.T) {
	p, err := Parse(" Standard ")
	if err != nil || p == nil || p.Name != Standard {
		t.Fatalf("standard: p=%v err=%v", p, err)
	}
	for _, f := range every {
		if !p.Allows(f) {
			t.Fatalf("standard must allow %s", f)
		}
	}
	if p.MaxCampaigns() != 0 {
		t.Fatal("standard has no program limit")
	}
	if DisplayName(Standard) != "Cloud" {
		t.Fatalf("display name %q", DisplayName(Standard))
	}
}

// Retired plans still parse and keep the gates they had, and an upgrade
// hint points at the Cloud plan, never at a retired one.
func TestRetiredPlans(t *testing.T) {
	starter, _ := Parse("starter")
	business, _ := Parse(" Business ")
	scale, _ := Parse("scale")
	if starter == nil || business == nil || scale == nil {
		t.Fatal("retired plans must still parse")
	}
	if starter.MaxCampaigns() != 1 || business.MaxCampaigns() != 0 || scale.MaxCampaigns() != 0 {
		t.Fatal("program limits wrong")
	}
	for _, f := range []string{FeatBranding, FeatMLM, FeatUsage, FeatPostbacks, FeatMCP, FeatWidget} {
		if starter.Allows(f) {
			t.Fatalf("starter must not allow %s", f)
		}
	}
	if !business.Allows(FeatMCP) || business.Allows(FeatWidget) {
		t.Fatal("business: MCP yes, widget no")
	}
	if !scale.Allows(FeatWidget) {
		t.Fatal("scale allows widget")
	}
	for _, f := range []string{FeatWidget, FeatMLM} {
		msg := starter.Denied(f)
		if !strings.Contains(msg, "the Cloud plan") || !strings.Contains(msg, "your Starter plan") {
			t.Fatalf("denied message: %q", msg)
		}
		if strings.Contains(msg, "Business") || strings.Contains(msg, "Scale") {
			t.Fatalf("upgrade hint names a retired plan: %q", msg)
		}
	}
}

func TestUnknownPlanFailsLoud(t *testing.T) {
	if _, err := Parse("enterprise"); err == nil {
		t.Fatal("unknown plan must error")
	}
}
