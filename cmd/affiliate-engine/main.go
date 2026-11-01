// affiliate-engine: self-hostable affiliate tracking, fraud-first.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/billing"
	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/config"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fingerprint"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/gamify"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/mail"
	"github.com/thebigasif/affiliate-engine/internal/mcp"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/mlm"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
	"github.com/thebigasif/affiliate-engine/internal/plan"
	"github.com/thebigasif/affiliate-engine/internal/postbacks"
	"github.com/thebigasif/affiliate-engine/internal/privacy"
	"github.com/thebigasif/affiliate-engine/internal/screening"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
	"github.com/thebigasif/affiliate-engine/internal/ui"
)

// version is stamped at release-build time via
// -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	// Owner-only file creation (db, -wal/-shm, backups, generated secret)
	// before anything touches the filesystem.
	setUmask()

	dsn := flag.String("db", "", `database dsn: "sqlite:<path>" (default sqlite:affiliate-engine.db)`)
	listen := flag.String("listen", ":8080", "http listen address")
	flag.Parse()

	// Subcommands (before server start) resolve from POSITIONAL args only
	// (flag.Args) — flag values can never shadow-trigger a subcommand,
	// and an unknown word errors out instead of silently serving.
	if serve, code := dispatchCLI(flag.Args(), *dsn); !serve {
		os.Exit(code)
	}

	cfg := config.Defaults()
	if *dsn != "" {
		cfg.Database = *dsn
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	// Trusted reverse proxies: the only peers whose X-Forwarded-For /
	// X-Forwarded-Proto / CDN geo headers are believed.
	trusted, err := config.ParseTrustedProxies(os.Getenv("AE_TRUSTED_PROXIES"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
		os.Exit(1)
	}
	cfg.TrustedProxies = trusted
	redirectRate := 0 // 0 = handler default (120/min per IP)
	if s := os.Getenv("AE_REDIRECT_RATE_PER_MIN"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "affiliate-engine: AE_REDIRECT_RATE_PER_MIN must be an integer (negative disables)")
			os.Exit(1)
		}
		redirectRate = n
	}

	// NFR-7 privacy option: write-time IP anonymization on the click path.
	// Unknown values fail loud — a privacy control must never silently
	// default (the operator asked for something we don't implement).
	ipMode, err := privacy.ParseIPMode(os.Getenv("AE_IP_MODE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
		os.Exit(1)
	}
	if ipMode != privacy.IPModeOff {
		fmt.Printf("affiliate-engine: IP anonymization active (%s)\n", os.Getenv("AE_IP_MODE"))
	}

	// Managed-cloud plan (AE_PLAN): unset on self-host = every feature.
	// Unknown names fail loud rather than silently locking or unlocking.
	cloudPlan, err := plan.Parse(os.Getenv("AE_PLAN"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Public read-only demo (AE_DEMO=1, demo.go): fresh seed + daily restart.
	demo, err := setupDemo(ctx, cfg.Database, stop)
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
		os.Exit(1)
	}

	// Engine-aware migration (docs audit #8): the serve path previously
	// hardcoded the SQLite chain — a -db postgres:// deployment crashed at
	// startup on PRAGMA/datetime('now') syntax the PG engine rejects.
	migEngine := migrations.SQLite
	if isPostgresDSN(cfg.Database) {
		migEngine = migrations.Postgres
	}

	// Two-role deployments (DEPLOY.md): AE_MIGRATE_DATABASE_URL is the
	// schema OWNER's DSN, used only to apply migrations; the server then
	// runs on -db, a runtime role without DELETE/TRUNCATE on money tables.
	if migDSN := os.Getenv("AE_MIGRATE_DATABASE_URL"); migDSN != "" {
		if isPostgresDSN(migDSN) != (migEngine == migrations.Postgres) {
			fmt.Fprintln(os.Stderr, "affiliate-engine: AE_MIGRATE_DATABASE_URL and -db must use the same engine")
			os.Exit(1)
		}
		mh, err := db.Open(ctx, migDSN, db.Options{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "affiliate-engine: migrate connection: %v\n", err)
			os.Exit(1)
		}
		err = migrations.Apply(ctx, mh.DB, migEngine)
		_ = mh.Ledger.Close()
		_ = mh.DB.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "affiliate-engine: migrate: %v\n", err)
			os.Exit(1)
		}
	}

	h, err := db.Open(ctx, cfg.Database, db.Options{LedgerSyncFull: cfg.LedgerSyncFull})
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
		os.Exit(1)
	}
	if os.Getenv("AE_MIGRATE_DATABASE_URL") == "" {
		if err := migrations.Apply(ctx, h.DB, migEngine); err != nil {
			fmt.Fprintf(os.Stderr, "affiliate-engine: migrate: %v\n", err)
			os.Exit(1)
		}
	}

	if err := bootstrapAdminKey(ctx, h.DB, os.Getenv("AE_BOOTSTRAP_ADMIN_KEY"), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: bootstrap admin key: %v\n", err)
		os.Exit(1)
	}

	secret, err := loadOrCreateSecret(cfg.Database)
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: secret: %v\n", err)
		os.Exit(1)
	}
	// Click identity hashes (ip_hash / ua_hash) are HMAC'd under a key
	// derived from the deployment secret: stable across restarts, and not
	// reversible by brute-forcing the IPv4 space. Must run before the
	// click path serves traffic.
	fingerprint.SetHashKey(fingerprint.DeriveHashKey(secret))
	if demo {
		// Payout details are sealed under the deployment secret, so the
		// demo's sample ones are added here rather than by the seed.
		if err := httpapi.SeedDemoPayoutDetails(ctx, h.DB, httpapi.DeriveTaxDocKey(secret)); err != nil {
			fmt.Fprintf(os.Stderr, "affiliate-engine: demo payout details: %v\n", err)
		}
		if err := httpapi.SeedDemoResources(ctx, h.DB); err != nil {
			fmt.Fprintf(os.Stderr, "affiliate-engine: demo resources: %v\n", err)
		}
	}
	signer, err := tokens.New(secret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: signer: %v\n", err)
		os.Exit(1)
	}

	// Core services.
	poster := ledger.NewPoster(h.Ledger)
	engine := commissions.NewEngine(h.DB, poster)
	// MLM business volume: every credited commission flows into the trees
	// automatically (was: manual endpoint choreography — the tree existed
	// but dollars never reached it).
	mlmEngine := mlm.New(h.DB)
	engine.MLMBV = mlmEngine.RecordBV
	// Fraud screening at the conversion boundary: every webhook purchase is
	// evaluated in the transaction that credits the commission (the verdict
	// and its hold commit with the credit, or nothing does).
	// AE_FRAUD_DRYRUN=1 screens without enforcing (shadow rollout) — on the
	// webhook path AND the periodic sweep below.
	fraudDryRun := os.Getenv("AE_FRAUD_DRYRUN") == "1"
	fraudEngine := fraud.NewEngine(h.DB)
	screened := screening.New(engine, fraudEngine, fraudDryRun)
	resolver := attribution.NewResolver(h.DB, signer)
	committer := clicks.NewCommitter(ctx, h.DB, cfg.ClickBatchMax, time.Duration(cfg.ClickFlushMs)*time.Millisecond)
	gamifyEngine := gamify.New(h.DB)
	if err := gamifyEngine.SeedBadges(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: badge seed: %v\n", err)
	}

	webhookHealthDB = h.DB
	dispatcher := outbox.NewDispatcher(h.DB, 2*time.Second, 100)
	dispatcher.Register(outbox.LogSink{}, "*")
	dispatcher.Register(gamify.NewOutboxSink(gamifyEngine, h.DB), "commission.created")
	// Partner emails (AE_MAIL_*): login links and payout notes. Off without
	// a provider, in the demo, and without AE_PUBLIC_URL (no link to send).
	mailCfg, err := mail.FromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
		os.Exit(1)
	}
	mailer := mail.New(mailCfg)
	if mailer != nil && (demo || os.Getenv("AE_PUBLIC_URL") == "") {
		if !demo {
			fmt.Fprintln(os.Stderr, "affiliate-engine: partner emails off: set AE_PUBLIC_URL so emails can link to the portal")
		}
		mailer = nil
	}
	if mailer != nil {
		dispatcher.Register(&partnermail.Sink{DB: h.DB, Mailer: mailer, PublicURL: os.Getenv("AE_PUBLIC_URL"), NewsKey: partnermail.DeriveNewsKey(secret)},
			partnermail.TopicLoginLink, partnermail.TopicPayoutPaid, partnermail.TopicNews, partnermail.TopicTips, partnermail.TopicRecap, partnermail.TopicRejected,
			partnermail.TopicPayoutDetailsChanged)
		fmt.Printf("affiliate-engine: partner emails on (%s)\n", mailCfg.Provider)
	}
	dispatcher.Start(ctx)

	reconciler := analytics.NewReconciler(h.DB)
	// Privacy retention: clicks pruned past the window (default 90d;
	// AE_CLICK_RETENTION_DAYS overrides; 0 disables).
	if d := os.Getenv("AE_CLICK_RETENTION_DAYS"); d != "" {
		var days int
		if _, err := fmt.Sscanf(d, "%d", &days); err != nil {
			// Raw env value deliberately not logged (log-injection guard):
			// the parse error is sufficient for the operator.
			log.Printf("AE_CLICK_RETENTION_DAYS: unparsable (%v) — using 90d default", err)
			days = 90
		}
		if days > 0 {
			go analytics.NewRetention(h.DB, time.Duration(days)*24*time.Hour).RunForever(ctx)
		}
	} else {
		go analytics.NewRetention(h.DB, 0).RunForever(ctx)
	}
	payoutRunner := payouts.NewRunner(h.DB)
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := poster.Post(ctx, payouts.SettleKey(payoutID), "payout", "",
			time.Now().UTC(), payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = poster // the settle journal commits with the payout rows

	fallbackURL, err := fallbackURLFromEnv(os.Getenv("AE_FALLBACK_URL"))
	if err != nil {
		log.Fatal(err)
	}

	// HTTP surface.
	secureCookies := httpapi.SecureCookiesFromEnv()
	mux := http.NewServeMux()
	mux.Handle("/r/", httpapi.NewRedirectHandler(h.DB, httpapi.Config{
		Signer:              signer,
		AllowedDestSuffixes: allowedDestinations(),
		FallbackURL:         fallbackURL,
		IPMode:              ipMode,
		CookieSecure:        secureCookies,
		TrustedProxies:      cfg.TrustedProxies,
		RateLimitPerMin:     redirectRate,
	}, committer))

	// JSON API (FR-29) + embedded UI (FR-27/28).
	mux.Handle("/api/v1/", httpapi.NewAPIHandler(httpapi.APIDeps{
		DB:             h.DB,
		PayoutRunner:   payoutRunner,
		RateLimit:      600,
		LedgerPoster:   poster,
		UsageScreen:    screened.ScreenUsage,
		MLM:            mlm.New(h.DB),
		Gamify:         gamifyEngine,
		Stats:          analytics.NewTokenStats(h.DB),
		TrustedProxies: cfg.TrustedProxies,
		SecureCookies:  secureCookies,
		Plan:           cloudPlan,
		Demo:           demo,
		MailOn:         mailer != nil,
		OwnerEmail:     ownerEmail(),
		SiteURL:        siteURL(fallbackURL),
		NewsKey:        partnermail.DeriveNewsKey(secret),
		Billing:        billingConfigured(),
		TaxDocKey:      httpapi.DeriveTaxDocKey(secret),
	}))
	mux.Handle("/static/", ui.Handler())
	// Embeddable affiliate-recruitment widget (FR-33 widget mode).
	mux.HandleFunc("/widget.js", planGate(cloudPlan, plan.FeatWidget, httpapi.NewWidgetHandler(httpapi.WidgetConfig{
		BaseURL: os.Getenv("AE_PUBLIC_URL"),
	})))
	// First-party tracking script for the merchant's site (core: not plan-gated).
	mux.Handle("/t.js", httpapi.NewTrackerHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Unconfigured API/webhook paths must 404, not serve the SPA shell
		// (a misconfigured provider webhook would otherwise "succeed").
		p := r.URL.Path
		if strings.HasPrefix(p, "/webhooks/") || strings.HasPrefix(p, "/api/") || p == "/mcp" {
			http.NotFound(w, r)
			return
		}
		ui.IndexHandler(w, r)
	})

	// Billing webhooks: each route exists only when its secret is set.
	// webhookHandler never echoes adapter errors to the (unauthenticated)
	// caller. limitWebhooks caps requests per client IP across all of them
	// (httpapi.WebhookPerIPPerMinute, far above any real provider burst).
	limitWebhooks := httpapi.NewIPLimiter(httpapi.WebhookPerIPPerMinute, cfg.TrustedProxies)

	// Paddle webhook (enabled when AE_PADDLE_SECRET is set).
	if s := os.Getenv("AE_PADDLE_SECRET"); s != "" {
		mux.HandleFunc("/webhooks/paddle", limitWebhooks(webhookHandler("paddle",
			billing.NewPaddleAdapter(s, screened, resolver, billing.NewSQLStore(h.DB)))))
	}

	// Chargebee webhook. Basic Auth (AE_CHARGEBEE_USERNAME and/or
	// AE_CHARGEBEE_PASSWORD) is Chargebee's native webhook protection and
	// wins when set; AE_CHARGEBEE_SECRET then also verifies a cb-signature
	// header when one is sent. With only AE_CHARGEBEE_SECRET the legacy
	// HMAC (cb-signature) mode is used. Neither set = route disabled.
	cbUser := os.Getenv("AE_CHARGEBEE_USERNAME")
	cbPass := os.Getenv("AE_CHARGEBEE_PASSWORD")
	cbSecret := os.Getenv("AE_CHARGEBEE_SECRET")
	switch {
	case cbUser != "" || cbPass != "":
		cb := billing.NewChargebeeBasicAuthAdapter(cbUser, cbPass, screened, resolver, billing.NewSQLStore(h.DB))
		cb.Secret = cbSecret
		mux.HandleFunc("/webhooks/chargebee", limitWebhooks(webhookHandler("chargebee", cb)))
	case cbSecret != "":
		mux.HandleFunc("/webhooks/chargebee", limitWebhooks(webhookHandler("chargebee",
			billing.NewChargebeeAdapter(cbSecret, screened, resolver, billing.NewSQLStore(h.DB)))))
	}

	// Lemon Squeezy webhook (enabled when AE_LEMONSQUEEZY_SECRET is set).
	if s := os.Getenv("AE_LEMONSQUEEZY_SECRET"); s != "" {
		mux.HandleFunc("/webhooks/lemonsqueezy", limitWebhooks(webhookHandler("lemonsqueezy",
			billing.NewLemonSqueezyAdapter(s, screened, resolver, billing.NewSQLStore(h.DB)))))
	}

	// Polar webhook (enabled when AE_POLAR_SECRET, the endpoint's whsec_
	// secret, is set).
	if s := os.Getenv("AE_POLAR_SECRET"); s != "" {
		mux.HandleFunc("/webhooks/polar", limitWebhooks(webhookHandler("polar",
			billing.NewPolarAdapter(s, screened, resolver, billing.NewSQLStore(h.DB)))))
	}

	// Recurly webhook (enabled when AE_RECURLY_USERNAME _or_
	// AE_RECURLY_PASSWORD is set — Recurly permits either Basic Auth
	// credential to be blank). AE_RECURLY_SIGNATURE_SECRET opts into
	// recurly-signature verification when the header appears (XML
	// endpoints are unsigned; JSON-mode endpoints send it).
	recurlyUser := os.Getenv("AE_RECURLY_USERNAME")
	recurlyPass := os.Getenv("AE_RECURLY_PASSWORD")
	if recurlyUser != "" || recurlyPass != "" {
		ra := billing.NewRecurlyAdapter(recurlyUser, recurlyPass,
			os.Getenv("AE_RECURLY_SIGNATURE_SECRET"), screened, resolver,
			billing.NewSQLStore(h.DB))
		// AE_RECURLY_API_KEY (read-only) lets a first sale read the
		// account's aff_token custom field; AE_RECURLY_API_BASE for EU sites.
		if key := os.Getenv("AE_RECURLY_API_KEY"); key != "" {
			ra.AccountToken = billing.RecurlyAccountToken(key, os.Getenv("AE_RECURLY_API_BASE"))
		}
		mux.HandleFunc("/webhooks/recurly", limitWebhooks(webhookHandler("recurly", ra)))
	}

	// Stripe webhook (enabled when AE_STRIPE_SECRET is set).
	// AE_STRIPE_SECRET_KEY (the payouts name) is accepted as a fallback:
	// one secret serves both when they're the same key — the old split
	// silently disabled webhooks OR payouts for operators who set the
	// "wrong" one.
	stripeSecret := os.Getenv("AE_STRIPE_SECRET")
	if stripeSecret == "" {
		stripeSecret = os.Getenv("AE_STRIPE_SECRET_KEY")
	}
	if s := stripeSecret; s != "" {
		sa := billing.NewStripeAdapter(s, screened, resolver, billing.NewSQLStore(h.DB))
		// AE_STRIPE_PRODUCTS: only sales of these products, prices or
		// payment links earn commission (a Stripe account shared with
		// other products). AE_STRIPE_API_KEY lets the engine look up a
		// one-time checkout's items, which Stripe leaves out of webhooks.
		if list := os.Getenv("AE_STRIPE_PRODUCTS"); strings.TrimSpace(list) != "" {
			f, err := billing.ParseProductFilter(list)
			if err != nil {
				log.Fatal(err)
			}
			if key := stripeAPIKey(); key != "" {
				f.LineItems = billing.StripeLineItems(key, os.Getenv("AE_STRIPE_API_BASE"))
			}
			sa.Products = f
			log.Printf("stripe: counting only %d listed products, prices or payment links", len(f.IDs))
		}
		mux.HandleFunc("/webhooks/stripe", limitWebhooks(webhookHandler("stripe", sa)))
	}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	// MCP server (AI-native surface): same admin-key auth as /api/v1.
	mux.Handle("/mcp", planGate(cloudPlan, plan.FeatMCP, apiKeysAuth(h.DB)(mcp.New(h.DB)).ServeHTTP))

	// Background tickers: maturation + reconciliation + postbacks + gamify.
	go ticker(ctx, 1*time.Minute, func() {
		if _, err := engine.Mature(ctx); err != nil {
			log.Printf("mature tick: %v [commissions may be lagging]", err)
		}
	})
	go ticker(ctx, 5*time.Minute, func() { fraudSweep(ctx, fraudEngine, h.DB, fraudDryRun) })
	go ticker(ctx, 1*time.Minute, func() {
		if _, err := reconciler.Tick(ctx); err != nil {
			log.Printf("reconciler tick: %v [daily_agg drift may persist]", err)
		}
	})
	go ticker(ctx, 5*time.Minute, func() { _ = gamifyEngine.TickActivity(ctx) })
	if mailer != nil {
		// The owner's "something needs you" email: checked every half hour,
		// sent at most once a day and only when something new is waiting.
		// Partners with a payout waiting who haven't said where to be paid
		// are asked by email, at most once a week.
		ownerTo, publicURL := ownerEmail(), os.Getenv("AE_PUBLIC_URL")
		go ticker(ctx, 30*time.Minute, func() {
			if _, err := partnermail.OwnerDigest(ctx, h.DB, mailer, publicURL, ownerTo, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "affiliate-engine: owner email: %v\n", err)
			}
			if _, err := partnermail.AskPayoutDetails(ctx, h.DB, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "affiliate-engine: payout-details emails: %v\n", err)
			}
			// The Monday report, tips for partners who haven't started,
			// and the monthly partner recap, each when it's due.
			if err := partnermail.ProgramEmails(ctx, h.DB, mailer, publicURL, ownerTo, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "affiliate-engine: program emails: %v\n", err)
			}
		})
	}
	// Postbacks are a plan feature: after a cloud downgrade, URLs already
	// set stop firing (queued events wait for an upgrade).
	postbackEmitter := postbacks.NewEmitter(h.DB)
	postbacksOn := cloudPlan.Allows(plan.FeatPostbacks)
	if postbacksOn {
		postbackEmitter.Start(ctx)
	}

	srv := &http.Server{
		// HSTS on HTTPS requests only (AE_HSTS=0 turns it off).
		Addr: cfg.Listen, Handler: httpapi.HSTS(demoGuard(mux, demo), cfg.TrustedProxies, httpapi.HSTSFromEnv()),
		// The redirect endpoint is public: a client that opens a
		// connection and stalls on headers must not pin a goroutine
		// (slowloris). ReadTimeout bounds the whole request (webhook
		// bodies are capped at 1 MiB, far inside 30s), WriteTimeout the
		// response, IdleTimeout parked keep-alive connections.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if version != "dev" {
			fmt.Printf("affiliate-engine: version %s\n", version)
		}
		// Never the DSN itself: it can carry a password.
		fmt.Printf("affiliate-engine: listening on %s (%s)\n", cfg.Listen, dsnSummary(cfg.Database))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
			stop()
		}
	}()

	<-ctx.Done()
	fmt.Println("affiliate-engine: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: shutdown: %v\n", err)
	}
	dispatcher.Stop()
	if postbacksOn {
		postbackEmitter.Stop() // Stop waits for the run loop: only if it was started
	}
	committer.Close()
	fmt.Println("affiliate-engine: stopped")
}

