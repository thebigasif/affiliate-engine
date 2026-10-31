# Payouts, tax forms and privacy

## Payouts

- **Hold.** A commission matures after its program's `net_terms_days` (0 = no hold). New programs get `default_hold_days` from `/admin/settings` (default 30). Changing a program's hold applies to new commissions only.
- **Minimum.** A payout run skips an affiliate whose net balance in a currency is below `min_payout_cents` (default 5000, in minor units of that currency). Pass `min_threshold_cents` on a run to override it once.
- **Tax forms.** Affiliates upload a W-9, W-8BEN or W-8BEN-E PDF from the portal (5 MB max, checked to be a PDF). The status becomes `pending`, whatever it was before, so it blocks payouts until the operator reviews it. The file is sealed with AES-256-GCM under a key derived from the deployment secret and stored in the database. It is never served from `/static/`; only the admin download returns it, as an attachment. The admin then validates or rejects it (or marks it received); only the admin can move a form out of `pending`. A `pending` or `rejected` form blocks batching and approval for that affiliate. GDPR erase deletes the files.
- **Paying.** Admin console: run the batch, download the PayPal or Wise CSV, pay on the rail, then click Approve on each payout to settle the ledger. Approve is safe to click twice.

## Privacy and GDPR

- **Write-time IP anonymization**: `AE_IP_MODE=trunc24` truncates IPv4 to /24 and IPv6 to /48 *before* the identity hash is computed; the raw IP is never stored anywhere (only salt-free hashes). Unique counting and IP-velocity fraud rules degrade to subnet granularity, a deliberate, documented trade. Unknown `AE_IP_MODE` values fail startup loudly.
- **Subject export (Art. 15/20)**: `affiliate-engine gdpr-export -affiliate N [-out file.json]` emits the machine-readable bundle (identity, clicks, attributions, customers, commissions, payouts, codes, coupons, badges, XP, tax docs). Output file is mode 600, because it is subject PII.
- **Right to erasure (Art. 17)**: `affiliate-engine gdpr-erase -affiliate N -yes` scrubs identity in place and revokes access (sessions, grants, affiliate API keys); booked commissions, payouts, ledger journals, aggregates and MLM structure remain under the pseudonymous id (accounting retention: the books stay balanced and Σ=0 proofs keep verifying). Idempotent; every erasure lands in the append-only audit log.

[Back to the README](../README.md)
