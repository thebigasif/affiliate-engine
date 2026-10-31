// demoseed.go — `demo-seed`: the sample program behind the public live
// demo (demo.go). Northwind Notes, a fictional note-taking SaaS, runs a
// 90-day-old affiliate program: 40 affiliates, clicks with utm/subid
// variety, purchases and monthly renewals, refunds with clawbacks, a paid
// payout run and a pending one, badges, and one fraud story per rule the
// engine can fire, so the fraud queue shows each rule's own reason.
//
// Money goes through the real code paths: clicks through the click
// committer (clicks, aggregates, attributions), conversions through
// screening -> commissions engine -> ledger poster (every verdict and hold
// is the fraud engine's own), then maturation, refunds and payouts through
// their engines on a replayed clock. Only rows the admin API itself writes
// with plain INSERTs (campaigns, rules, codes, branding, tax documents,
// applications) are inserted directly. The generator has a fixed seed:
// every run builds the same program, only the dates follow "now".
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fingerprint"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/gamify"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/mlm"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
	"github.com/thebigasif/affiliate-engine/internal/screening"
)

const (
	demoSeedValue = 20260929 // fixed: every seed builds the same program
	demoDays      = 90       // history length, ending at 00:00 UTC today
)

// runDemoSeed is the `demo-seed [-reset]` subcommand.
func runDemoSeed(dsn string, args []string) error {
	fs := flag.NewFlagSet("demo-seed", flag.ContinueOnError)
	reset := fs.Bool("reset", false, "wipe the demo database (and its -wal/-shm) first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Never the default database: seeding (let alone -reset) must be aimed.
	if strings.TrimPrefix(dsn, "sqlite:") == "" {
		return errors.New("demo-seed: pass -db sqlite:<path> for the demo database")
	}
	path := sqlitePath(dsn)
	ctx := context.Background()
	st, err := inspectDemoDB(ctx, path)
	if err != nil {
		return err
	}
	if !st.empty {
		if !*reset {
			return fmt.Errorf("demo-seed: %s is not empty; pass -reset to wipe it and seed again", path)
		}
		// -reset only ever wipes what demo-seed created: pointed at a real
		// install by mistake it refuses instead of deleting the program.
		if !st.marked {
			return fmt.Errorf("demo-seed: %s holds data demo-seed did not create; refusing to wipe it", path)
		}
	}
	if *reset && st.exists {
		if err := wipeSQLite(path); err != nil {
			return err
		}
	}
	res, err := seedDemo(ctx, path, time.Now().UTC())
	if err != nil {
		return err
	}
	c := res.counts
	fmt.Printf("demo-seed: %s — %d affiliates (%d frozen, %d applications pending), %d clicks, %d commissions (%d refunds clawed back), %d held for fraud review by %d rules, %d payouts paid, %d awaiting approval\n",
		path, c["affiliates"], c["affiliates_frozen"], c["applications_pending"], c["clicks"],
		c["commissions"], c["clawbacks"], c["holds_open"], c["rules_fired"], c["payouts_paid"], c["payouts_requested"])
	fmt.Println("serve it read-only with AE_DEMO=1 (DEPLOY.md, Live demo)")
	return nil
}

// demoSeedResult is what a finished seed reports.
type demoSeedResult struct {
	seededAt time.Time
	counts   demoCounts
}

// seedDemo builds the program into an empty database at path, with the
// history ending at 00:00 UTC of now's day, then runs the doctor checks:
// a seed that leaves the ledger inconsistent fails loudly.
func seedDemo(ctx context.Context, path string, now time.Time) (demoSeedResult, error) {
	var res demoSeedResult
	// The demo database is regenerated daily: per-post fsync (synchronous
	// FULL on the ledger conn) would only slow the replay down.
	h, err := db.Open(ctx, "sqlite:"+path, db.Options{})
	if err != nil {
		return res, err
	}
	defer closeHandles(h)
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		return res, err
	}
	empty, err := dbIsEmpty(ctx, h.DB)
	if err != nil {
		return res, err
	}
	if !empty {
		return res, fmt.Errorf("demo-seed: %s is not empty", path)
	}
	end := now.UTC().Truncate(24 * time.Hour)
	// The marker goes in first (seeded_at empty): a seed that dies half way
	// still reads as a demo database, which the next start may wipe.
	if _, err := h.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS demo_seed (
			id         INTEGER PRIMARY KEY CHECK (id = 1),
			anchor     TEXT NOT NULL,
			started_at TEXT NOT NULL,
			seeded_at  TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		return res, err
	}
	if _, err := h.DB.ExecContext(ctx, `INSERT INTO demo_seed (id, anchor, started_at) VALUES (1, ?, ?)`,
		end.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)); err != nil {
		return res, err
	}

	// payment_fingerprint looks commissions up by buyer email on every
	// checkout; unindexed, the replay spends a third of its time scanning.
	if _, err := h.DB.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS ix_demo_commissions_buyer ON commissions(buyer_email)`); err != nil {
		return res, err
	}

	s := newDemoSeeder(ctx, h, end)
	if err := s.run(); err != nil {
		return res, err
	}

	ok, err := doctor(ctx, h, io.Discard)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, errors.New("demo-seed: doctor checks failed on the seeded database")
	}
	res.seededAt = time.Now().UTC().Truncate(time.Second)
	if _, err := h.DB.ExecContext(ctx, `UPDATE demo_seed SET seeded_at = ? WHERE id = 1`,
		res.seededAt.Format(time.RFC3339)); err != nil {
		return res, err
	}
	res.counts, err = countDemo(ctx, h.DB)
	return res, err
}

// --- the program ---

type demoProduct struct {
	key      string
	campaign int64
	price    int64 // minor units
	monthly  bool
}

var (
	demoMonthly = demoProduct{"pro-monthly", 1, 1200, true}
	demoAnnual  = demoProduct{"pro-annual", 1, 9600, false}
	demoTeams   = demoProduct{"teams-annual", 2, 48000, false}
)

// demoWeighted is one weighted choice, parsed from "US:6 CA:2" specs.
type demoWeighted struct {
	v string
	w float64
}

func parseWeights(spec string) []demoWeighted {
	var out []demoWeighted
	for _, f := range strings.Fields(spec) {
		v, w, _ := strings.Cut(f, ":")
		n, err := strconv.ParseFloat(w, 64)
		if err != nil || n <= 0 {
			n = 1
		}
		out = append(out, demoWeighted{v, n})
	}
	return out
}

// demoAff is one affiliate: roster fields, then runtime state.
type demoAff struct {
	name, email, code string
	join              int     // day the affiliate was approved
	sales             float64 // share of organic purchases
	traffic           float64 // share of organic clicks
	until             int     // organic purchases stop on this day (0 = never): a story's window stays clean
	geo, sources      string  // weighted specs
	subids            string
	coupon            string
	couponShare       float64 // share of organic purchases made with the coupon
	override          int64   // campaign-1 rate override (bp)
	tax               string  // "w9"/"w8ben" validated; "w9-pending" blocks payouts
	note              string  // application note ("" = added by the program admin)
	story             bool    // fraud-story account (never a coupon-hijack victim)

	id        int64
	ipB       int // second IPv4 octet: this affiliate's visitors are nobody else's
	geoW      []demoWeighted
	srcW      []demoWeighted
	subW      []demoWeighted
	clicks    []demoClick // committed, oldest first (known bots left out)
	recentIPs []string
}

type demoClick struct {
	id, ip, country, tag string
	at                   time.Time
	suspect, dc          bool
}

type demoCustomer struct {
	ext, email, country string
	aff                 *demoAff // bound affiliate (first touch)
	plan                demoProduct
	active              bool // subscription alive (monthly renewals continue)
}

// demoRoster is the cast. Emails use reserved example domains (RFC 2606).
func demoRoster() []*demoAff {
	return []*demoAff{
		// Launch partners (added by the program admin).
		{name: "Maya Chen", email: "maya.chen@example.com", code: "maya", join: 0, sales: .15, traffic: .12, geo: "US:6 CA:2 GB:2", sources: "youtube:5 blog:3 instagram:1", subids: "desk-tour:3 sync-review:2 pinned-comment:1", override: 3500, tax: "w9"},
		{name: "Deep Work Weekly", email: "sam@deepworkweekly.example", code: "deepwork", join: 0, sales: .12, traffic: .09, geo: "US:5 GB:3 DE:2", sources: "newsletter:1", subids: "issue-41:1 issue-42:1 issue-43:1 issue-44:1 sponsor-slot:2", tax: "w9"},
		{name: "Studio Ferro", email: "luca@studioferro.example", code: "ferro", join: 1, sales: .07, traffic: .15, geo: "IT:3 US:4 DE:3", sources: "youtube:6 tiktok:2", subids: "tutorial-notes:3 shorts:2 description:2", tax: "w8ben"},
		{name: "Priya Raman", email: "priya@priyaraman.example", code: "priya", join: 1, sales: .06, traffic: .05, geo: "IN:5 US:3 GB:2", sources: "instagram:4 tiktok:3", subids: "bio-link:3 story-swipe:2", coupon: "PRIYA15", couponShare: .45, tax: "w8ben"},
		{name: "The Paper Desk", email: "hannah@thepaperdesk.example", code: "paperdesk", join: 2, sales: .05, traffic: .05, geo: "DE:7 NL:3", sources: "blog:4 pinterest:2", subids: "top10-apps:3 sidebar:1", tax: "w8ben"},
		{name: "Remote Tools Hub", email: "ben.carter@example.com", code: "remotetoolshub", join: 2, sales: .045, traffic: .04, geo: "US:7 CA:3", sources: "blog:3 google:2", subids: "comparison-table:2 footer:1", tax: "w9"},
		{name: "Study Smarter", email: "aisha@studysmarter.example", code: "studysmarter", join: 3, sales: .04, traffic: .05, geo: "GB:5 NG:3 US:2", sources: "youtube:3 tiktok:2", subids: "exam-season:2 desk-setup:1"},
		{name: "Minimal Setup", email: "kenji@minimalsetup.example", code: "minimalsetup", join: 3, sales: .035, traffic: .03, geo: "JP:6 US:4", sources: "blog:3 twitter:2", subids: "setup-2026:2"},
		{name: "Oliver Grant", email: "oliver.grant@example.com", code: "olivergrant", join: 4, sales: .04, traffic: .035, geo: "US:1", sources: "linkedin:3 newsletter:2", subids: "post-link:2", tax: "w9-pending"},
		{name: "Cahier Numérique", email: "chloe@cahiernumerique.example", code: "cahier", join: 5, sales: .03, traffic: .03, geo: "FR:8 BE:2", sources: "blog:3 youtube:2", subids: "guide-2026:2"},
		// Approved from the portal application form.
		{name: "Notas Claras", email: "tomas@notasclaras.example", code: "notasclaras", join: 6, sales: .04, traffic: .035, geo: "ES:6 MX:3 US:1", sources: "youtube:3 blog:2", subids: "review:2", note: "Spanish productivity channel, 60k subscribers"},
		{name: "Nora Lindqvist", email: "nora.lindqvist@example.com", code: "nora", join: 8, sales: .025, traffic: .025, geo: "SE:7 NO:3", sources: "instagram:2 blog:2", note: "Bullet-journal blog, mostly Nordic readers"},
		{name: "Lena Ortiz Reviews", email: "lena@lenaortiz.example", code: "lenaortiz", join: 10, sales: .05, traffic: .045, until: 80, geo: "US:7 CA:3", sources: "blog:4 pinterest:1", subids: "best-note-apps:3 sidebar:1", tax: "w9", story: true, note: "Software review blog"},
		{name: "Founder Stack", email: "marcus@founderstack.example", code: "founderstack", join: 12, sales: .03, traffic: .025, geo: "US:6 GB:2 AU:2", sources: "podcast:4 newsletter:1", subids: "ep-112:2 ep-118:2 show-notes:1", coupon: "FOUNDER10", couponShare: .3, tax: "w9", note: "Podcast for early-stage founders"},
		{name: "Ethan Brooks", email: "ethan.brooks@example.com", code: "ethanb", join: 15, sales: .02, traffic: .02, geo: "US:8 CA:2", sources: "twitter:3 blog:1", note: "Writes about PKM on X"},
		{name: "Isabel Duarte", email: "isabel.duarte@example.com", code: "isabel", join: 18, sales: .02, traffic: .02, geo: "BR:7 PT:3", sources: "youtube:3 instagram:2", note: "Study vlogs in Portuguese"},
		{name: "Dana Whitfield", email: "dana.whitfield@example.org", code: "danaw", join: 20, sales: .02, traffic: .02, geo: "US:8 GB:2", sources: "blog:3 pinterest:1", story: true, note: "Home-office blog"},
		{name: "SaaS for Students", email: "rahul@saasforstudents.example", code: "saasforstudents", join: 22, sales: .025, traffic: .03, geo: "IN:6 US:2 GB:2", sources: "youtube:3 reddit:2", note: "Student discounts roundup"},
		{name: "Grace Owens", email: "grace.owens@example.com", code: "graceo", join: 25, sales: .015, traffic: .015, geo: "AU:6 NZ:4", sources: "instagram:3", note: "Planner and stationery Instagram"},
		{name: "Felix Wagner", email: "felix.wagner@example.com", code: "felixw", join: 28, sales: .02, traffic: .02, geo: "DE:6 AT:2 CH:2", sources: "blog:2 linkedin:2", note: "Consultant, writes about team docs"},
		{name: "BurstLink Media", email: "team@burstlink.example", code: "burstlink", join: 30, sales: .015, traffic: .02, geo: "US:7 CA:3", sources: "reddit:3 twitter:2", story: true, note: "Social media agency"},
		{name: "Mia Novak", email: "mia.novak@example.com", code: "mianovak", join: 35, sales: .015, traffic: .015, geo: "CZ:5 DE:3 US:2", sources: "blog:2 youtube:1", note: "Notion-alternative comparisons"},
		{name: "Jordan Miles", email: "jordan.miles@example.com", code: "jmiles", join: 40, sales: .02, traffic: .02, until: 78, geo: "US:9 CA:1", sources: "twitter:3 blog:1", story: true, note: "Tech Twitter, 25k followers"},
		{name: "Owen Hughes", email: "owen.hughes@example.com", code: "owenh", join: 42, sales: .01, traffic: .012, geo: "GB:8 IE:2", sources: "linkedin:3", note: "Ops lead, shares tooling posts"},
		{name: "QuickCash Rewards", email: "partners@quickcash.example", code: "quickcash", join: 45, sales: .03, traffic: .04, geo: "US:6 GB:2 CA:2", sources: "cashback:5", subids: "offer-wall:3", story: true, note: "Cashback and rewards site"},
		{name: "Zoe Adler", email: "zoe.adler@example.com", code: "zoea", join: 48, sales: .015, traffic: .015, geo: "US:5 DE:5", sources: "tiktok:3 instagram:1", note: "Productivity TikTok"},
		{name: "DealDrop Coupons", email: "deals@dealdrop.example", code: "dealdrop", join: 50, traffic: .004, geo: "US:1", sources: "coupons:1", coupon: "DROP40", story: true, note: "Coupon and deals aggregator"},
		{name: "TopTools Weekly", email: "hello@toptoolsweekly.example", code: "toptools", join: 52, sales: .015, traffic: .015, until: 76, geo: "US:6 GB:4", sources: "newsletter:3", story: true, note: "Weekly tools newsletter"},
		{name: "SaaS Stack Picks", email: "picks@saasstack.example", code: "saasstack", join: 56, sales: .012, traffic: .012, until: 76, geo: "US:7 CA:3", sources: "blog:3 google:1", story: true, note: "SaaS comparison site"},
		{name: "Growth Loop Media", email: "ops@growthloop.example", code: "growthloop", join: 58, geo: "US:1", sources: "facebook:3 google:1", story: true, note: "Performance marketing agency"},
		{name: "Promo Nest", email: "team@promonest.example", code: "promonest", join: 59, geo: "US:1", sources: "facebook:2 google:2", story: true, note: "Deals community"},
		{name: "Liam Foster", email: "liam.foster@example.com", code: "liamf", join: 62, sales: .01, traffic: .012, geo: "GB:6 US:4", sources: "youtube:2", note: "Desk setup videos"},
		{name: "Ivy Tran", email: "ivy.tran@example.com", code: "ivyt", join: 66, sales: .008, traffic: .01, geo: "US:6 AU:4", sources: "tiktok:3", note: "Study-with-me TikTok"},
		{name: "NightOwl Deals", email: "deals@nightowl.example", code: "nightowl", join: 70, geo: "US:6 GB:4", sources: "display:3", story: true, note: "Deal alerts"},
		{name: "ViralBoost Ads", email: "media@viralboost.example", code: "viralboost", join: 72, geo: "US:7 CA:3", sources: "native-ads:3", story: true, note: "Native ads network"},
		{name: "ClickFlow Traffic", email: "buy@clickflow.example", code: "clickflow", join: 74, geo: "US:1", sources: "display:2", story: true, note: "Media buying"},
		{name: "J. Miles Deals", email: "jordan.miles+deals@example.com", code: "jmilesdeals", join: 78, geo: "US:9 CA:1", sources: "twitter:1", story: true, note: "Deals account"},
		{name: "Hugo Lambert", email: "hugo.lambert@example.com", code: "hugol", join: 80, sales: .006, traffic: .008, geo: "FR:6 BE:4", sources: "youtube:2", note: "French tech YouTube"},
		{name: "Sofia Ricci", email: "sofia.ricci@example.com", code: "sofiar", join: 84, traffic: .004, geo: "IT:1", sources: "instagram:1", note: "Design student, Instagram"},
		{name: "Noah Kim", email: "noah.kim@example.com", code: "noahk", join: 87, geo: "US:1", sources: "blog:1", note: "Just starting a productivity blog"},
	}
}

// demoApplications are portal applications still in review (or turned
// down), inserted on their day.
var demoApplications = []struct {
	day                int
	email, name, note  string
	status             string
	reviewedAfterHours int
}{
	{61, "cheapclicks247@example.net", "CheapClicks 24/7", "Incentivized traffic, 1M clicks/day guaranteed", "rejected", 20},
	{86, "ravi.kapoor@example.net", "Ravi Kapoor", "Study-with-me streams, 40k followers", "pending", 0},
	{87, "tessa@freelancefolio.example", "Freelance Folio", "Newsletter for freelance designers (9k readers)", "pending", 0},
	{88, "omar@couponcrate.example", "Coupon Crate", "Coupon and deals site", "pending", 0},
	{89, "grace.liu@example.com", "Grace Liu", "Weekly productivity podcast", "pending", 0},
}

var (
	demoFirst = []string{"Alex", "Jamie", "Taylor", "Morgan", "Riley", "Casey", "Avery", "Quinn", "Harper", "Rowan", "Elena", "Marco", "Yuki", "Amara", "Lucas", "Sara", "Omar", "Ines", "Noor", "Felipe", "Hana", "Leo", "Clara", "Ivan", "Mei", "Arjun", "Sofia", "Tom", "Lina", "Emil"}
	demoLast  = []string{"Walker", "Nguyen", "Fischer", "Rossi", "Kowalski", "Silva", "Tanaka", "Okafor", "Dubois", "Larsen", "Patel", "Moreno", "Schmidt", "Brennan", "Haddad", "Novak", "Ito", "Costa", "Weber", "Kaur"}
	demoTLDs  = []string{"com", "net", "org"}

	demoUAs = []string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:130.0) Gecko/20100101 Firefox/130.0",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Mobile Safari/537.36",
	}
	demoBotUAs = []string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"curl/8.5.0",
		"python-requests/2.32.3",
	}
	// First octets outside every fingerprint datacenter range.
	demoHomeOctets = []int{24, 67, 71, 73, 76, 81, 86, 92, 98, 101, 109, 176, 188, 212}
	demoReferers   = map[string]string{
		"youtube": "https://www.youtube.com/", "tiktok": "https://www.tiktok.com/",
		"instagram": "https://l.instagram.com/", "twitter": "https://t.co/",
		"reddit": "https://www.reddit.com/", "linkedin": "https://www.linkedin.com/",
		"pinterest": "https://www.pinterest.com/", "google": "https://www.google.com/",
		"facebook": "https://l.facebook.com/",
	}
	demoLandings = parseWeights("/:3 /pricing:3 /features/offline-sync:1 /templates:2 /teams:1")
	// UTC hour weights of a mostly US/EU audience (night share ~8%).
	demoHours = [24]float64{3, 2, 1.5, 1, 1, 1, 1.5, 2.5, 4, 5, 6, 6.5, 7, 7.5, 8, 8.5, 8.5, 8, 7.5, 7, 6.5, 6, 5, 4}
	// Weekly rhythm by day index (not weekday: counts must not depend on
	// which date the demo is seeded).
	demoWeek = [7]float64{1.0, 1.08, 1.1, 1.05, 0.95, 0.78, 0.82}
)

// --- the replay ---

type demoEvent struct {
	at    time.Time
	seq   int
	click *demoClickSpec // a click to enqueue
	run   func() error   // anything else
}

type demoClickSpec struct {
	aff                    *demoAff
	at                     time.Time
	ip, ua, country, tag   string
	source, subid, landing string
	placement, referer     string
	accept                 bool // Accept + Accept-Language sent (false = a suspected bot)
}

type demoSeeder struct {
	ctx        context.Context
	h          *db.Handles
	rng        *rand.Rand
	start, end time.Time
	now        time.Time // the replayed clock
	day        int       // the day being replayed
	replaying  bool

	poster    *ledger.Poster
	engine    *commissions.Engine
	screened  *screening.Engine
	committer *clicks.Committer
	runner    *payouts.Runner

	affs      []*demoAff
	byCode    map[string]*demoAff
	customers []*demoCustomer
	pending   [demoDays][]demoEvent
	lastWG    *sync.WaitGroup
	seq       int
	extSeq    int
	custSeq   int
	organicN  int
	victims   map[*demoAff]bool

	refundPaid, refundApproved bool
}

func newDemoSeeder(ctx context.Context, h *db.Handles, end time.Time) *demoSeeder {
	s := &demoSeeder{
		ctx: ctx, h: h,
		rng:     rand.New(rand.NewPCG(demoSeedValue, 0)), // #nosec G404 -- demo data: reproducible on purpose, never a secret
		start:   end.AddDate(0, 0, -demoDays),
		end:     end,
		byCode:  map[string]*demoAff{},
		victims: map[*demoAff]bool{},
	}
	s.now = s.start
	s.poster = ledger.NewPoster(h.Ledger)
	s.engine = commissions.NewEngine(h.DB, s.poster)
	s.engine.SetClock(func() time.Time { return s.now })
	s.engine.MLMBV = mlm.New(h.DB).RecordBV
	s.screened = screening.New(s.engine, fraud.NewEngine(h.DB), false)
	s.runner = payouts.NewRunner(h.DB)
	for i, a := range demoRoster() {
		a.ipB = 11 + i
		a.geoW, a.srcW, a.subW = parseWeights(a.geo), parseWeights(a.sources), parseWeights(a.subids)
		s.affs = append(s.affs, a)
		s.byCode[a.code] = a
	}
	return s
}

func (s *demoSeeder) run() error {
	ctx := s.ctx
	// Settle payouts on the replayed clock (main wires the same journal).
	prev, prevLedger := payouts.LedgerPoster, payouts.Ledger
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := s.poster.Post(ctx, payouts.SettleKey(payoutID), "payout", "", s.now,
			payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = s.poster
	defer func() { payouts.LedgerPoster, payouts.Ledger = prev, prevLedger }()

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.committer = clicks.NewCommitter(cctx, s.h.DB, 500, 2*time.Millisecond)
	s.committer.FullWait = -1 // offline loader: backpressure, never drop
	committerOpen := true
	defer func() {
		if committerOpen {
			s.committer.Close()
		}
	}()

	if err := s.setup(); err != nil {
		return err
	}
	s.planStories()

	s.replaying = true
	for d := 0; d < demoDays; d++ {
		s.day = d
		dayStart := s.start.AddDate(0, 0, d)
		s.now = dayStart
		if err := s.join(d, dayStart); err != nil {
			return err
		}
		if err := s.mature(); err != nil {
			return err
		}
		events := append(s.pending[d], s.organicDay(d, dayStart)...)
		s.pending[d] = nil
		slices.SortStableFunc(events, func(a, b demoEvent) int {
			if c := a.at.Compare(b.at); c != 0 {
				return c
			}
			return a.seq - b.seq
		})
		for _, ev := range events {
			if ev.click != nil {
				if err := s.enqueue(*ev.click); err != nil {
					return err
				}
				continue
			}
			s.flushClicks()
			s.now = ev.at
			if err := ev.run(); err != nil {
				return err
			}
		}
		s.flushClicks()
	}
	s.committer.Close()
	committerOpen = false
	s.now = s.end
	if err := s.mature(); err != nil {
		return err
	}
	return s.finish()
}

// setup writes the program the admin configured before launch.
func (s *demoSeeder) setup() error {
	at := s.ts(s.start.AddDate(0, 0, -1).Add(10 * time.Hour))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO campaigns (id, name, status, created_at, attribution_window_days, net_terms_days)
		  VALUES (1, 'Northwind Notes Partner Program', 'active', ?, 30, 30),
		         (2, 'Northwind Teams', 'active', ?, 30, 30)`, []any{at, at}},
		// 25% recurring by default, 30% on annual plans, 30% once an
		// affiliate has referred $1,500; Teams pays a $75 bounty.
		{`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value, priority, product_key)
		  VALUES (1, NULL, 'purchase', 'percent', 2500, 0, NULL),
		         (1, NULL, 'purchase', 'percent', 3000, 5, 'pro-annual'),
		         (2, NULL, 'purchase', 'fixed', 7500, 0, NULL)`, nil},
		{`INSERT INTO rate_tiers (campaign_id, product_key, threshold_minor, rate_type, rate_value, created_at)
		  VALUES (1, NULL, 150000, 'percent', 3000, ?)`, []any{at}},
		{`INSERT INTO program_branding (campaign_id, brand_name, accent_color, updated_at)
		  VALUES (1, 'Northwind Notes Partners', '#0f766e', ?), (2, 'Northwind Teams Partners', '#0f766e', ?)`, []any{at, at}},
	} {
		if _, err := s.h.DB.ExecContext(s.ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("demo-seed: setup: %w", err)
		}
	}
	if err := gamify.New(s.h.DB).SeedBadges(s.ctx); err != nil {
		return err
	}
	for _, app := range demoApplications {
		s.schedule(s.dayAt(app.day, 9, 30), func() error {
			var reviewed any
			if app.reviewedAfterHours > 0 {
				reviewed = s.ts(s.now.Add(time.Duration(app.reviewedAfterHours) * time.Hour))
			}
			_, err := s.h.DB.ExecContext(s.ctx, `
				INSERT INTO affiliate_applications (email, name, note, status, created_at, reviewed_at)
				VALUES (?, ?, ?, ?, ?, ?)`, app.email, app.name, app.note, app.status, s.ts(s.now), reviewed)
			return err
		})
	}
	// Oliver's W-9 was requested and never came back: his approved balance
	// stays out of both payout runs.
	s.schedule(s.dayAt(55, 12, 0), func() error {
		_, err := s.h.DB.ExecContext(s.ctx, `
			INSERT INTO tax_documents (affiliate_id, doc_type, status, note, created_at)
			VALUES (?, 'w9', 'pending', 'requested by email', ?)`, s.byCode["olivergrant"].id, s.ts(s.now))
		return err
	})
	s.schedule(s.dayAt(68, 10, 0), func() error { return s.payoutRun(true) })
	s.schedule(s.dayAt(88, 9, 0), func() error { return s.payoutRun(false) })
	return nil
}

