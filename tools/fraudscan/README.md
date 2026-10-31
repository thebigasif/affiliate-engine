# Fraud scan (in-browser)

A free fraud check for affiliate programs on Rewardful, FirstPromoter, Tapfiliate or Tolt.
The visitor picks a CSV export. The browser reads it and runs our fraud rules on it.
Nothing is uploaded. There is no server part.

Files:

| File | What it is |
|---|---|
| `fraudscan.js` | The scan. Dependency-free ES module. `scan(csvText, {tool})`. |
| `calculator.js` | The no-upload estimate. Pure functions. `estimateRisk({...})`. |
| `demo.html` + `demo.js` | Bare page wiring both. Copy the wiring, not the markup. |
| `test.mjs` | Node tests. `node tools/fraudscan/test.mjs` |
| `parity_test.go` | Runs the real Go engine on the same fixtures. `go test ./tools/fraudscan/` |
| `testdata/` | 8 synthetic exports, `expected.json` (both test suites assert it), `normalized.json` (rows the Go test replays). |

## Privacy note (page copy)

> Your file is read in your browser. It is never uploaded.

Longer version, if the page has room:

> The scan runs on your computer. Your file is not sent to us or anyone else.
> Close the tab and it is gone.

This is true because:

- `fraudscan.js` and `calculator.js` make no network calls (a test fails if `fetch`, XHR, WebSocket, beacons or dynamic `import()` show up).
- The file is read with `file.text()`. That is a local read.
- The demo page sets a CSP with `connect-src 'none'`. Keep that on the real page. If the site needs analytics, load it on other pages, not this one. Any analytics script on this page could read the results.

## Embed

Serve both modules from the same origin as the page (ES modules do not load from `file://`).

```html
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; script-src 'self'; connect-src 'none'; img-src 'self'; style-src 'self'; form-action 'none'; base-uri 'none'">
<input type="file" id="file" accept=".csv,text/csv">
<script type="module">
  import { scan } from '/tools/fraudscan/fraudscan.js';
  document.getElementById('file').addEventListener('change', async (e) => {
    const text = await e.target.files[0].text();
    const result = scan(text);            // or scan(text, { tool: 'rewardful' })
    // render result with textContent only
  });
</script>
```

An inline module needs a CSP hash or nonce. `demo.js` as a separate file avoids that.

**Render with `textContent`, never `innerHTML`.** Affiliate names, emails and coupon codes come from the visitor's file. Treat them as untrusted.

Keep a size limit on the file input (the demo uses 50 MB). The scan is linear in rows, but the browser has to hold the text.

### `scan()` output

```js
{
  tool: 'rewardful',              // detected or passed in
  tool_detected: true,
  export_kind: 'conversions',     // or 'affiliates'
  rows: 14,
  parse: { rows, conversions|affiliates, skipped, dup_emails },
  columns: { affiliate_email: 'affiliate_email', ... },   // which header we read as what
  currency: 'USD' | null,
  rules_run: ['self_referral', ...],
  rules_skipped: [{ rule, reason }],                      // reason is page-ready copy
  findings: [{ rule, affiliate, unit, count, amount_cents, examples, scan_only }],
  summary: { commission_at_risk_cents, conversions_flagged, affiliates_flagged },
  warnings: ['...'],
}
```

- `count` is how many sales the engine would have put on hold. For affiliate lists `unit` is `accounts` and `count` is 1 per flagged account.
- `amount_cents` is the commission on those sales. It is `null` when the file has no commission column.
- `summary.commission_at_risk_cents` counts each flagged sale once, even when several rules hit it.
- `scan_only: true` marks the two checks with no Go rule behind them (see below).
- Errors are thrown as `Error` with a readable message (`unrecognized export header: ...`, `empty file`).

### `estimateRisk()` (no upload)

```js
import { estimateRisk, presetOptions } from './calculator.js';
estimateRisk({ monthlyPayouts: 10000, couponSharePct: 30, fraudRatePct: 5 });
// or { ..., preset: 'low' | 'typical' | 'high' }
```

Returns yearly payouts, yearly and monthly money at risk, a coupon vs link split (all in cents) and four short `lines` of page copy.

The fraud rate is the visitor's guess. The presets (2%, 5%, 10%) are labelled "your guess" in `presetOptions()`. They are round starting points, not industry data. Do not present them as benchmarks.

## How it works

Two export shapes:

1. **Affiliate lists.** Column mapping is a line-for-line copy of `internal/importer/importer.go` (`indexHeader` + `Parse`): same header names, same priority, same email check, same status mapping, same in-file duplicate collapse. The Go test parses the same files with the real importer and compares.
2. **Sales lists** (referrals, commissions, conversions). The Go importer does not read these. It never imports money rows. This mapping is scan-only. Its header names are in `CONVERSION_COLUMNS` in `fraudscan.js`.

The scan replays history. Rows are sorted by sale time. Every Go rule is checked at each sale using only the sales before it, the same way the engine screens a live sale. `parity_test.go` inserts the same rows into a real database one by one, runs `fraud.NewEngine(...).Evaluate` in dry-run, and asserts the same per-rule, per-affiliate counts.

Tool detection only sets the label. The Go importer takes `--source` from the operator and maps every tool the same way, so detection never changes how columns are read.

## Rules by export

Rule names and thresholds are the Go engine's (`internal/fraud/`). Each rule in `fraudscan.js` has a comment pointing at its Go source.

