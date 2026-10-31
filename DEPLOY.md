# Self-hosting affiliate-engine

One binary, one database file, zero runtime dependencies. CGO-free — the same binary runs on linux/amd64, darwin/arm64, windows/amd64.

## 1. Get the binary

    go build -o affiliate-engine ./cmd/affiliate-engine
    # or: docker build -t affiliate-engine . && docker run -p 8080:8080 -v ae-data:/data affiliate-engine

Prebuilt artifacts: every `v*` tag publishes verified per-architecture
binaries + checksums under
[Releases](https://github.com/thebigasif/affiliate-tracking/releases)
and a container image at `ghcr.io/thebigasif/affiliate-tracking:<tag>`
(each release pulls and runs its own image as a smoke test).

The image runs as the distroless `nonroot` user (uid 65532); `/data` is
created owned by that uid with mode 0700, so a fresh named volume just
works. A bind-mounted host directory must be writable by uid 65532
(`chown 65532:65532 ./data`).

## 2. First run

    ./affiliate-engine -listen :8080 -db sqlite:affiliate-engine.db

What happens: the database is created and migrated; unless
`AE_TOKEN_SECRET` is set, an attribution signing secret is generated and
persisted **next to the SQLite database** as `<db path>.secret` (mode 0600;
for the default db that is `affiliate-engine.db.secret`, the same name as
before). `AE_SECRET_FILE` overrides the location; with Postgres the default
is `/data/affiliate-engine.secret`. A legacy `./affiliate-engine.db.secret`
in the working directory is still picked up. Back this file up with the
database — losing it invalidates every outstanding attribution cookie.

To supply your own secret, generate at least 32 random bytes:

    export AE_TOKEN_SECRET="$(openssl rand -hex 32)"

Placeholders (anything containing `change-me`) and short values are refused
at startup. The process runs with umask 077 on unix, so every file it
creates (db, WAL, backups, secret) is owner-only.

## 3. Configure

| Env var | Purpose | Default |
|---|---|---|
| `AE_TOKEN_SECRET` | attribution token secret (>=32 bytes after hex/base64 decoding; placeholders refused) | generated + persisted |
| `AE_SECRET_FILE` | where the generated secret is stored | `<sqlite db>.secret`, or `/data/affiliate-engine.secret` for Postgres |
| `AE_ALLOWED_DEST` | redirect destination allowlist (`;`-separated suffixes) | `example.com` |
| `AE_FALLBACK_URL` | where `/r/` sends a visitor whose link is unknown or whose destination is not allowlisted (absolute http(s) URL; anything else fails startup) | `https://affiliatetracking.co/` |
| `AE_STRIPE_SECRET` | Stripe webhook signing secret | route disabled |
| `AE_STRIPE_PRODUCTS` | Only sales of these Stripe products, prices or payment links earn commission (`prod_`, `price_`, `plink_` ids, comma separated). For a Stripe account that sells other things too | unset (every sale counts) |
| `AE_STRIPE_API_KEY` | Restricted Stripe key (Checkout Sessions: read) so the engine can look up what a one-time checkout sold, which Stripe leaves out of webhooks. Needed with `AE_STRIPE_PRODUCTS` unless every sale uses a listed payment link | unset |
| `AE_PADDLE_SECRET` | Paddle webhook signing secret | route disabled |
| `AE_CHARGEBEE_SECRET` | Chargebee webhook signing secret | route disabled |
| `AE_LEMONSQUEEZY_SECRET` | Lemon Squeezy webhook signing secret | route disabled |
| `AE_POLAR_SECRET` | Polar webhook secret (`whsec_...`) | route disabled |
| `AE_RECURLY_USERNAME` | Recurly Basic Auth username (either credential enables the route) | route disabled |
| `AE_RECURLY_PASSWORD` | Recurly Basic Auth password | unset |
| `AE_RECURLY_SIGNATURE_SECRET` | Optional recurly-signature secret | unset (XML mode) |
| `AE_RECURLY_API_KEY` | Read-only Recurly API key. A first sale (and a new account or trial) reads the account's `aff_token` custom field with it, so the affiliate is found | unset (renewals of bound accounts only) |
| `AE_RECURLY_API_BASE` | Recurly API host; `https://v3.eu.recurly.com` for EU sites. Only these two hosts are allowed | `https://v3.recurly.com` |
| `AE_STRIPE_SECRET_KEY` | Stripe API key (Connect payout transfers) | unset |
| `AE_PUBLIC_URL` | external origin (widget + portal absolute links); an `https://` value also makes cookies `Secure` | request origin |
| `AE_MAIL_PROVIDER` | `postmark` or `resend` turns on partner emails (see below); `log` prints them instead. Needs `AE_PUBLIC_URL` | unset (off) |
| `AE_MAIL_KEY` | Postmark server token or Resend API key | unset |
| `AE_MAIL_FROM` | sending address your provider has verified, e.g. `partners@yourdomain.com` | unset |
| `AE_MAIL_REPLY_TO` | where partner replies go | the From address |
| `AE_OWNER_EMAIL` | where the owner's daily "something needs you" email goes (Settings can change it) | `AE_MAIL_REPLY_TO` |
| `AE_COOKIE_SECURE` | `1` = always mark attribution + portal cookies `Secure` (TLS requests get it anyway) | unset |
| `AE_TRUSTED_PROXIES` | comma-separated CIDRs/IPs of your reverse proxies / CDN edges; only these peers may set `X-Forwarded-For`, `X-Forwarded-Proto` and geo headers (`Cf-Ipcountry`, ...) | unset (trust nobody) |
| `AE_REDIRECT_RATE_PER_MIN` | `/r/` requests per client IP per minute before `429`; `-1` disables (load tests) | `120` |
| `AE_MIGRATE_DATABASE_URL` | separate DSN (same engine) used only to run migrations at startup — see the two-role Postgres setup | unset (migrate with `-db`) |
| `AE_FRAUD_DRYRUN` | `1` = fraud rules score-only shadow mode (request path and the 5-minute sweep) | unset (enforce) |
| `AE_IP_MODE` | `trunc24` = anonymize IPs at write time (/24 v4, /48 v6 before hashing); unknown values fail startup | `off` |
| `AE_CLICK_RETENTION_DAYS` | raw-click retention window | `90` |
| `AE_DEMO` | `1` = public read-only demo over a sample program that reseeds itself daily; see [Live demo](#live-demo-ae_demo1) | unset |

### Behind a reverse proxy / CDN

Without `AE_TRUSTED_PROXIES` the engine uses the TCP peer as the client IP
and ignores forwarding headers — behind a proxy that means every visitor
shares the proxy's IP (and its rate-limit bucket). Set it to the proxy's
address range: the client is then the rightmost `X-Forwarded-For` hop that
is not itself a trusted proxy, which a visitor cannot forge.

## 4. Create an admin key

    ./affiliate-engine -db sqlite:affiliate-engine.db keygen --scope admin --name ops

No shell? The Docker image is distroless, so a platform terminal (Coolify,
Railway) can't run `keygen`. Set `AE_BOOTSTRAP_ADMIN_KEY=<a name>`
(for example `setup-1`) and redeploy: on start the engine creates one admin
key with that name and prints it once in the log. A restart with the same
name prints nothing. Copy the key, then remove the variable.

Then open the admin screen, paste the key, and set the commission rate in
**Program settings**. A sale earns nothing until its program has a rate.

**Partner emails (optional).** With `AE_MAIL_PROVIDER`, `AE_MAIL_KEY`,
`AE_MAIL_FROM` and `AE_PUBLIC_URL` set, the engine emails affiliates from your
own Postmark or Resend account: a sign-in link when you approve them, a new
link when they ask for one on the sign-in page (1 hour, at most one per
address every 5 minutes), and a note when you approve their payout. Emails go
out through the outbox, so a provider outage retries instead of losing them.
The start-up log says `partner emails on (resend)` when it's working.

The key is shown ONCE (stored as a SHA-256 hash). Use it as
`Authorization: Bearer ***` on `/api/v1/*` and `/mcp`. Repeated bad keys
from one client IP are throttled (HTTP 429) before any database lookup.

## 5. Point traffic

    https://your-host/r/{affiliate_id}?dest=https://shop.example/offer

Destinations are allowlisted (`AE_ALLOWED_DEST`); subdomains pass. Each
client IP gets `AE_REDIRECT_RATE_PER_MIN` redirects per minute (default
120); beyond that `/r/` answers `429 Too Many Requests` with
`Retry-After: 60`. Run load tests (`cmd/loadtest`) with
`AE_REDIRECT_RATE_PER_MIN=-1`.

## 6. Wire billing

Stripe dashboard -> Webhooks -> `https://your-host/webhooks/stripe`, copy
the signing secret into `AE_STRIPE_SECRET`. Same pattern for Paddle at
`/webhooks/paddle`. Rejected deliveries answer a bare `400 rejected`; the
reason is in the engine's log.

## 7. Operate

    ./affiliate-engine -db sqlite:affiliate-engine.db doctor     # 6-check integrity report
    ./affiliate-engine -db sqlite:affiliate-engine.db backup     # VACUUM INTO snapshot
    ./affiliate-engine -db sqlite:affiliate-engine.db restore -from snapshot.db  # verify + atomic swap (rollback copy kept)

Cron both: doctor for drift alerts, backup nightly. Backups are the
compacted db — restore verifies the snapshot, re-applies the migration
chain so the restored file carries the current guard triggers (a snapshot
with dropped or neutered triggers is healed, not trusted), swaps it in
atomically, and preserves the previous file as a rollback copy.

`doctor`, `backup`, `restore` and `test-conversion` are SQLite-only; with a
Postgres DSN they refuse to run — use `pg_dump`/`psql` against the server.

A retention sweep prunes raw clicks older than `AE_CLICK_RETENTION_DAYS`
(default 90) while keeping all aggregates — dashboards are unaffected.

### Payouts and tax forms

- Payout defaults live in `/api/v1/admin/settings` (Admin console,
  "Payout settings"): minimum payout $50 (`min_payout_cents: 5000`) and a
  30-day default hold for new programs. A program's own hold is its
  `net_terms_days` (`PATCH /api/v1/admin/campaigns/{id}`).
- How long a customer earns: a program's `commission_months`
  (`PATCH /api/v1/admin/campaigns/{id}` with `{"commission_months":12}`;
  0, the default, means for the customer's lifetime). The months count
  from when the customer was bound to the affiliate (their signup through
  the link; for imported customers, the import). Later payments and usage
  earn nothing and are recorded as `commission_period_ended`. A change
  applies to every later payment, including existing customers.
- Manual rails: run the batch, click "Download PayPal CSV" or "Download
  Wise CSV", pay on the rail, then "Approve" each payout. Approving twice
  is a no-op.
- Tax forms uploaded from the portal are stored in the database, sealed
  with a key derived from the deployment secret (`AE_TOKEN_SECRET` or the
  generated secret file). Back the secret up with the database: **if the
  secret is lost or rotated, uploaded forms can no longer be opened**
  (affiliates re-upload; statuses and the payout gate are unaffected).
  Uploads are capped at 5 MB, so a large program adds a few MB per
  affiliate to the database and its backups.

Admin list endpoints (`/api/v1/admin/affiliates`, campaigns, codes,
taxdocs, payouts, rules, tiers) return at most `?limit=` rows (default 200, max
1000). Keyset-paged lists include `next_after_id`; pass it back as
`?after_id=` for the next page.

### Ad-hoc analytics (parquet + DuckDB)

Pull clicks + aggregates out for analysis without touching the serving
database (read-only SELECTs; both engines):

    ./affiliate-engine -db sqlite:affiliate-engine.db export-parquet -out analytics-snapshot -verify
    # bounded: -from/-to YYYYMMDD filter the clicks file only

`-verify` reads both files back and cross-checks row counts and the
click totals against SQL — the snapshot an analyst loads provably
matches the database it came from. Then query with DuckDB (read-only
sidecar, never the live db):

    duckdb -c "SELECT day, sum(clicks) FROM read_parquet('analytics-snapshot/daily_agg.parquet') GROUP BY day ORDER BY day;"
    duckdb -c "SELECT country, count(*) FROM read_parquet('analytics-snapshot/clicks.parquet') WHERE bot_class=0 GROUP BY country;"

`ip_hash`/`ua_hash` travel as opaque 16-byte pseudonyms (the same
non-reversible identity hashes the product stores) — they make
velocity/fraud-pattern analysis possible without exposing raw IPs.

Migrating from an incumbent (Rewardful/FirstPromoter/Tapfiliate/Tolt)?
Export the affiliates CSV (and the referrals and commissions CSVs if you
have them), then review, import, and keep the undo at hand:

    ./affiliate-engine -db sqlite:affiliate-engine.db import --from rewardful --csv affiliates.csv --referrals referrals.csv --commissions commissions.csv --dry-run
    ./affiliate-engine -db sqlite:affiliate-engine.db import --from rewardful --csv affiliates.csv --referrals referrals.csv --commissions commissions.csv
    ./affiliate-engine -db sqlite:affiliate-engine.db import --list
    ./affiliate-engine -db sqlite:affiliate-engine.db import --undo 1

The admin console does the same without a shell: "Import from another
tool" takes the CSV files, shows the review, imports, and lists each batch
with an Undo button (the API is `/api/v1/admin/import`).

The dry run writes nothing and reports counts, conflicts and the fraud
found in the history. The import is one batch; re-running it changes
nothing. `--undo` removes exactly that batch and refuses once money moved
against it (no ledger rows are ever deleted or reversed: the import posts
none). Commissions are never imported; settle balances in the old tracker.

While both trackers run, compare them daily:

    ./affiliate-engine -db sqlite:affiliate-engine.db compare --from rewardful --referrals referrals.csv --commissions commissions.csv --clicks clicks.csv --date 2026-09-28

## 8. Postgres (optional)

    ./affiliate-engine -db "postgres://user:pass@host:5432/affiliate?sslmode=verify-full&sslrootcert=/etc/ssl/certs/db-ca.pem"

Use `sslmode=verify-full`: `require` encrypts but does not check the
server's certificate or hostname, so it does not stop a man in the middle.

The same binary, run `./affiliate-engine -db "postgres://..." migrate` first (creates every table), then serve — the server auto-detects the engine from the DSN and applies the correct chain.
The full invariant suite is verified against PG 18.x in CI. The startup log
prints only the engine and host, never the DSN.

The money and audit tables (`ledger_entries`, `journal_entries`,
`audit_log`, `commissions`, `payouts`, `payout_lines`, `raw_events`) carry
`BEFORE UPDATE/DELETE` row triggers and `BEFORE TRUNCATE` statement
triggers. Triggers stop the application and mistakes; they do not stop a
role that owns the tables (it can drop or disable them). For defense in
depth run with two roles.

### Two-role setup (recommended)

- **owner** (`ae_owner`): owns the schema, runs migrations. Its password
  lives only where migrations run.
- **runtime** (`ae_app`): what the server connects as. No DDL, no
  `TRUNCATE`, no `DELETE` on money/audit tables.

```sql
-- as a superuser / database owner
CREATE ROLE ae_owner LOGIN PASSWORD '...';
CREATE ROLE ae_app   LOGIN PASSWORD '...';
CREATE DATABASE affiliate OWNER ae_owner;
\c affiliate
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO ae_app;
```

Run the migrations once as the owner (`./affiliate-engine -db
"postgres://ae_owner:..." migrate`), then grant the runtime role:

```sql
-- as ae_owner
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ae_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ae_app;

-- money + audit tables: append-only for the runtime role
REVOKE DELETE, TRUNCATE ON ledger_entries, journal_entries, audit_log,
                           commissions, payouts, raw_events FROM ae_app;
REVOKE UPDATE ON ledger_entries, journal_entries, audit_log FROM ae_app;
-- payout_lines: the payout runner prunes cancelled/clawed-back lines
-- from a payout it is processing, so DELETE stays (the trigger limits it)
REVOKE TRUNCATE ON payout_lines FROM ae_app;

-- tables created by future migrations get the default grants
ALTER DEFAULT PRIVILEGES FOR ROLE ae_owner IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ae_app;
ALTER DEFAULT PRIVILEGES FOR ROLE ae_owner IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO ae_app;
```

The server applies the (idempotent) migration chain on every start, which
needs DDL rights. Give it the owner DSN for that step only:

    AE_MIGRATE_DATABASE_URL="postgres://ae_owner:...@db:5432/affiliate?sslmode=verify-full" \
    ./affiliate-engine -db "postgres://ae_app:...@db:5432/affiliate?sslmode=verify-full"

With `AE_MIGRATE_DATABASE_URL` set, migrations run over that connection
(which is then closed) and the server serves over `-db`. The two DSNs must
use the same engine. Re-run the `REVOKE` statements after an upgrade that
adds a money table, and keep `-db` pointing at the runtime role.

## Live demo (AE_DEMO=1)

The public demo (`https://try.affiliatetracking.co`) is this binary in
read-only demo mode over a generated sample program: Northwind Notes, a
fictional note-taking SaaS, with 40 affiliates, 90 days of clicks, sales,
renewals, refunds, a paid and a pending payout run, and one fraud story
per detection rule the engine can fire.

    docker run -d --name ae-demo --restart unless-stopped \
      -p 8080:8080 -v ae-demo-data:/data \
      -e AE_DEMO=1 -e AE_PUBLIC_URL=https://try.affiliatetracking.co \
      -e AE_TRUSTED_PROXIES=<your proxy CIDRs> \
      ghcr.io/thebigasif/affiliate-tracking:<tag>

- **`AE_DEMO=1`**: the dashboard opens with no key prompt, on the public
  key `demo` (a read-only admin), under a "Live demo" banner. Every
  request that is not GET/HEAD answers `403` with a read-only message;
  billing webhooks and `/mcp` are refused; `/r/` links open the dashboard
  without recording a click. Real API keys and portal sessions are not
  accepted at all.
- **A dedicated volume.** The demo seeds an empty database on first start
  and replaces it daily. It refuses to start on a database `demo-seed`
  did not create, so it cannot wipe a real program.
- **The restart policy must be on** and must restart a clean exit:
  `--restart unless-stopped` or `always` (not `on-failure`), or
  `restart: unless-stopped` in compose. On start the engine reseeds when
  the seed is a day old (23h); once it is, the process exits with status
  0 at the next 04:00 UTC and the restart brings it back with fresh dates.
  A crash-restart in between keeps the current seed.
- **`AE_TRUSTED_PROXIES`**: every visitor shares the demo key, so request
  limits are per client IP. Behind a proxy, set it or all visitors share
  one bucket.

Build or inspect the sample program without serving it:

    ./affiliate-engine -db sqlite:demo.db demo-seed          # refuses a non-empty database
    ./affiliate-engine -db sqlite:demo.db demo-seed -reset   # wipes a previous demo seed first
    ./affiliate-engine -db sqlite:demo.db doctor

## Upgrades

Replace the binary, restart. Migrations are forward-only and idempotent;
the guard-trigger script runs last on every start and re-creates every
money/audit trigger, so fixes reach existing installs. `doctor` verifies
integrity after every upgrade.

### Screening commissions from older releases

Releases before `commissions.screened_at` screened a sale after its
credit committed; a crash or a failed hold write in between left it
credited and never screened. Such a commission is screened when its
provider delivers the event again. To screen them all now, run
`screen-backfill` once after upgrading (safe while the server runs,
SQLite or Postgres):

    ./affiliate-engine -db sqlite:affiliate-engine.db screen-backfill -dry-run   # what would be held, writes nothing
    ./affiliate-engine -db sqlite:affiliate-engine.db screen-backfill            # screen them all
    ./affiliate-engine -db sqlite:affiliate-engine.db screen-backfill -limit 500 -since 2026-01-01

A commission is picked when it was credited from a sale, is pending or
approved, has never been screened, and has no hold (open or resolved):
paid, cancelled and reviewed commissions are left alone. Each one is
screened in its own transaction with the same rules and holds as a new
sale. The summary lists how many were scanned, screened and held (per
rule, and each held commission); with errors it exits 1, and running it
again retries them. A second run finds nothing.

Run the dry run first and read the list. Every commission credited before
the upgrade counts as never screened, including the ones the old release
did screen, and the rules are evaluated as of now, on data that arrived
after the sale (a partner's account paused since, a payment card seen
again later). Some commissions that passed at the time are held this
time; on the demo data, 48 of 660. This is also why the engine does not
run it by itself on start. A hold blocks the payout until you release it
in the fraud queue.
