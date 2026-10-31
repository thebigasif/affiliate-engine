# Security Policy

## Supported versions

Pre-1.0: the latest `main` only.

## Reporting a vulnerability

Email the maintainer (see GitHub profile). Please include reproduction steps and impact assessment. We aim to respond within 72 hours. Do not open public issues for suspected vulnerabilities.

## Threat model summary

Key invariants enforced by tests:

| Guarantee | Mechanism | Test |
|---|---|---|
| Exactly-once money | `(source, external_event_id)` unique + idem-key ledger | P1, P7, replay corpus |
| Balanced ledger | Trigger-enforced sum-0, >=2 lines | P2/3/5 |
| No overdraft payable | Pre-post balance guard | P6 |
| Immutable money rows | UPDATE/UPDATE triggers ABORT | P9/10/11 |
| Webhook authenticity | HMAC timing-safe, verify-before-write, freshness window | billing corpora |
| Token integrity | HMAC + canonical base64, every-byte flip rejected | tokens suite |
| No SSRF from postbacks | host blocklist (loopback/private/metadata) | postbacks suite |
| No open redirect | destination allowlist incl. suffix tricks | redirect suite |
| Fraud can't pay out | commission_holds excluded from payout batches | fraud suite |
| No tenant crossing | scope-pinned keys/sessions | api auth suite |

## Money-handling rules for contributors

1. All money is INTEGER minor units. Never floats.
2. Every money mutation posts through the ledger or is a domain fact feeding it.
3. Never mutate commission state outside the state machine (fraud holds block payment; they don't reverse states).
4. Every new money path ships with property tests naming the invariant.
