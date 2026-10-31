# Affiliate-Engine

Self-hostable affiliate tracking, built fraud-first: every commission exactly-once, attributable, and auditable by the affiliate.

One Go binary. Embedded SQLite (Postgres-ready dual dialect). Zero runtime dependencies. Free and open source under the [AGPL-3.0](LICENSE). If you would rather we run it for you, see [Enterprise](https://affiliatetracking.co/pricing). This repository is the core engine: the Shopify app and the WordPress plugin are separate products with their own pricing.

## What it does

- **Click tracking**: `/r/{affiliate}` redirect with signed attribution tokens (HMAC), open-redirect allowlist, bot classification (fail-open, stored but excluded from stats)
- **Attribution ladder** (server-side, cookie is one channel never the authority): coupon → lifetime customer binding → signed token within window → flagged IP fallback
- **Money correctness**: hybrid ledger — single-entry commissions as domain facts + double-entry journal as money authority. Replay-safe (idempotent on `(source, external_event_id)`), concurrency-safe, immutable money rows, gapless journal ids. 15 property tests pin the invariants
- **Billing adapters**: Stripe, Paddle, Chargebee (signature verify-before-write, replay-safe) → normalized billing-event stream with buyer-email enrichment
- **Fraud engine**: 12 rules — self-referral (gmail alias normalization + payment fingerprint), click/conversion velocity, coupon-hijack, conversion-time anomaly (diurnal z-test), ASN/datacenter reputation, IP overlap, duplicate-account, bot-class gating, geo-mismatch, IP-fallback anomaly. Screening runs at the conversion boundary (before credit) + 5-min sweep; `AE_FRAUD_DRYRUN=1` shadow mode. Verdicts bind to money via payment-blocking holds — fraud review never mutates the commission state machine
- **Multi-tier/MLM**: sponsor/placement trees, closure table, tiered periods (30/10/60 vectors), binary pairing with caps and carry-forward
- **Usage-based commissions**: metered/credit events with delta settlement (the AI-SaaS case)
- **Gamification**: money-derived XP, quadratic levels, milestones/streaks/traffic badges, public leaderboard
- **White-label**: per-program portal branding (accent/logo), custom portal domains, embeddable `/widget.js` launcher (FR-33)
- **Payouts**: batch by affiliate + threshold, whole-commission coverage, retryable execution, PayPal/Wise CSV export (formula-injection safe, per-currency decimals); a Stripe Connect transfer executor is built but not switched on yet, so you send the money and approve the payout
- **Analytics**: on-write aggregates + watermark reconciler with drift self-heal; dashboards read pre-aggregated tables; token-dimension drill (utm_source/subid/placement)
- **Surfaces**: embedded UI (admin + portal shell), JSON API with scoped API keys (`admin`, `affiliate:<id>`), MCP server (read-only analytics + program digest)
- **Ops**: `doctor` (6-check integrity report), `backup`/`restore`, `import` (migrate from incumbent CSV exports), `keygen`, `test-conversion`, `screen-backfill` (fraud-screen commissions credited by releases that never screened them); retention job (90d raw clicks, aggregates kept); append-only audit log

## Quickstart

```bash
# build (CGO-free, cross-compilable)
go build -o affiliate-engine ./cmd/affiliate-engine

# run (creates + migrates sqlite db, generates signing secret)
./affiliate-engine -listen :8080 -db sqlite:affiliate.db

# create an admin API key
./affiliate-engine -db sqlite:affiliate.db keygen --scope admin --name ops
# -> ae_... (shown once; stored as SHA-256 hash)

# integrity check
./affiliate-engine -db sqlite:affiliate.db doctor

# backup
./affiliate-engine -db sqlite:affiliate.db backup /path/to/snapshot.db

# ad-hoc analytics export (parquet for DuckDB — FR-26)
./affiliate-engine -db sqlite:affiliate.db export-parquet -out /path/to/dir -verify
```

Redirect traffic to `http://your-host:8080/r/{affiliate_id}?dest=https://shop.example.com/offer`.

Configure via environment:

| Variable | Purpose | Default |
|---|---|---|
| `AE_TOKEN_SECRET` | attribution signing secret (≥32 bytes) | generated + persisted |
| `AE_ALLOWED_DEST` | redirect destination allowlist (`;`-separated suffixes) | `example.com` |
| `AE_STRIPE_SECRET` | Stripe webhook signing secret (`whsec_...`) | unset (webhook route disabled) |
| `AE_STRIPE_PRODUCTS` | Only sales of these Stripe products, prices or payment links earn commission (`prod_`, `price_`, `plink_` ids, comma separated). For a Stripe account that sells other things too | unset (every sale counts) |
| `AE_STRIPE_API_KEY` | Restricted Stripe key (Checkout Sessions: read) so the engine can look up what a one-time checkout sold, which Stripe leaves out of webhooks. Needed with `AE_STRIPE_PRODUCTS` unless every sale uses a listed payment link | unset |
| `AE_PADDLE_SECRET` | Paddle webhook signing secret | unset (route disabled) |
| `AE_CHARGEBEE_SECRET` | Chargebee webhook signing secret | unset (route disabled) |
| `AE_LEMONSQUEEZY_SECRET` | Lemon Squeezy webhook signing secret | unset (route disabled) |
| `AE_POLAR_SECRET` | Polar webhook secret (`whsec_...`) | unset (route disabled) |
| `AE_RECURLY_USERNAME` | Recurly Basic Auth username (either credential enables the route) | unset (route disabled) |
| `AE_RECURLY_PASSWORD` | Recurly Basic Auth password | unset |
| `AE_RECURLY_SIGNATURE_SECRET` | Optional recurly-signature secret (JSON-mode endpoints) | unset (XML mode) |
| `AE_STRIPE_SECRET_KEY` | Stripe API key (Connect payout transfers) | unset |
| `AE_PUBLIC_URL` | external origin for widget/portal absolute links | request origin |
| `AE_MAIL_PROVIDER` | `postmark` or `resend`: partner emails (sign-in links, payout notes); `log` prints them. Needs `AE_PUBLIC_URL` | unset (off) |
| `AE_MAIL_KEY` | Postmark server token or Resend API key | unset |
| `AE_MAIL_FROM` | sending address the provider has verified; the program name is shown in front of it | unset |
| `AE_MAIL_REPLY_TO` | where partner replies go | the From address |
| `AE_OWNER_EMAIL` | where the owner's daily "something needs you" email goes (Settings can change it) | `AE_MAIL_REPLY_TO` |
| `AE_FRAUD_DRYRUN` | `1` = fraud rules score-only, no enforcement | unset (enforce) |
| `AE_IP_MODE` | `trunc24` = write-time IP anonymization (/24 v4, /48 v6 before hashing; NFR-7) | `off` (full precision) |
| `AE_CLICK_RETENTION_DAYS` | raw-click retention before the sweep prunes | `90` |
| `AE_COOKIE_SECURE` | `1` = set Secure on attribution cookies | auto (https) |

## Connect your site

Add one line to every page of your site:

```html
<script src="https://acme.affiliatetracking.co/t.js" async></script>
```

`/r/{code}` sends the visitor to your site with `?aff_token=...`. The script keeps that token in a first-party cookie (`at_token`) and removes it from the address bar. Your site's domain must be in `AE_ALLOWED_DEST`, or visitors land on the fallback URL with no token.

**Stripe Payment Links, Pricing Table, Buy Button.** Nothing else to do. The script passes the token to `buy.stripe.com` links, `<stripe-pricing-table>` and `<stripe-buy-button>` as `client_reference_id` (put the script above the table or button). One-time and subscription checkouts are both credited, trials on their first paid invoice. A `client_reference_id` you set yourself is left alone, and the engine ignores any value that does not start with `at1_`.

**Your own Checkout Sessions.** Read the `at_token` cookie on your server and pass it along:

- one-time payment (`mode=payment`): `metadata[aff_token]`
- subscription (`mode=subscription`): `subscription_data[metadata][aff_token]`, so every invoice carries it. The session's `metadata[aff_token]` works too: the engine then ties the Stripe customer to the affiliate at checkout.

In the browser, `AffiliateTracking.token()` returns the token and `AffiliateTracking.clientReferenceId()` its `client_reference_id` form.

**Switching from Rewardful, FirstPromoter, Tolt or Tapfiliate.** Review first, then import your affiliates in one batch. Codes carry over, so `/r/{code}` works at once:

```
affiliate-engine -db sqlite:ae.db import -from rewardful -csv affiliates.csv -referrals referrals.csv -commissions commissions.csv -dry-run
affiliate-engine -db sqlite:ae.db import -from rewardful -csv affiliates.csv -referrals referrals.csv -commissions commissions.csv
affiliate-engine -db sqlite:ae.db import -undo 1      # one command, exactly what batch 1 created
affiliate-engine -db sqlite:ae.db compare -from rewardful -commissions commissions.csv -referrals referrals.csv -date 2026-09-28
```

- The dry run writes nothing. It counts affiliates, links, coupons, referrals and commissions, lists conflicts (duplicate emails, codes or coupons already taken, unknown currencies, unreadable amounts or dates, rows naming an unknown affiliate) and runs the fraud rules the export can support over their history (self-referral, duplicate accounts, payment fingerprint, conversion velocity and time of day, coupon hijack, inactive affiliates). The click-level rules can't run on a rival export (no IPs, user agents or countries in it) and the report says so.
- Referrals that carry a billing customer id (`stripe_customer_id`) become customer bindings, so renewals from customers an affiliate already brought keep earning here. Commissions are read for the review and the compare only. They are never imported: balances stay with the old tracker, settle them there.
- Re-running the same export changes nothing and records no batch. A changed export imports only what's new, as a new batch. `import -list` shows the batches.
- `import -undo <id>` removes the batch's bindings, coupons, codes, affiliates and their empty payable accounts in one transaction. It refuses, and changes nothing, once money moved against them (a commission, payout or ledger entry for an imported affiliate, or a sale credited through an imported binding or coupon), or when a later batch or new traffic hangs off them. The import posts nothing to the ledger, so there is never anything to reverse; money earned after it is real, so freeze the affiliate instead.
- `compare` puts the old tracker's totals for a day (or `-since`/`-until`) next to ours: clicks (with `-clicks`), new paying customers, commission count and amount per currency, and the difference. While both run, pay each commission from one system only.

Then tell the script which parameter your old links use:

```html
<script src="https://acme.affiliatetracking.co/t.js" data-param="via" async></script>
```

Rewardful uses `via`, FirstPromoter `fpr`, Tolt and Tapfiliate `ref` (comma-separate several). A visit to `yoursite.com/?via=jane` passes through `/r/jane` once, records the click, and comes back with the token.

## Endpoints

| Route | Auth | Purpose |
|---|---|---|
| `GET /r/{code}` | — | click capture + redirect |
| `POST /webhooks/stripe` | Stripe signature | billing events |
| `POST /webhooks/paddle` | Paddle signature | billing events |
| `POST /webhooks/chargebee` | Chargebee signature | billing events |
| `GET /api/v1/admin/dashboard` | admin key | program-wide stats |
| `GET/POST /api/v1/admin/campaigns` | admin key | program config (incl. attribution window, NET terms) |
| `PATCH /api/v1/admin/campaigns/{id}` | admin key | change a program's hold (`net_terms_days`) or attribution window |
| `GET/PUT /api/v1/admin/settings` | admin key | payout settings: `min_payout_cents` (default 5000 = $50), `default_hold_days` (default 30) |
| `GET/PUT /api/v1/admin/branding` | admin key | per-program white-label branding |
| `PUT/DELETE /api/v1/admin/branding/logo` | admin key | upload (raw PNG/JPEG/WebP/GIF body, ≤256 KB) or remove the program logo |
| `POST /api/v1/admin/payouts/run` | admin key | payout batch (dry-run supported; threshold defaults to the program minimum) |
| `GET /api/v1/admin/payouts?status=requested` | admin key | payouts awaiting approval (`all` for every status) |
| `POST /api/v1/admin/payouts/approve` | admin key | execute one payout (idempotent: approving a paid payout is a no-op 200) |
| `GET /api/v1/admin/payouts/export/paypal.csv` / `wise.csv` | admin key | staged payouts of that method that pass Approve's checks, as a PayPal or Wise bulk-pay CSV; each payout is claimed and listed once |
| `GET/POST /api/v1/admin/taxdocs` | admin key | tax form status (pending, received, validated, rejected, not_required) |
| `GET /api/v1/admin/taxdocs/file?id=N` | admin key | download an uploaded tax form (attachment) |
| `GET/POST /api/v1/affiliate/taxdocs` | portal session or affiliate key | own tax forms / upload a W-9, W-8BEN or W-8BEN-E PDF |
| `GET /api/v1/admin/fraud/queue` + `POST .../resolve` | admin key | fraud verdicts (release \| uphold) |
| `GET /api/v1/affiliate/earnings` | affiliate/admin key | portal earnings |
| `GET /api/v1/affiliate/ledger` | portal session or affiliate/admin key | auditable ledger view (Σ=0 proof) |
| `GET /api/v1/affiliate/payouts` | affiliate key or portal session | the affiliate's own payouts, newest first |
| `GET/PUT /api/v1/affiliate/payout-details` | affiliate key or portal session | where the affiliate wants to be paid (PayPal, Wise or bank; sealed at rest) |
| `GET /api/v1/portal/brand` | — | public branding for a domain/campaign |
| `GET /api/v1/portal/logo` | — | the program's uploaded logo (versioned, cached) |
| `POST /api/v1/admin/partners/message` | admin key | email program news to every active partner who hasn't unsubscribed (`{subject, body}`); needs `AE_MAIL_PROVIDER` |
| `GET/POST /api/v1/portal/unsubscribe?t=` | — | the signed unsubscribe link in each news email: GET asks, POST opts out |
| `POST /api/v1/portal/login-link` | — | email an active affiliate a one-time sign-in link (1 hour); same answer for any address; needs `AE_MAIL_PROVIDER` |
| `GET /widget.js` | — | embeddable launcher script |
| `GET /t.js` | — | first-party tracking script ([Connect your site](#connect-your-site)) |
| `GET /healthz` | — | liveness |
| `/`, `/admin`, `/portal` | — | embedded UI shell |

## Payouts

- **Hold.** A commission matures after its program's `net_terms_days` (0 = no hold). New programs get `default_hold_days` from `/admin/settings` (default 30). Changing a program's hold applies to new commissions only.
- **Minimum.** A payout run skips an affiliate whose net balance in a currency is below `min_payout_cents` (default 5000, in minor units of that currency). Pass `min_threshold_cents` on a run to override it once.
- **Tax forms.** Affiliates upload a W-9, W-8BEN or W-8BEN-E PDF from the portal (5 MB max, checked to be a PDF). The status becomes `pending`, whatever it was before, so it blocks payouts until the operator reviews it. The file is sealed with AES-256-GCM under a key derived from the deployment secret and stored in the database. It is never served from `/static/`; only the admin download returns it, as an attachment. The admin then validates or rejects it (or marks it received); only the admin can move a form out of `pending`. A `pending` or `rejected` form blocks batching and approval for that affiliate. GDPR erase deletes the files.
- **Paying.** Admin console: run the batch, download the PayPal or Wise CSV, pay on the rail, then click Approve on each payout to settle the ledger. Approve is safe to click twice.

## Privacy & GDPR (NFR-7)

- **Write-time IP anonymization** — `AE_IP_MODE=trunc24` truncates IPv4 to /24 and IPv6 to /48 *before* the identity hash is computed; the raw IP is never stored anywhere (only salt-free hashes). Unique counting and IP-velocity fraud rules degrade to subnet granularity — a deliberate, documented trade. Unknown `AE_IP_MODE` values fail startup loudly.
- **Subject export (Art. 15/20)** — `affiliate-engine gdpr-export -affiliate N [-out file.json]` emits the machine-readable bundle (identity, clicks, attributions, customers, commissions, payouts, codes, coupons, badges, XP, tax docs). Output file is mode 600 — it is subject PII.
- **Right to erasure (Art. 17)** — `affiliate-engine gdpr-erase -affiliate N -yes` scrubs identity in place and revokes access (sessions, grants, affiliate API keys); booked commissions, payouts, ledger journals, aggregates and MLM structure remain under the pseudonymous id (accounting retention — the books stay balanced and Σ=0 proofs keep verifying). Idempotent; every erasure lands in the append-only audit log.

## Architecture

```
cmd/affiliate-engine     entrypoint + CLI (doctor/backup/restore/keygen/import/gdpr-*/screen-backfill)
internal/
  db          engine opener (WAL pragmas, dedicated ledger conn)
  migrations  dual-dialect DDL (sqlite + postgres)
  ledger      double-entry poster + invariants
  commissions purchase/replay/mature/clawback engine
  clicks      group-commit committer (batch tx: raw+agg+uniq)
  attribution coupon > customer > token > flagged-IP ladder
  tokens      HMAC-SHA256 signed attribution tokens
  fingerprint identity hashing + bot classification
  billing     Stripe/Paddle/Chargebee adapters (verify-before-write)
  payouts     batch + approve + retry lifecycle
  fraud       12 rules + payment-blocking holds
  screening   conversion-boundary rule evaluation + sweep
  mlm         closure trees, tiered periods, binary pairing
  usage       metered ingest + delta settlement
  gamify      XP/levels/badges/leaderboard
  importer    incumbent CSV migration (review, batches, undo, side-by-side compare)
  analytics   watermark reconciler + dashboards
  outbox      at-least-once event dispatch
  postbacks   S2S emitter (SSRF-guarded, retries)
  export      PayPal/Wise CSV
  apikeys     scoped hashed keys
  mcp         read-only MCP server (analytics + digest)
  httpapi     redirect + JSON API + branding/widget
  privacy     NFR-7: IP anonymization + GDPR export/erase
  ui          embedded single-page shell
```

## Performance

Measured on an Apple M2 (file-backed SQLite, CGO-free build). Reproduce with `make bench` and `cmd/loadtest` — every number below is from a real run, never projected.

| Path | Result | Requirement |
|---|---|---|
| Click pipeline (enqueue → group-commit → durable, ≤500/batch, attribution rows in-tx) | **19,613 clicks/s** | ≥1,000 (W1 gate) |
| Redirect endpoint (end-to-end HTTP load test) | **10,844 rps, 0 errors** | — |
| Admin dashboard aggregate query (30d × 10 affiliates, pre-aggregated tables) | **~51 µs/op** | NFR-3 |
| Token-dimension rollup (watermark incremental) | **~293 µs/op** | — |

PRD NFR-2 asks ≥5,000 clicks/min on one VPS core; the measured sustained ingest is ~235× that with a single-writer committer, leaving the read path untouched.

## Testing

~277 tests across 31 packages, all green under `-race`, dual-engine (file-backed SQLite + PostgreSQL 18 in CI). Money properties (15), adversarial attack suite (15, dual-engine), commission lifecycle, 3 billing-adapter corpora, attribution precedence, redirect guards, click pipeline, fraud rules (12), branding/widget XSS corpus, e2e M1 gate (click → conversion → lifetime renewal → maturation → payout → balanced ledger → replay-safe).

```bash
make test        # full suite
make test-race   # race detector
make build       # binary
```

## License

GNU Affero General Public License, version 3: see [LICENSE](LICENSE), or the plain-words version at https://affiliatetracking.co/license. Copyright (c) 2026 SAAS SEO AGENCY LLC.

Run it and change it freely. If you offer it to others as a hosted service, publish your changes under the same license. There is no license key. With `AE_PLAN` unset, every feature is on.
