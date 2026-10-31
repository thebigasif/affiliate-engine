# Connect your site and switch from another tracker

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

[Back to the README](../README.md)