// join inserts the affiliates approved on day d (the portal review writes
// the same rows) with their code, coupon, rate override and tax document.
func (s *demoSeeder) join(d int, dayStart time.Time) error {
	for _, a := range s.affs {
		if a.join != d {
			continue
		}
		approved := dayStart.Add(9 * time.Hour)
		if err := s.h.DB.QueryRowContext(s.ctx, `
			INSERT INTO affiliates (name, email, status, created_at) VALUES (?, ?, 'active', ?) RETURNING id`,
			a.name, a.email, s.ts(approved)).Scan(&a.id); err != nil {
			return fmt.Errorf("demo-seed: affiliate %s: %w", a.code, err)
		}
		if _, err := s.h.DB.ExecContext(s.ctx, `
			INSERT INTO affiliate_codes (affiliate_id, code, is_primary, created_at) VALUES (?, ?, TRUE, ?)`,
			a.id, a.code, s.ts(approved)); err != nil {
			return err
		}
		if a.note != "" {
			if _, err := s.h.DB.ExecContext(s.ctx, `
				INSERT INTO affiliate_applications (email, name, note, status, created_at, reviewed_at)
				VALUES (?, ?, ?, 'approved', ?, ?)`,
				a.email, a.name, a.note, s.ts(approved.Add(-40*time.Hour)), s.ts(approved)); err != nil {
				return err
			}
		}
		if a.coupon != "" {
			if _, err := s.h.DB.ExecContext(s.ctx, `
				INSERT INTO coupons (code, affiliate_id, campaign_id, rate_type, rate_value, active, created_at)
				VALUES (?, ?, 1, 'inherit', 0, 1, ?)`, a.coupon, a.id, s.ts(approved)); err != nil {
				return err
			}
		}
		if a.override > 0 {
			if _, err := s.h.DB.ExecContext(s.ctx, `
				INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value, priority)
				VALUES (1, ?, 'purchase', 'percent', ?, 10)`, a.id, a.override); err != nil {
				return err
			}
		}
		if a.tax == "w9" || a.tax == "w8ben" {
			if _, err := s.h.DB.ExecContext(s.ctx, `
				INSERT INTO tax_documents (affiliate_id, doc_type, status, reference, created_at, reviewed_at)
				VALUES (?, ?, 'validated', ?, ?, ?)`,
				a.id, a.tax, fmt.Sprintf("form-%04d", a.id), s.ts(approved), s.ts(approved.Add(26*time.Hour))); err != nil {
				return err
			}
		}
	}
	return nil
}

