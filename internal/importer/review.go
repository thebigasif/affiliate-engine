package importer

// review.go — the engine's fraud rules, run over a rival's history before
// anything is written. The live rules query our own tables (clicks,
// commissions, customers), which an import never fills with the rival's
// events, so each rule the export can support is evaluated here in memory
// with the live rule's thresholds and alias normalization. Rules the
// export cannot support are listed with the reason, never silently passed.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

// FraudReport is the review's fraud section.
type FraudReport struct {
	Rules    []RuleStatus   `json:"rules"`
	Findings []FraudFinding `json:"findings,omitempty"`
}

// RuleStatus says whether a live rule could run over the export.
type RuleStatus struct {
	Rule string `json:"rule"`
	Ran  bool   `json:"ran"`
	Hits int    `json:"hits"`
	Why  string `json:"why,omitempty"` // why it could not run, or how it ran
}

// FraudFinding is one flagged subject.
type FraudFinding struct {
	Rule    string `json:"rule"`
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

// Live-rule thresholds mirrored here (internal/fraud).
const (
	velocityPerHour  = 30 // ConversionVelocity
	timeAnomalyMin   = 20 // ConversionTimeAnomaly minSample
	fingerprintMinSh = 2  // PaymentFingerprint minShared
)

// noClickData is why the click-level rules cannot run on any rival export.
const noClickData = "needs click-level data (IP, user agent, country, bot class) that %s exports do not include"

// event is one referral or commission row, resolved.
type event struct {
	aff      *affInfo
	custKey  string // billing id, else alias-normalized email
	custMail string
	coupon   string
	at       time.Time
	comm     bool
	status   string
}

func review(source Source, ds Dataset, ix *affIndex, couponOwner map[string]int64) *FraudReport {
	src := string(source)
	refInfo, comInfo := ds.file("referrals"), ds.file("commissions")
	hasCustEmail := refInfo.Has("cust_email") || comInfo.Has("cust_email")
	hasCoupon := refInfo.Has("coupon") || comInfo.Has("coupon")
	hasCustID := refInfo.Has("cust_id") || comInfo.Has("cust_id")

	var evs []event
	timeOfDay := false
	add := func(email, ext, code, custID, custMail, coupon, status string, at time.Time, comm bool) {
		a, ok := ix.resolve(email, ext, code)
		if !ok {
			return
		}
		key := custID
		if key == "" && custMail != "" {
			key = "email:" + fraud.NormalizeEmail(custMail)
		}
		if !at.IsZero() && (at.Hour() != 0 || at.Minute() != 0 || at.Second() != 0) {
			timeOfDay = true
		}
		evs = append(evs, event{aff: a, custKey: key, custMail: custMail, coupon: coupon, at: at, comm: comm, status: status})
	}
	for _, r := range ds.Referrals {
		add(r.AffiliateEmail, r.AffiliateExtID, r.AffiliateCode, r.CustomerID, r.CustomerEmail, r.Coupon, r.Status, r.At, false)
	}
	for _, c := range ds.Commissions {
		add(c.AffiliateEmail, c.AffiliateExtID, c.AffiliateCode, c.CustomerID, c.CustomerEmail, c.Coupon, c.Status, c.At, true)
	}
	var comms []event
	for _, e := range evs {
		if e.comm && !voidStatus(e.status) {
			comms = append(comms, e)
		}
	}

	rep := &FraudReport{}
	type evaluator func() (ran bool, why string, fs []FraudFinding)
	evals := map[string]evaluator{
		"self_referral": func() (bool, string, []FraudFinding) {
			if !hasCustEmail {
				return false, "needs a customer email column in the referrals or commissions export", nil
			}
			hits := map[int64]int{}
			for _, e := range evs {
				if e.custMail != "" && fraud.NormalizeEmail(e.custMail) == fraud.NormalizeEmail(e.aff.email) {
					hits[e.aff.id]++
				}
			}
			var fs []FraudFinding
			for id, n := range hits {
				fs = append(fs, FraudFinding{
					"self_referral", ix.byID[id].email,
					fmt.Sprintf("%d referral/commission rows where the customer email matches the affiliate's (alias-normalized)", n),
				})
			}
			return true, "", fs
		},
		"duplicate_account": func() (bool, string, []FraudFinding) {
			groups := map[string][]string{}
			for _, a := range ix.byID {
				if a.status != "active" {
					continue
				}
				n := fraud.NormalizeEmail(a.email)
				groups[n] = append(groups[n], a.email)
			}
			var fs []FraudFinding
			for n, emails := range groups {
				if len(emails) > 1 {
					sort.Strings(emails)
					fs = append(fs, FraudFinding{
						"duplicate_account", n,
						fmt.Sprintf("%d active affiliates share one alias-normalized email: %s", len(emails), strings.Join(emails, ", ")),
					})
				}
			}
			return true, "affiliates file plus affiliates already here", fs
		},
		"payment_fingerprint": func() (bool, string, []FraudFinding) {
			if !hasCustEmail {
				return false, "needs a customer email column in the referrals or commissions export", nil
			}
			affsOf := map[string]map[int64]bool{}
			mine := map[int64]map[string]bool{}
			for _, e := range evs {
				if e.custMail == "" {
					continue
				}
				n := fraud.NormalizeEmail(e.custMail)
				if affsOf[n] == nil {
					affsOf[n] = map[int64]bool{}
				}
				affsOf[n][e.aff.id] = true
				if mine[e.aff.id] == nil {
					mine[e.aff.id] = map[string]bool{}
				}
				mine[e.aff.id][n] = true
			}
			var fs []FraudFinding
			for id, emails := range mine {
				shared := 0
				for n := range emails {
					if len(affsOf[n]) > 1 {
						shared++
					}
				}
				if shared >= fingerprintMinSh {
					fs = append(fs, FraudFinding{
						"payment_fingerprint", ix.byID[id].email,
						fmt.Sprintf("%d customer emails also bought through other affiliates", shared),
					})
				}
			}
			return true, "on customer emails across referrals and commissions", fs
		},
		"conversion_velocity": func() (bool, string, []FraudFinding) {
			if len(comms) == 0 {
				return false, "needs the commissions export", nil
			}
			if !timeOfDay {
				return false, "commission dates carry no time of day", nil
			}
			var fs []FraudFinding
			for id, ts := range timesByAff(comms) {
				if n, at := maxInWindow(ts, time.Hour); n >= velocityPerHour {
					fs = append(fs, FraudFinding{
						"conversion_velocity", ix.byID[id].email,
						fmt.Sprintf("%d commissions within one hour (from %s)", n, at.Format(time.RFC3339)),
					})
				}
			}
			return true, "", fs
		},
		"conversion_time_anomaly": func() (bool, string, []FraudFinding) {
			if len(comms) == 0 {
				return false, "needs the commissions export", nil
			}
			if !timeOfDay {
				return false, "commission dates carry no time of day", nil
			}
			var pNight, pTotal int
			per := timesByAff(comms)
			for _, ts := range per {
				for _, t := range ts {
					pTotal++
					if t.Hour() < 6 {
						pNight++
					}
				}
			}
			why := "over the whole export (the live rule uses 7 days), hours in UTC"
			if pTotal < timeAnomalyMin {
				return true, why + "; too few commissions for a program baseline", nil
			}
			pShare := float64(pNight) / float64(pTotal)
			var fs []FraudFinding
			for id, ts := range per {
				if len(ts) < timeAnomalyMin {
					continue
				}
				night := 0
				for _, t := range ts {
					if t.Hour() < 6 {
						night++
					}
				}
				share := float64(night) / float64(len(ts))
				z := (share - pShare) / math.Sqrt(pShare*(1-pShare)/float64(len(ts))+1e-9)
				if z > 3 && share > 0.5 {
					fs = append(fs, FraudFinding{
						"conversion_time_anomaly", ix.byID[id].email,
						fmt.Sprintf("%.0f%% of %d commissions 00:00-05:59 vs program %.0f%% (z=%.1f)", share*100, len(ts), pShare*100, z),
					})
				}
			}
			return true, why, fs
		},
		"coupon_hijack": func() (bool, string, []FraudFinding) {
			if !hasCoupon {
				return false, "needs a coupon column in the referrals or commissions export", nil
			}
			if !hasCustID && !hasCustEmail {
				return false, "needs a customer id or customer email to know who referred the customer first", nil
			}
			ordered := append([]event(nil), evs...)
			sort.SliceStable(ordered, func(i, j int) bool {
				if ordered[i].at.IsZero() || ordered[j].at.IsZero() {
					return false
				}
				return ordered[i].at.Before(ordered[j].at)
			})
			first := map[string]int64{}
			overrides := map[string][]time.Time{}
			dated := true
			for _, e := range ordered {
				if e.custKey == "" {
					continue
				}
				b, seen := first[e.custKey]
				if !seen {
					first[e.custKey] = e.aff.id
					continue
				}
				owner, ok := couponOwner[e.coupon]
				if e.coupon == "" || !ok || owner != e.aff.id || b == e.aff.id {
					continue
				}
				if e.at.IsZero() {
					dated = false
				}
				overrides[e.coupon] = append(overrides[e.coupon], e.at)
			}
			why := ""
			var fs []FraudFinding
			for c, ts := range overrides {
				n, window := len(ts), "in the export"
				if dated {
					n, _ = maxInWindow(ts, 7*24*time.Hour)
					window = "within 7 days"
				} else {
					why = "some rows have no date: overrides counted over the whole export"
				}
				if n >= fraud.DefaultCouponHijackThreshold {
					fs = append(fs, FraudFinding{
						"coupon_hijack", c,
						fmt.Sprintf("coupon %q took %d customers first referred by other affiliates %s", c, n, window),
					})
				}
			}
			return true, why, fs
		},
		"inactive_affiliate": func() (bool, string, []FraudFinding) {
			if len(comms) == 0 {
				return false, "needs the commissions export", nil
			}
			hits := map[int64]int{}
			for _, e := range comms {
				if e.aff.status != "active" {
					hits[e.aff.id]++
				}
			}
			var fs []FraudFinding
			for id, n := range hits {
				fs = append(fs, FraudFinding{
					"inactive_affiliate", ix.byID[id].email,
					fmt.Sprintf("%d commissions for an affiliate who is blocked or not approved in %s (imported frozen)", n, src),
				})
			}
			return true, "", fs
		},
	}

	for _, r := range fraud.NewEngine(nil).Rules() {
		name := r.Name()
		ev, ok := evals[name]
		if !ok {
			rep.Rules = append(rep.Rules, RuleStatus{Rule: name, Why: fmt.Sprintf(noClickData, src)})
			continue
		}
		ran, why, fs := ev()
		sort.Slice(fs, func(i, j int) bool { return fs[i].Subject < fs[j].Subject })
		rep.Rules = append(rep.Rules, RuleStatus{Rule: name, Ran: ran, Hits: len(fs), Why: why})
		rep.Findings = append(rep.Findings, fs...)
	}
	return rep
}

func timesByAff(evs []event) map[int64][]time.Time {
	out := map[int64][]time.Time{}
	for _, e := range evs {
		if !e.at.IsZero() {
			out[e.aff.id] = append(out[e.aff.id], e.at.UTC())
		}
	}
	return out
}

// maxInWindow returns the most events inside any window of length w, and
// where that window starts.
func maxInWindow(ts []time.Time, w time.Duration) (int, time.Time) {
	s := append([]time.Time(nil), ts...)
	sort.Slice(s, func(i, j int) bool { return s[i].Before(s[j]) })
	best, at, j := 0, time.Time{}, 0
	for i := range s {
		for s[i].Sub(s[j]) >= w {
			j++
		}
		if n := i - j + 1; n > best {
			best, at = n, s[j]
		}
	}
	return best, at
}
