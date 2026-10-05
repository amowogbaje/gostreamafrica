# ADR-0009: Payment provider strategy
Status: Accepted

## Context
All payments are in NGN. Paystack is the primary provider and Flutterwave the fallback if Paystack has an outage or an account problem. Payment code is the highest-risk code in the product: double charges and free access are both costly.

## Decision
- The billing module defines a `PaymentProvider` interface (initialize checkout, verify transaction, parse webhook). Paystack and Flutterwave are two adapters behind it.
- The active provider is chosen by configuration, not by automatic retry. An operator flips it. Silently retrying a payment on a second provider could charge a customer twice.
- Amounts are stored as integer kobo. Currency is NGN only.
- Entitlement is granted only after the server verifies the transaction with the provider API. A browser redirect or a webhook alone is never enough.
- Webhooks check the signature (Paystack: HMAC-SHA512 header. Flutterwave: the configured secret hash header) and are processed idempotently using the provider event or reference id.
- Every payment attempt is stored with provider, reference, status and raw event, with secrets and card data excluded. A daily reconciliation job compares our records with the provider.
- Payment endpoints are rate limited. Keys live in environment variables, test keys everywhere except production.

## Consequences
- Provider outages are survivable, at the cost of building and testing two adapters. Build Paystack first and Flutterwave behind the same interface later.
- A manual switch means someone must notice an outage, so alert on checkout failure rates.
- Recurring billing details depend on ADR-0008.
- Customers on a switched provider cannot reuse saved authorisations across providers.