// organicDay draws day d's ordinary business: new purchases spread over
// the affiliates by sales share, and clicks by traffic share.
func (s *demoSeeder) organicDay(d int, dayStart time.Time) []demoEvent {
	lam := (3.5 + 5.5*float64(d)/float64(demoDays-1)) * demoWeek[d%7]
	var sellers, sources []*demoAff
	for _, a := range s.affs {
		if d <= a.join {
			continue
		}
		if a.sales > 0 && (a.until == 0 || d < a.until) {
			sellers = append(sellers, a)
		}
		if a.traffic > 0 {
			sources = append(sources, a)
		}
	}
	var out []demoEvent
	if len(sellers) > 0 {
		for range poisson(s.rng, lam) {
			a := pickAff(s.rng, sellers, func(a *demoAff) float64 { return a.sales })
			out = append(out, s.event(s.diurnal(dayStart), func() error { return s.organicPurchase(a) }))
		}
	}
	if len(sources) > 0 {
		for range poisson(s.rng, lam*32) { // ~3% of clicks convert
			a := pickAff(s.rng, sources, func(a *demoAff) float64 { return a.traffic })
			c := s.clickSpec(a, s.diurnal(dayStart))
			out = append(out, demoEvent{at: c.at, seq: s.nextSeq(), click: &c})
		}
	}
	return out
}