| Rule | Go source | Threshold | Needs | Affiliate list | Sales list |
|---|---|---|---|---|---|
| `self_referral` | engine.go | buyer email = affiliate email after alias cleanup | affiliate email, customer email | skipped: no sales | runs if both email columns exist |
| `payment_fingerprint` | rules3.go | 2+ of an affiliate's buyers also bought via another affiliate | customer email | skipped: no sales | runs if customer email exists |
| `duplicate_account` | rules3.go | same alias-normalized email as another active account | affiliate email | **runs** | runs if affiliate email exists |
| `coupon_hijack` | engine.go | 3+ sales on one coupon in 7 days whose customer was already tied to another affiliate | coupon, customer id or email, sale dates | skipped: no sales | runs if coupons were used |
| `conversion_velocity` | engine.go | 30+ sales for one affiliate in 1 hour | sale time of day | skipped: no sales | runs if dates have times |
| `conversion_time_anomaly` | rules2.go | 20+ sales in 7 days, over 50% at 00:00-05:59 UTC, z > 3 vs program | sale time of day | skipped: no sales | runs if dates have times |
| `click_velocity` | engine.go | 50+ in 1 hour from one IP | IP, sale time of day | skipped: no sales | runs if IP column exists |
| `ip_overlap` | rules3.go | 2+ IPs shared with other affiliates in 30 days, and 3+ sales | IP, sale dates | skipped: no sales | runs if IP column exists |
| `asn_reputation` | rules2.go | 80%+ datacenter clicks | click IP classification | skipped | skipped: no click stream, and we never look IPs up online |
| `bot_class_gating` | rules3.go | 50%+ suspected-bot clicks | click user agents | skipped | skipped: no click stream |
| `geo_mismatch` | rules3.go | 3+ and 30%+ sales from countries with no clicks | click country, sale country | skipped | skipped: exports have no click country |
| `ip_fallback_anomaly` | rules3.go | 5%+ sales via IP fallback | our attribution method | skipped | skipped: other tools do not record it |
| `inactive_affiliate` | engine.go | commission to a frozen account | live unpaid commissions | skipped | skipped: an export is history |
| `refund_rate` | none (scan only) | 5+ sales and 20%+ refunded or charged back | status or refunded column | skipped | runs if a status column exists |
| `click_to_conversion` | none (scan only) | 3+ sales under 10 seconds after the click | click time and sale time | skipped | runs if both have times |

Which of these a real export supports depends on the columns in it. What our fixtures (and the tests) show:

| Fixture | Runs | Skipped (reason) |
|---|---|---|
| Any affiliate list | `duplicate_account` | everything else: no sales in the file |
| `rewardful-referrals.csv` | self_referral, payment_fingerprint, duplicate_account, conversion_velocity, conversion_time_anomaly, refund_rate | coupon_hijack (no coupons used), click_velocity + ip_overlap (no IP), click_to_conversion (no click date) |
| `firstpromoter-commissions.csv` | self_referral, payment_fingerprint, duplicate_account, coupon_hijack, conversion_velocity, conversion_time_anomaly, refund_rate | click_velocity + ip_overlap (no IP), click_to_conversion |
| `tapfiliate-conversions.csv` | conversion_velocity, conversion_time_anomaly, click_velocity, ip_overlap, duplicate_account, click_to_conversion | self_referral + payment_fingerprint (no customer email), coupon_hijack (no coupon), refund_rate (no status) |
| `tolt-referrals.csv` | self_referral, payment_fingerprint, duplicate_account, conversion_velocity, conversion_time_anomaly, refund_rate | coupon_hijack (no coupons used), click_velocity + ip_overlap (no IP), click_to_conversion |

## Assumptions (need sign-off)

1. **Sales-export headers are our best reading, not verified against live exports.** The four fixture files use header names we chose. The column lists in `CONVERSION_COLUMNS` cover common names. Test with one real export from each tool before launch.
2. **Customer binding is first touch in the file.** `coupon_hijack` needs to know who a customer "belonged to" before the coupon sale. We use the affiliate on that customer's first sale in the file.
3. **A coupon value on a sale means the coupon got the credit.** Exports do not say how a sale was attributed.
4. **Sale IPs stand in for click IPs** in `click_velocity` and `ip_overlap`. The engine uses click IPs.
5. **Dates without a zone are UTC.** Only `YYYY-MM-DD` with optional time is read. `03/01/2026` is not (day vs month is ambiguous). Date-only files skip the time-of-day rules instead of guessing.
6. **Money uses "." as the decimal point.** A header containing `cents` or `minor` means the value is already in cents. Mixed currencies are added as-is with a warning.
7. **Refunded or charged-back rows count toward money at risk.** The incumbent may have clawed some of that back already.
8. **The two scan-only checks** (`refund_rate`, `click_to_conversion`) use our default thresholds (5 sales / 20%, 3 sales / 10 seconds). They are not industry figures. They can be changed through `scan()` options.
9. **Affiliates in a sales export count as active.** The export does not carry their status.

## Differences from the Go importer

- A UTF-8 BOM at the start of the file is stripped. Go's `encoding/csv` keeps it, so `importer.Parse` rejects a BOM file with "unrecognized export header". Excel adds a BOM when saving CSV. Worth fixing in the importer.
- A stray `"` inside an unquoted field is kept as text. Go returns a parse error.

## Changing a rule

1. Change the Go rule.
2. Mirror it in `fraudscan.js` (constants are in `GO` at the top).
3. `node tools/fraudscan/test.mjs --update`, review the diff in `testdata/expected.json`.
4. `go test ./tools/fraudscan/` must pass. It is the check that JS and Go still agree.
