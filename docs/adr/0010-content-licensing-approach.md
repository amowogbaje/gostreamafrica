# ADR-0010: Content licensing approach before public launch
Status: **Proposed. Owner decision needed.**

This ADR is a product and risk framing, not legal advice. Confirm the points below with a Nigerian entertainment or IP lawyer.

## Context
The platform will publish films it does not own. Without cleared rights the business risks takedowns, payment-provider account termination and legal claims. A film can also contain third-party music and footage. Distribution of films in Nigeria may require classification or registration (for example, with the film censors board). Verify what applies to online streaming.

## Options
| | A. Creator-warranted uploads | B. Curated licensed catalog | C. Public domain, CC or self-owned only | D. Hybrid |
|---|---|---|---|---|
| How | Creators accept a distribution agreement that warrants they hold the rights, and supply chain-of-title proof. | We sign non-exclusive licences with rights holders or distributors (flat fee, minimum guarantee or revenue share). | Launch only with content we own or that is freely licensed. | Launch with a small curated set (B), then onboard creators by invitation (A). |
| Pros | Fast, scales, fits the creator vision. | Clearest legal position, better quality control. | Lowest risk and cost. | Strong launch story and a path to scale. |
| Cons | Infringement risk lands on the platform's reputation. Needs takedown operations and verification. | Slow, needs legal time and cash, and negotiation. | Thin appeal, hard to prove demand. | Longer setup, and both paths must be run well. |

## Recommendation
Option D: no public launch until the first titles each have a signed licence on file, then invitation-only creators under a standard agreement. Before launch, have counsel confirm: licence terms (territory, term, exclusivity, payment), music clearance, classification requirements, and the takedown process.

## Consequences (whichever option is chosen)
- The catalog schema stores licence data per title: licensor, agreement reference, territory (default NG), start and end dates, and takedown status.
- Titles auto-unpublish when a licence window ends, and an admin can unpublish immediately.
- A documented takedown contact and process exist before launch.
- A pre-launch checklist gates publishing: signed agreement on file, chain-of-title evidence, music cleared, classification confirmed.

## Decision
Pending. Reply with A, B, C or D, plus any legal constraints, to move this to Accepted.
