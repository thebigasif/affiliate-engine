# Configuration

AffiliateTracking reads its settings from environment variables. Every one is optional: with none set, the engine runs with SQLite, generates its own signing secret and turns on every feature.

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

For a full server setup, see [DEPLOY.md](../DEPLOY.md).

[Back to the README](../README.md)
