# ADR-0008: Pricing model for the MVP
Status: **Proposed. Owner decision needed.**

## Context
The mockups disagree. The web mockup shows a N2,000 per month subscription. The mobile mockup shows a N500 rental. Mobile apps are out of scope, but the pricing question is not. The loop only needs "viewer pays, then content is available", and either model proves it. Pricing affects entitlement logic, checkout, payment failures and how creators are paid later.

## Options
| | A. Subscription only (N2,000/month) | B. Rental only (N500/title) | C. Both |
|---|---|---|---|
| Pros | Simplest entitlement (active or expired). Predictable revenue. Matches the web mockup. | Low commitment, suits casual viewers and mobile-data budgets. One-off charges are simpler and more reliable. Per-title revenue makes creator payouts easy. | Reaches both audiences. |
| Cons | N2,000 up front is a big ask with a thin launch catalog. Recurring card charges fail more often (funds, OTP), so retries and a grace period are needed. Splitting revenue among creators needs a pool rule. | Every watch is a purchase decision. Lower repeat revenue. Needs per-title expiry rules. | Two entitlement models, two checkouts, confusing prices, double the tests. Conflicts with "keep it simple". |

## Recommendation
Option A, as you suggested, with these safeguards: model access as a generic `entitlement` (user, scope, starts_at, expires_at) so rentals can be added later without a rewrite, and decide the failed-renewal grace period before building billing. Open points: free trial or not, and whether N2,000 holds up against the launch catalog size.

## Consequences (if A is approved)
- One checkout flow and one plan to configure in the payment provider.
- Creator revenue sharing is deferred until creators are paid.
- Rental can be added later as a second entitlement scope.

## Decision
Pending. Reply with A, B or C to move this to Accepted.