func (s *demoSeeder) organicPurchase(a *demoAff) error {
	p := s.pickProduct()
	b := demoBuy{aff: a, product: p}
	if a.coupon != "" && s.rng.Float64() < a.couponShare {
		b.coupon, b.noClick = a.coupon, true // typed the code, never clicked
	}
	cid, cust, err := s.buy(b)
	if err != nil {
		return err
	}
	s.organicN++
	full := p.price
	switch {
	case !s.refundPaid && a.code == "maya" && p == demoAnnual && s.day >= 2 && s.day <= 30:
		// Paid out in the first run, refunded after: the clawback turns
		// Maya's payable into a receivable the second run nets.
		s.refundPaid = true
		s.schedule(s.dayAt(74, 11, 20), func() error { return s.refund(cust, cid, full, full) })
	case !s.refundApproved && !a.story && p == demoAnnual && s.day >= 15 && s.day <= 28:
		// Refunded after maturation: clawed back from the payable.
		s.refundApproved = true
		s.schedule(s.now.AddDate(0, 0, 35), func() error { return s.refund(cust, cid, full, full) })
	case s.organicN == 40:
		// A half refund while pending: only the rest matures.
		s.schedule(s.now.AddDate(0, 0, 5), func() error { return s.refund(cust, cid, full/2, full) })
	case s.rng.Float64() < 0.03:
		after := time.Duration(2+s.rng.IntN(10))*24*time.Hour + time.Duration(s.rng.IntN(600))*time.Minute
		s.schedule(s.now.Add(after), func() error { return s.refund(cust, cid, full, full) })
	}
	return nil
}

