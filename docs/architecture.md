# Architecture, performance and testing

## How it works

- **Click tracking**: `/r/{affiliate}` redirect with signed attribution tokens (HMAC), open-redirect allowlist, bot classification (fail-open, stored but excluded from stats)
- **Attribution ladder** (server-side, cookie is one channel never the authority): coupon → lifetime customer binding → signed token within window → flagged IP fallback
- **Money correctness**: hybrid ledger: single-entry commissions as domain facts + double-entry journal as money authority. Replay-safe (idempotent on `(source, external_event_id)`), concurrency-safe, immutable money rows, gapless journal ids. 15 property tests pin the invariants
- **Billing adapters**: Stripe, Paddle, Chargebee (signature verify-before-write, replay-safe) → normalized billing-event stream with buyer-email enrichment
- **Fraud engine**: 13 rules: self-referral (gmail alias normalization + payment fingerprint), click/conversion velocity, coupon-hijack, conversion-time anomaly (diurnal z-test), ASN/datacenter reputation, IP overlap, duplicate-account, bot-class gating, geo-mismatch, IP-fallback anomaly, inactive affiliate. Screening runs at the conversion boundary (before credit) + 5-min sweep; `AE_FRAUD_DRYRUN=1` shadow mode. Verdicts bind to money via payment-blocking holds; fraud review never mutates the commission state machine
- **Multi-tier/MLM**: sponsor/placement trees, closure table, tiered periods (30/10/60 vectors), binary pairing with caps and carry-forward
- **Usage-based commissions**: metered/credit events with delta settlement (the AI-SaaS case)
- **Gamification**: money-derived XP, quadratic levels, milestones/streaks/traffic badges, public leaderboard
- **White-label**: per-program portal branding (accent/logo), custom portal domains, embeddable `/widget.js` launcher (FR-33)
- **Payouts**: batch by affiliate + threshold, whole-commission coverage, retryable execution, PayPal/Wise CSV export (formula-injection safe, per-currency decimals); a Stripe Connect transfer executor is built but not switched on yet, so you send the money and approve the payout
- **Analytics**: on-write aggregates + watermark reconciler with drift self-heal; dashboards read pre-aggregated tables; token-dimension drill (utm_source/subid/placement)
- **Surfaces**: embedded UI (admin + portal shell), JSON API with scoped API keys (`admin`, `affiliate:<id>`), MCP server (read-only analytics + program digest)
- **Ops**: `doctor` (6-check integrity report), `backup`/`restore`, `import` (migrate from incumbent CSV exports), `keygen`, `test-conversion`, `screen-backfill` (fraud-screen commissions credited by releases that never screened them); retention job (90d raw clicks, aggregates kept); append-only audit log

## Code layout

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

Measured on an Apple M2 (file-backed SQLite, CGO-free build). Reproduce with `make bench` and `cmd/loadtest`. Every number below is from a real run, never projected.

| Path | Result | Requirement |
|---|---|---|
| Click pipeline (enqueue → group-commit → durable, ≤500/batch, attribution rows in-tx) | **19,613 clicks/s** | ≥1,000 (W1 gate) |
| Redirect endpoint (end-to-end HTTP load test) | **10,844 rps, 0 errors** | n/a |
| Admin dashboard aggregate query (30d × 10 affiliates, pre-aggregated tables) | **~51 µs/op** | NFR-3 |
| Token-dimension rollup (watermark incremental) | **~293 µs/op** | n/a |

PRD NFR-2 asks ≥5,000 clicks/min on one VPS core; the measured sustained ingest is ~235× that with a single-writer committer, leaving the read path untouched.

## Testing

~277 tests across 31 packages, all green under `-race`, dual-engine (file-backed SQLite + PostgreSQL 18 in CI). Money properties (15), adversarial attack suite (15, dual-engine), commission lifecycle, 3 billing-adapter corpora, attribution precedence, redirect guards, click pipeline, fraud rules (12), branding/widget XSS corpus, e2e M1 gate (click → conversion → lifetime renewal → maturation → payout → balanced ledger → replay-safe).

```bash
make test        # full suite
make test-race   # race detector
make build       # binary
```

[Back to the README](../README.md)
