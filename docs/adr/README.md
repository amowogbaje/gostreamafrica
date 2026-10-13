# Architecture Decision Records

One page each. Copy `0000-template.md`. Never edit an accepted ADR's decision: write a new one that supersedes it.
ADRs 0001-0003 were written in Phase 0 Step 1, so later numbering starts at 0004.

| # | Title | Status |
|---|---|---|
| 0001 | Monorepo with two Go modules | Accepted |
| 0002 | Local stack choices | Accepted |
| 0003 | MinIO community images are no longer published | Accepted (temporary; replacement planned) |
| 0004 | Modular monolith in Go instead of microservices | Accepted |
| 0005 | HLS delivery with a separate FFmpeg worker | Accepted |
| 0006 | S3-compatible storage with signed URLs | Accepted |
| 0007 | PostgreSQL full-text search first, pgvector later | Accepted |
| 0008 | MVP pricing model: subscription only | Accepted |
| 0009 | Payment provider strategy | Accepted |
| 0010 | Content licensing approach: curated first, creators by invitation | Accepted (legal review pending) |
| 0011 | Server-side sessions and argon2id passwords | Superseded by 0012 |
| 0012 | Short-lived JWT access tokens and rotating refresh tokens | Accepted |
| 0013 | Catalog read model: visibility, keyset pagination, epoch-versioned cache | Accepted |