// demoBuy is one checkout.
type demoBuy struct {
	aff     *demoAff      // credited explicitly, as the billing adapters pass their resolved affiliate
	coupon  string        // coupon typed at checkout; with no aff, the engine's own coupon rung credits its owner
	noClick bool          // the buyer never clicked a link (coupon sales)
	cust    *demoCustomer // an existing customer (upgrade); nil = a new customer
	email   string        // a new customer's email, when the story needs a specific one
	product demoProduct
	country string               // billing geo ("" = the click's country)
	via     func(demoClick) bool // which of aff's clicks the buyer came through (nil = any)
}

func (s *demoSeeder) buy(b demoBuy) (int64, *demoCustomer, error) {
	owner := b.aff
	if owner == nil {
		owner = s.couponOwner(b.coupon)
	}
	var click *demoClick
	if b.aff != nil && !b.noClick {
		click = s.pickClick(b.aff, b.via)
	}
	country := b.country
	switch {
	case country != "":
	case click != nil:
		country = click.country
	case b.cust != nil:
		country = b.cust.country
	default:
		country = owner.geoW[0].v
	}
	cust := b.cust
	if cust == nil {
		cust = s.newCustomer(owner, country, b.email)
	}
	ev := commissions.Event{
		Source: "stripe", ExternalID: s.nextExt("in_demo_"), EventType: "purchase",
		AmountMinor: b.product.price, Currency: "USD",
		CustomerExtID: cust.ext, CustomerEmail: cust.email,
		CouponCode: b.coupon, ProductKey: b.product.key, Country: country,
		CampaignID: b.product.campaign, OccurredAt: s.now,
	}
	if b.aff != nil {
		ev.AffiliateID = b.aff.id
	}
	if click != nil {
		ev.ClickID = click.id
	}
	cid, err := s.screened.ProcessPurchase(s.ctx, ev)
	if err == nil && cid == 0 {
		err = errors.New("no commission credited")
	}
	if err != nil {
		return 0, nil, fmt.Errorf("demo-seed: purchase %s: %w", ev.ExternalID, err)
	}
	cust.plan = b.product
	if b.product.monthly {
		s.scheduleRenewal(cust)
	}
	return cid, cust, nil
}

// scheduleRenewal books the next monthly invoice (88% of subscribers stay).
func (s *demoSeeder) scheduleRenewal(c *demoCustomer) {
	next := s.now.AddDate(0, 0, 30)
	if !next.Before(s.end) {
		return
	}
	if s.rng.Float64() > 0.88 {
		c.active = false
		return
	}
	s.schedule(next, func() error { return s.renew(c) })
}

// renew bills a monthly subscriber. Like the billing adapters, the event
// names the customer's bound affiliate — unless it is no longer active:
// the resolver then leaves the event unattributed and the engine's own
// customer rung refuses the inactive affiliate too, so nothing credits.
func (s *demoSeeder) renew(c *demoCustomer) error {
	if !c.active || c.plan != demoMonthly {
		return nil // cancelled, refunded, or upgraded to annual
	}
	var status string
	if err := s.h.DB.QueryRowContext(s.ctx, `SELECT status FROM affiliates WHERE id = ?`, c.aff.id).Scan(&status); err != nil {
		return err
	}
	ev := commissions.Event{
		Source: "stripe", ExternalID: s.nextExt("in_demo_"), EventType: "renewal",
		AmountMinor: demoMonthly.price, Currency: "USD",
		CustomerExtID: c.ext, CustomerEmail: c.email, ProductKey: demoMonthly.key,
		CampaignID: 1, OccurredAt: s.now,
	}
	if status == "active" {
		ev.AffiliateID = c.aff.id
	}
	if _, err := s.screened.ProcessPurchase(s.ctx, ev); err != nil && !errors.Is(err, commissions.ErrNoAttribution) {
		return fmt.Errorf("demo-seed: renewal %s: %w", ev.ExternalID, err)
	}
	s.scheduleRenewal(c)
	return nil
}

// refund reverses refunded minor units of a purchase (a full refund also
// ends the subscription).
func (s *demoSeeder) refund(c *demoCustomer, commissionID, refunded, gross int64) error {
	ev := commissions.Event{
		Source: "stripe", ExternalID: s.nextExt("re_demo_"), EventType: "refund",
		AmountMinor: refunded, Currency: "USD", CustomerExtID: c.ext, OccurredAt: s.now,
	}
	if err := s.screened.ProcessRefund(s.ctx, ev, commissionID, refunded, gross); err != nil {
		return fmt.Errorf("demo-seed: refund %s: %w", ev.ExternalID, err)
	}
	if refunded >= gross {
		c.active = false
	}
	return nil
}

