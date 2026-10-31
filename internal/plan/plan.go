// Package plan gates the managed-cloud plan. Self-host (AE_PLAN unset)
// gets every feature; the cloud control plane starts each workspace with
// AE_PLAN=standard, the one Cloud plan, which also includes everything.
// The retired Starter, Business and Scale plans still parse, so a
// workspace that is still on one keeps the gates it signed up with.
// Money correctness (ledger, fraud holds, payouts, refunds) is never
// gated: only the conveniences a plan sells.
package plan

import (
	"fmt"
	"strings"
)

// Features a plan can include.
const (
	FeatMultiProgram = "multi_program" // more than one campaign/program
	FeatBranding     = "branding"      // white-label portal branding
	FeatMLM          = "mlm"           // multi-tier commissions
	FeatUsage        = "usage"         // usage-based commissions
	FeatPostbacks    = "postbacks"     // server-side postbacks
	FeatMCP          = "mcp"           // MCP server for AI assistants
	FeatWidget       = "widget"        // embeddable portal widget
)

// Standard is the one Cloud plan sold today. Everything is on.
const Standard = "standard"

// Names of the plans sold today, in upgrade order. The retired plans are
// not sold, so an upgrade hint never points at them.
var order = []string{Standard}

var all = []string{FeatMultiProgram, FeatBranding, FeatMLM, FeatUsage, FeatPostbacks, FeatMCP, FeatWidget}

var catalog = map[string][]string{
	Standard: all,
	// Retired on 31 October 2026. Kept so an account still on one keeps
	// the gates it had; new signups never get these names.
	"starter":  {},
	"business": {FeatMultiProgram, FeatBranding, FeatMLM, FeatUsage, FeatPostbacks, FeatMCP},
	"scale":    all,
}

// Display names, as the website and the account page show them.
var names = map[string]string{
	Standard:   "Cloud",
	"starter":  "Starter",
	"business": "Business",
	"scale":    "Scale",
}

var labels = map[string]string{
	FeatMultiProgram: "More than one program",
	FeatBranding:     "Portal branding",
	FeatMLM:          "Multi-tier commissions",
	FeatUsage:        "Usage-based commissions",
	FeatPostbacks:    "Server-side postbacks",
	FeatMCP:          "The MCP server",
	FeatWidget:       "The embeddable portal",
}

// Plan is one cloud plan. A nil *Plan is self-host: everything allowed.
type Plan struct {
	Name     string
	features map[string]bool
}

// Parse resolves AE_PLAN. Empty = self-host (nil, no limits). An unknown
// name is an error: a typo must never silently unlock or lock features.
func Parse(name string) (*Plan, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return nil, nil
	}
	feats, ok := catalog[n]
	if !ok {
		return nil, fmt.Errorf("plan: unknown AE_PLAN %q (want %s, or a retired plan: starter, business, scale)", name, Standard)
	}
	p := &Plan{Name: n, features: map[string]bool{}}
	for _, f := range feats {
		p.features[f] = true
	}
	return p, nil
}

// Allows reports whether the plan includes feature f.
func (p *Plan) Allows(f string) bool {
	return p == nil || p.features[f]
}

// MaxCampaigns is the program limit (0 = unlimited).
func (p *Plan) MaxCampaigns() int {
	if p.Allows(FeatMultiProgram) {
		return 0
	}
	return 1
}

// Denied is the message shown when a feature is outside the plan.
func (p *Plan) Denied(f string) string {
	label := labels[f]
	if label == "" {
		label = f
	}
	need := "a higher plan"
	if n := lowestWith(f); n != "" {
		need = "the " + DisplayName(n) + " plan"
	}
	name := "self-host"
	if p != nil {
		name = DisplayName(p.Name)
	}
	return fmt.Sprintf("%s is not in your %s plan. It comes with %s.", label, name, need)
}

// DisplayName is the customer-facing name of a plan key ("standard" is
// shown as "Cloud"). An unknown key is returned as it is.
func DisplayName(key string) string {
	if n, ok := names[key]; ok {
		return n
	}
	return key
}

// lowestWith returns the cheapest plan sold today that includes f ("" if none).
func lowestWith(f string) string {
	for _, n := range order {
		for _, g := range catalog[n] {
			if g == f {
				return n
			}
		}
	}
	return ""
}