// planGate answers 402 for a route outside the workspace's cloud plan.
// A nil plan (self-host) serves everything.
func planGate(p *plan.Plan, feature string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.Allows(feature) {
			http.Error(w, p.Denied(feature), http.StatusPaymentRequired)
			return
		}
		next(w, r)
	}
}

// apiKeysAuth guards the MCP endpoint with real (hashed) admin API keys.
func apiKeysAuth(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			idn, err := apikeys.Authenticate(r.Context(), db, key)
			if err != nil || idn == nil || !idn.IsAdmin {
				http.Error(w, "admin key required", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func ticker(ctx context.Context, every time.Duration, fn func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// defaultFallbackURL is where /r/ sends a visitor whose link is dead or
// whose destination is not allowlisted, unless AE_FALLBACK_URL says otherwise.
const defaultFallbackURL = "https://affiliatetracking.co/"

// fallbackURLFromEnv validates AE_FALLBACK_URL: an absolute http(s) URL,
// or empty for the default. A bad value fails startup rather than sending
// every dead link somewhere unintended.
func fallbackURLFromEnv(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultFallbackURL, nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("AE_FALLBACK_URL must be an absolute http(s) URL, got %q", v)
	}
	return u.String(), nil
}

func allowedDestinations() []string {
	if s := os.Getenv("AE_ALLOWED_DEST"); s != "" {
		var out []string
		start := 0
		for i := 0; i <= len(s); i++ {
			if i == len(s) || s[i] == ';' {
				if i > start {
					out = append(out, s[start:i])
				}
				start = i + 1
			}
		}
		return out
	}
	return []string{"example.com"}
}

// fraudSweep re-evaluates affiliates active in the last hour: aggregate
// patterns (velocity, hijack, anomaly, ASN) emerge over events, not on the
// first one. dryRun (AE_FRAUD_DRYRUN) records verdicts without placing
// holds or freezing — the sweep used to enforce even in shadow mode.
func fraudSweep(ctx context.Context, fe *fraud.Engine, dbh *sql.DB, dryRun bool) {
	rows, err := dbh.QueryContext(ctx, `
		SELECT DISTINCT c.affiliate_id FROM commissions c
		WHERE c.created_at > ?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		log.Printf("fraud sweep: query: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		if _, err := fe.Evaluate(ctx, fraud.Query{AffiliateID: id, DryRun: dryRun, Now: time.Now().UTC()}); err != nil {
			log.Printf("fraud sweep: affiliate %d: %v", id, err)
		}
	}
}

// billingConfigured names the billing webhooks enabled by the environment,
// matching the conditions that mount each /webhooks/ route below.
func billingConfigured() []string {
	on := func(keys ...string) bool {
		for _, k := range keys {
			if os.Getenv(k) != "" {
				return true
			}
		}
		return false
	}
	var out []string
	if on("AE_STRIPE_SECRET", "AE_STRIPE_SECRET_KEY") {
		out = append(out, "stripe")
	}
	if on("AE_PADDLE_SECRET") {
		out = append(out, "paddle")
	}
	if on("AE_CHARGEBEE_USERNAME", "AE_CHARGEBEE_PASSWORD", "AE_CHARGEBEE_SECRET") {
		out = append(out, "chargebee")
	}
	if on("AE_LEMONSQUEEZY_SECRET") {
		out = append(out, "lemonsqueezy")
	}
	if on("AE_RECURLY_USERNAME", "AE_RECURLY_PASSWORD") {
		out = append(out, "recurly")
	}
	if on("AE_POLAR_SECRET") {
		out = append(out, "polar")
	}
	return out
}

// ownerEmail is where the owner's "something needs you" email goes by
// default: AE_OWNER_EMAIL, else the reply-to address partner emails
// already use (both are the owner's). Settings can change it.
func ownerEmail() string {
	if e := strings.TrimSpace(os.Getenv("AE_OWNER_EMAIL")); e != "" {
		return e
	}
	return strings.TrimSpace(os.Getenv("AE_MAIL_REPLY_TO"))
}

// siteURL is the owner's website for "Test your setup": the redirect
// fallback, unless it's still our own default.
func siteURL(fallback string) string {
	if fallback == defaultFallbackURL {
		return ""
	}
	return fallback
}