// payoutRun batches everything payable (the admin API's payout run); the
// first run is approved and paid, the second waits for approval.
func (s *demoSeeder) payoutRun(pay bool) error {
	runID, n, err := s.runner.CreateBatch(s.ctx, 2500, "paypal")
	if err != nil {
		return err
	}
	if err := s.runner.CompleteRun(s.ctx, runID, fmt.Sprintf("scheduled: %d payouts, method=paypal", n)); err != nil {
		return err
	}
	if !pay {
		return nil
	}
	rows, err := s.h.DB.QueryContext(s.ctx, `SELECT payout_id FROM payout_run_lines WHERE run_id = ? ORDER BY payout_id`, runID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	for _, id := range ids {
		if err := s.runner.Approve(s.ctx, id, nil); err != nil {
			return fmt.Errorf("demo-seed: approve payout %d: %w", id, err)
		}
	}
	return nil
}

func (s *demoSeeder) mature() error {
	for {
		n, err := s.engine.Mature(s.ctx)
		if err != nil || n == 0 {
			return err
		}
	}
}

// finish runs what the server's background workers would have done by
// now: XP and badges from the commission outbox and the click history,
// and the token-dimension rollup behind the stats drill.
func (s *demoSeeder) finish() error {
	g := gamify.New(s.h.DB)
	d := outbox.NewDispatcher(s.h.DB, 5*time.Millisecond, 500)
	d.Register(gamify.NewOutboxSink(g, s.h.DB), "commission.created")
	d.Start(s.ctx)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var left int
		if err := s.h.DB.QueryRowContext(s.ctx,
			`SELECT COUNT(*) FROM outbox_events WHERE delivered_at IS NULL AND topic = 'commission.created'`).Scan(&left); err != nil {
			d.Stop()
			return err
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			d.Stop()
			return fmt.Errorf("demo-seed: %d outbox events still undelivered", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.Stop()
	if err := g.TickActivity(s.ctx); err != nil {
		return err
	}
	_, err := analytics.NewTokenStats(s.h.DB).Rollup(s.ctx)
	return err
}

// --- fraud stories: one per rule the engine can fire ---

// planStories books each story's clicks and checkouts. Every verdict and
// hold comes from the fraud engine screening those checkouts. There is no
// ip_fallback_anomaly story: nothing records attribution_method
// 'ip_fallback' (billing adapters skip flagged IP-fallback resolutions and
// the engine's own ladder has no IP rung), so that rule cannot fire from
// any data the real paths write.
func (s *demoSeeder) planStories() {
	// self_referral: Dana buys through her own link with a plus-alias of
	// her own address.
	dana := s.byCode["danaw"]
	s.scheduleBuy(s.dayAt(84, 19, 12), demoBuy{aff: dana, product: demoAnnual, email: "dana.whitfield+notes@example.org"})

	// click_velocity: 70 clicks from one address in 45 minutes, then a
	// sale through the last of them.
	burst := s.byCode["burstlink"]
	for i := range 70 {
		c := s.clickSpec(burst, s.dayAt(86, 2, 5).Add(time.Duration(i)*38*time.Second))
		c.ip, c.country, c.source, c.referer, c.tag = "73.201.14.77", "US", "reddit", demoReferers["reddit"], "burst"
		c.ua, c.accept = demoUAs[1], true // one browser, refreshing
		s.scheduleClick(c)
	}
	s.scheduleBuy(s.dayAt(86, 2, 55), demoBuy{aff: burst, product: demoMonthly, via: tagged("burst")})

	// conversion_velocity: a cashback email blast, 30 sales inside an hour.
	qc := s.byCode["quickcash"]
	for i := range 60 {
		c := s.clickSpec(qc, s.dayAt(85, 13, 0).Add(time.Duration(i)*115*time.Second))
		c.subid, c.tag = "email-blast", "blast"
		s.scheduleClick(c)
	}
	for i := range 30 {
		s.scheduleBuy(s.dayAt(85, 15, 0).Add(time.Duration(i)*96*time.Second),
			demoBuy{aff: qc, product: demoMonthly, via: tagged("blast")})
	}

	// coupon_hijack, then inactive_affiliate: DealDrop's coupon lands on
	// customers other affiliates already referred (the engine freezes the
	// owner at the third), and a later sale on the coupon still credits
	// the frozen account. The hijacks go through the engine's own coupon
	// rung (no affiliate on the event): the billing adapters resolve an
	// active owner's coupon themselves and pass it as explicit, which the
	// rule does not count. After the freeze both the resolver and the
	// engine's rung refuse the frozen owner; the late sale is a delivery
	// that still names the account explicitly (resolved before the freeze
	// landed), which inactive_affiliate holds.
	dd := s.byCode["dealdrop"]
	for _, at := range []time.Time{s.dayAt(62, 10, 10), s.dayAt(66, 18, 40), s.dayAt(70, 12, 5), s.dayAt(74, 21, 15), s.dayAt(78, 9, 50)} {
		s.scheduleBuy(at, demoBuy{aff: dd, coupon: "DROP40", noClick: true, product: demoAnnual})
	}
	for _, at := range []time.Time{s.dayAt(83, 11, 20), s.dayAt(84, 16, 5), s.dayAt(85, 20, 40)} {
		s.schedule(at, func() error {
			victim := s.hijackVictim()
			if victim == nil {
				return errors.New("demo-seed: no customer to hijack")
			}
			_, _, err := s.buy(demoBuy{coupon: "DROP40", cust: victim, product: demoAnnual})
			return err
		})
	}
	s.scheduleBuy(s.dayAt(87, 13, 30), demoBuy{aff: dd, coupon: "DROP40", noClick: true, product: demoAnnual})

	// conversion_time_anomaly: NightOwl's sales arrive while the program
	// sleeps (18 of 21 between 00:00 and 06:00 UTC).
	no := s.byCode["nightowl"]
	for d := 71; d <= 88; d++ {
		for range 25 {
			s.scheduleClick(s.clickSpec(no, s.dayAt(d, 0, 0).Add(time.Duration(s.rng.IntN(86400))*time.Second)))
		}
	}
	for _, t := range [][3]int{
		{83, 1, 12},
		{83, 3, 40},
		{83, 14, 30},
		{84, 0, 25},
		{84, 2, 18},
		{84, 4, 51},
		{85, 1, 7},
		{85, 2, 44},
		{85, 5, 2},
		{85, 16, 20},
		{86, 0, 48},
		{86, 3, 15},
		{86, 4, 33},
		{87, 1, 29},
		{87, 2, 57},
		{87, 4, 10},
		{87, 13, 5},
		{88, 0, 36},
		{88, 2, 22},
		{88, 3, 49},
		{88, 5, 18},
	} {
		s.scheduleBuy(s.dayAt(t[0], t[1], t[2]), demoBuy{aff: no, product: demoMonthly})
	}

	// asn_reputation: ClickFlow's traffic comes from cloud servers.
	cf := s.byCode["clickflow"]
	for d := 76; d <= 88; d++ {
		for range 35 {
			c := s.clickSpec(cf, s.diurnal(s.dayAt(d, 0, 0)))
			if s.rng.Float64() < 0.9 {
				c.ip = s.dcIP(cf)
			}
			s.scheduleClick(c)
		}
	}
	for _, at := range []time.Time{s.dayAt(84, 15, 10), s.dayAt(85, 11, 45), s.dayAt(87, 17, 30), s.dayAt(88, 10, 5)} {
		s.scheduleBuy(at, demoBuy{aff: cf, product: demoMonthly, via: func(c demoClick) bool { return c.dc }})
	}

	// ip_overlap: two "different" agencies sharing one office's four
	// addresses; their sales come through those clicks.
	gl, pn := s.byCode["growthloop"], s.byCode["promonest"]
	office := []string{"92.213.40.11", "92.213.40.12", "92.213.40.13", "92.213.40.14"}
	for d := 60; d <= 88; d++ {
		for _, a := range []*demoAff{gl, pn} {
			for range 12 {
				c := s.clickSpec(a, s.diurnal(s.dayAt(d, 0, 0)))
				if s.rng.Float64() < 0.4 {
					c.ip = office[s.rng.IntN(len(office))]
				}
				s.scheduleClick(c)
			}
		}
	}
	fromOffice := func(c demoClick) bool { return slices.Contains(office, c.ip) }
	for _, t := range [][4]int{
		{66, 14, 0, 0},
		{74, 16, 30, 0},
		{81, 12, 15, 0},
		{87, 18, 45, 0},
		{69, 10, 20, 1},
		{77, 19, 5, 1},
		{83, 15, 40, 1},
		{88, 13, 25, 1},
	} {
		a := gl
		if t[3] == 1 {
			a = pn
		}
		s.scheduleBuy(s.dayAt(t[0], t[1], t[2]), demoBuy{aff: a, product: demoMonthly, via: fromOffice})
	}

	// duplicate_account: Jordan opens a second account on a plus-alias of
	// his own address; sales on either account now go to review.
	jm, jd := s.byCode["jmiles"], s.byCode["jmilesdeals"]
	for d := 79; d <= 88; d++ {
		for range 8 {
			s.scheduleClick(s.clickSpec(jd, s.diurnal(s.dayAt(d, 0, 0))))
		}
	}
	s.scheduleBuy(s.dayAt(81, 17, 20), demoBuy{aff: jd, product: demoMonthly})
	s.scheduleBuy(s.dayAt(84, 12, 40), demoBuy{aff: jm, product: demoMonthly})
	s.scheduleBuy(s.dayAt(86, 11, 10), demoBuy{aff: jd, product: demoAnnual})

	// payment_fingerprint: the same three buyers purchase through both
	// TopTools and SaaS Stack Picks (accounts buying from each other).
	tt, sp := s.byCode["toptools"], s.byCode["saasstack"]
	for _, b := range []struct {
		d, h, m int
		a       *demoAff
		email   string
	}{
		{79, 13, 0, tt, "nate.price@example.net"},
		{80, 15, 30, sp, "nate.price@example.net"},
		{82, 10, 45, tt, "ola.berg@example.org"},
		{84, 19, 20, sp, "ola.berg@example.org"},
		{87, 16, 0, tt, "ray.soto@example.com"},
	} {
		s.scheduleBuy(s.dayAt(b.d, b.h, b.m), demoBuy{aff: b.a, product: demoMonthly, email: b.email})
	}

	// bot_class_gating: ViralBoost's traffic is mostly suspected bots
	// (browser user agent, no Accept/Accept-Language headers).
	vb := s.byCode["viralboost"]
	for d := 78; d <= 88; d++ {
		for range 40 {
			c := s.clickSpec(vb, s.diurnal(s.dayAt(d, 0, 0)))
			c.accept = s.rng.Float64() >= 0.65
			s.scheduleClick(c)
		}
	}
	for _, at := range []time.Time{s.dayAt(84, 13, 40), s.dayAt(85, 18, 25), s.dayAt(86, 12, 10), s.dayAt(88, 15, 55)} {
		s.scheduleBuy(at, demoBuy{aff: vb, product: demoMonthly, via: func(c demoClick) bool { return c.suspect }})
	}

	// geo_mismatch: Lena's readers are in the US and Canada, but half of
	// her recent checkouts bill from countries her links never saw.
	lena := s.byCode["lenaortiz"]
	for _, t := range []struct {
		d, h, m int
		country string
	}{{82, 14, 10, "US"}, {83, 17, 45, "CA"}, {84, 3, 20, "VN"}, {85, 11, 5, "NG"}, {86, 20, 30, "US"}, {87, 9, 15, "PK"}} {
		s.scheduleBuy(s.dayAt(t.d, t.h, t.m), demoBuy{aff: lena, product: demoMonthly, country: t.country})
	}
}

func tagged(tag string) func(demoClick) bool {
	return func(c demoClick) bool { return c.tag == tag }
}

// hijackVictim picks the next monthly subscriber of an ordinary affiliate
// (a different affiliate each time) for DealDrop's coupon to land on.
func (s *demoSeeder) hijackVictim() *demoCustomer {
	for _, c := range s.customers {
		if c.active && c.plan == demoMonthly && !c.aff.story && !s.victims[c.aff] {
			s.victims[c.aff] = true
			return c
		}
	}
	return nil
}

// --- clicks ---

// clickSpec draws an ordinary click on a's links at time at.
func (s *demoSeeder) clickSpec(a *demoAff, at time.Time) demoClickSpec {
	c := demoClickSpec{
		aff: a, at: at, accept: true,
		ua:      demoUAs[s.rng.IntN(len(demoUAs))],
		country: pickWeighted(s.rng, a.geoW),
		source:  pickWeighted(s.rng, a.srcW),
		landing: pickWeighted(s.rng, demoLandings),
	}
	if len(a.subW) > 0 && s.rng.Float64() < 0.6 {
		c.subid = pickWeighted(s.rng, a.subW)
	}
	c.referer = demoReferers[c.source]
	if c.source == "blog" {
		c.referer = "https://" + a.code + ".example/"
		c.placement = pickWeighted(s.rng, parseWeights("inline:3 sidebar:1 header:1"))
	}
	switch r := s.rng.Float64(); {
	case r < 0.02: // crawlers and scripts: stored, excluded from stats
		c.ua = demoBotUAs[s.rng.IntN(len(demoBotUAs))]
	case r < 0.05: // header-less clients the classifier distrusts
		c.accept = false
	}
	switch r := s.rng.Float64(); {
	case r < 0.015:
		c.ip = s.dcIP(a)
	case r < 0.2 && len(a.recentIPs) > 0: // a returning visitor
		c.ip = a.recentIPs[s.rng.IntN(len(a.recentIPs))]
	default:
		c.ip = fmt.Sprintf("%d.%d.%d.%d", demoHomeOctets[s.rng.IntN(len(demoHomeOctets))], a.ipB,
			s.rng.IntN(256), 1+s.rng.IntN(254))
		a.recentIPs = append(a.recentIPs, c.ip)
		if len(a.recentIPs) > 50 {
			a.recentIPs = a.recentIPs[1:]
		}
	}
	return c
}

// dcIP draws a cloud-provider address unique to a (AWS, GCP, Hetzner,
// DigitalOcean ranges the fingerprint package classifies as datacenter).
func (s *demoSeeder) dcIP(a *demoAff) string {
	host := 1 + s.rng.IntN(254)
	switch s.rng.IntN(4) {
	case 0:
		return fmt.Sprintf("3.%d.%d.%d", a.ipB, s.rng.IntN(256), host)
	case 1:
		return fmt.Sprintf("34.%d.%d.%d", 64+a.ipB, s.rng.IntN(256), host)
	case 2:
		return fmt.Sprintf("5.9.%d.%d", a.ipB, host)
	default:
		return fmt.Sprintf("159.65.%d.%d", a.ipB, host)
	}
}

// enqueue hands a click to the real committer, enriched exactly like the
// redirect handler does (classification, identity hashes, datacenter flag).
func (s *demoSeeder) enqueue(c demoClickSpec) error {
	bc, _ := fingerprint.Classify(c.ua, c.accept, c.accept)
	ipHash := fingerprint.NewIdentity(fingerprint.ByIP, c.ip)
	uaHash := fingerprint.NewIdentity(fingerprint.ByIPUA, c.ip, c.ua)
	isDC := fingerprint.IsDatacenterIP(c.ip)
	id, wg, err := s.committer.Enqueue(c.aff.id, 1, clicks.Click{
		IPHash: ipHash[:], UAHash: uaHash[:],
		Referer: c.referer, LandingPath: c.landing,
		UTMSource: c.source, SubID: c.subid, Placement: c.placement,
		Country: c.country, BotClass: int(bc), IsDC: isDC,
		AttributionAffiliateID: c.aff.id, AttributionCampaignID: 1,
		AttributionTTL: 30 * 24 * time.Hour,
	}, c.at)
	if err != nil {
		return fmt.Errorf("demo-seed: click: %w", err)
	}
	s.lastWG = wg
	if bc != fingerprint.Known {
		c.aff.clicks = append(c.aff.clicks, demoClick{
			id: id, ip: c.ip, country: c.country, tag: c.tag, at: c.at,
			suspect: bc == fingerprint.Suspected, dc: isDC,
		})
	}
	return nil
}

// flushClicks waits until every enqueued click is committed: a checkout
// must see the clicks before it (the fraud rules read them).
func (s *demoSeeder) flushClicks() {
	if s.lastWG != nil {
		s.lastWG.Wait()
		s.lastWG = nil
	}
}

// pickClick picks the click a buyer came through: one of a's 40 latest
// matching clicks from the last 30 days (nil when there is none).
func (s *demoSeeder) pickClick(a *demoAff, via func(demoClick) bool) *demoClick {
	var cands []int
	for i := len(a.clicks) - 1; i >= 0 && len(cands) < 40; i-- {
		c := a.clicks[i]
		if c.at.After(s.now) {
			continue
		}
		if s.now.Sub(c.at) > 30*24*time.Hour {
			break
		}
		if via == nil || via(c) {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	c := a.clicks[cands[s.rng.IntN(len(cands))]]
	return &c
}

// --- helpers ---

func (s *demoSeeder) pickProduct() demoProduct {
	switch r := s.rng.Float64(); {
	case r < 0.62:
		return demoMonthly
	case r < 0.90:
		return demoAnnual
	default:
		return demoTeams
	}
}

func (s *demoSeeder) newCustomer(owner *demoAff, country, email string) *demoCustomer {
	s.custSeq++
	n := s.custSeq
	if email == "" {
		first := demoFirst[n%len(demoFirst)]
		last := demoLast[(n/len(demoFirst))%len(demoLast)]
		email = fmt.Sprintf("%s.%s%d@example.%s", strings.ToLower(first), strings.ToLower(last), n, demoTLDs[n%len(demoTLDs)])
	}
	c := &demoCustomer{ext: fmt.Sprintf("cus_demo_%05d", n), email: email, country: country, aff: owner, active: true}
	s.customers = append(s.customers, c)
	return c
}

func (s *demoSeeder) couponOwner(code string) *demoAff {
	for _, a := range s.affs {
		if a.coupon == code {
			return a
		}
	}
	panic("demo-seed: unknown coupon " + code)
}

func (s *demoSeeder) dayAt(d, h, m int) time.Time {
	return s.start.AddDate(0, 0, d).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

// diurnal draws a time of day on dayStart's date, weighted by demoHours.
func (s *demoSeeder) diurnal(dayStart time.Time) time.Time {
	total := 0.0
	for _, w := range demoHours {
		total += w
	}
	x := s.rng.Float64() * total
	h := 0
	for ; h < 23 && x >= demoHours[h]; h++ {
		x -= demoHours[h]
	}
	return dayStart.Add(time.Duration(h)*time.Hour + time.Duration(s.rng.IntN(3600))*time.Second)
}

func (s *demoSeeder) nextSeq() int { s.seq++; return s.seq }

func (s *demoSeeder) nextExt(prefix string) string {
	s.extSeq++
	return fmt.Sprintf("%s%06d", prefix, s.extSeq)
}

func (s *demoSeeder) event(at time.Time, fn func() error) demoEvent {
	return demoEvent{at: at, seq: s.nextSeq(), run: fn}
}

// slot is the replay day of time at; ok=false past the end of history
// (it has not happened yet, so it is dropped).
func (s *demoSeeder) slot(at time.Time) (d int, ok bool) {
	if at.Before(s.start) || !at.Before(s.end) {
		return 0, false
	}
	d = int(at.Sub(s.start) / (24 * time.Hour))
	if s.replaying && d <= s.day {
		// That day's events are already sorted and running: a late
		// addition would silently never happen.
		panic(fmt.Sprintf("demo-seed: event at %s booked into replayed day %d", at, d))
	}
	return d, true
}

// schedule books fn at time at.
func (s *demoSeeder) schedule(at time.Time, fn func() error) {
	if d, ok := s.slot(at); ok {
		s.pending[d] = append(s.pending[d], s.event(at, fn))
	}
}

func (s *demoSeeder) scheduleBuy(at time.Time, b demoBuy) {
	s.schedule(at, func() error {
		_, _, err := s.buy(b)
		return err
	})
}

func (s *demoSeeder) scheduleClick(c demoClickSpec) {
	if d, ok := s.slot(c.at); ok {
		s.pending[d] = append(s.pending[d], demoEvent{at: c.at, seq: s.nextSeq(), click: &c})
	}
}

func (s *demoSeeder) ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func pickWeighted(r *rand.Rand, ws []demoWeighted) string {
	if len(ws) == 0 {
		return ""
	}
	total := 0.0
	for _, w := range ws {
		total += w.w
	}
	x := r.Float64() * total
	for _, w := range ws {
		if x < w.w {
			return w.v
		}
		x -= w.w
	}
	return ws[len(ws)-1].v
}

func pickAff(r *rand.Rand, as []*demoAff, weight func(*demoAff) float64) *demoAff {
	total := 0.0
	for _, a := range as {
		total += weight(a)
	}
	x := r.Float64() * total
	for _, a := range as {
		if x < weight(a) {
			return a
		}
		x -= weight(a)
	}
	return as[len(as)-1]
}

// poisson draws a Poisson count (normal approximation for large means).
func poisson(r *rand.Rand, lam float64) int {
	if lam <= 0 {
		return 0
	}
	if lam > 30 {
		return max(int(math.Round(lam+math.Sqrt(lam)*r.NormFloat64())), 0)
	}
	l, k, p := math.Exp(-lam), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// --- inspection ---

// demoCounts summarizes a seeded database (the CLI report; tests compare
// two seeds with it).
type demoCounts map[string]int64

func countDemo(ctx context.Context, dbh *sql.DB) (demoCounts, error) {
	c := demoCounts{}
	for _, q := range []struct{ name, sql string }{
		{"affiliates", `SELECT COUNT(*) FROM affiliates`},
		{"affiliates_frozen", `SELECT COUNT(*) FROM affiliates WHERE status = 'frozen'`},
		{"applications_pending", `SELECT COUNT(*) FROM affiliate_applications WHERE status = 'pending'`},
		{"applications", `SELECT COUNT(*) FROM affiliate_applications`},
		{"clicks", `SELECT COUNT(*) FROM clicks`},
		{"clicks_counted", `SELECT COALESCE(SUM(clicks), 0) FROM daily_agg`},
		{"customers", `SELECT COUNT(*) FROM customers`},
		{"commissions", `SELECT COUNT(*) FROM commissions WHERE amount > 0`},
		{"commissions_pending", `SELECT COUNT(*) FROM commissions WHERE amount > 0 AND status = 'pending'`},
		{"commissions_approved", `SELECT COUNT(*) FROM commissions WHERE amount > 0 AND status = 'approved'`},
		{"commissions_paid", `SELECT COUNT(*) FROM commissions WHERE amount > 0 AND status = 'paid'`},
		{"commission_cents", `SELECT COALESCE(SUM(amount), 0) FROM commissions`},
		{"clawbacks", `SELECT COUNT(*) FROM commissions WHERE amount < 0`},
		{"holds_open", `SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`},
		{"verdicts", `SELECT COUNT(*) FROM fraud_verdicts`},
		{"rules_fired", `SELECT COUNT(DISTINCT rule) FROM fraud_verdicts`},
		{"payout_runs", `SELECT COUNT(*) FROM payout_runs WHERE status = 'completed'`},
		{"payouts_paid", `SELECT COUNT(*) FROM payouts WHERE status = 'paid'`},
		{"payouts_requested", `SELECT COUNT(*) FROM payouts WHERE status = 'requested'`},
		{"badges", `SELECT COUNT(*) FROM affiliate_badges`},
		{"xp", `SELECT COALESCE(SUM(xp), 0) FROM xp_totals`},
	} {
		var n int64
		if err := dbh.QueryRowContext(ctx, q.sql).Scan(&n); err != nil {
			return nil, fmt.Errorf("demo-seed: count %s: %w", q.name, err)
		}
		c[q.name] = n
	}
	rows, err := dbh.QueryContext(ctx, `SELECT rule, COUNT(*) FROM fraud_verdicts GROUP BY rule`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rule string
		var n int64
		if err := rows.Scan(&rule, &n); err != nil {
			return nil, err
		}
		c["verdicts:"+rule] = n
	}
	return c, rows.Err()
}
