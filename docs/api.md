# HTTP endpoints

Admin routes need an admin API key (`affiliate-engine keygen --scope admin`). Affiliate routes take an affiliate key or a portal session.

| Route | Auth | Purpose |
|---|---|---|
| `GET /r/{code}` | none | click capture + redirect |
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
| `GET/PUT /api/v1/affiliate/payout-details` | GET: affiliate key or portal session; PUT: portal session only | where the affiliate wants to be paid (PayPal, Wise or bank; sealed at rest). A change to existing details holds the affiliate's payouts for 72 hours and emails them |
| `GET /api/v1/portal/brand` | none | public branding for a domain/campaign |
| `GET /api/v1/portal/logo` | none | the program's uploaded logo (versioned, cached) |
| `POST /api/v1/admin/partners/message` | admin key | email program news to every active partner who hasn't unsubscribed (`{subject, body}`); needs `AE_MAIL_PROVIDER` |
| `GET/POST /api/v1/portal/unsubscribe?t=` | none | the signed unsubscribe link in each news email: GET asks, POST opts out |
| `POST /api/v1/portal/login-link` | none | email an active affiliate a one-time sign-in link (1 hour); same answer for any address; needs `AE_MAIL_PROVIDER` |
| `GET /widget.js` | none | embeddable launcher script |
| `GET /t.js` | none | first-party tracking script ([Connect your site](connect-your-site.md)) |
| `GET /healthz` | none | liveness |
| `/`, `/admin`, `/portal` | none | embedded UI shell |

[Back to the README](../README.md)
