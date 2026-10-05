# ADR-0007: PostgreSQL full-text search first, pgvector later
Status: Accepted

## Context
The MVP catalog is hundreds to low thousands of titles. Users search by title, creator, cast, genre and language, often with Yoruba, Igbo or Hausa names that carry diacritics and typos. A separate search engine is a non-goal, and AI features are out of scope now.

## Decision
- Use PostgreSQL only. Each title has a generated `tsvector` column with weights (title A, creator and cast B, synopsis C) and a GIN index.
- Use the `simple` text-search configuration, because English stemming is wrong for these languages. Add `unaccent` for diacritics and `pg_trgm` for typo-tolerant title matching and autocomplete. Query with `websearch_to_tsquery`.
- **pgvector is deferred.** No extension, no embedding columns and no AI code in the MVP. Adding it later is a purely additive migration.
- Revisit when the catalog passes about 50,000 titles, or search analytics show semantic misses, or AI work enters scope.
- Choose a managed Postgres that offers `pg_trgm`, `unaccent` and `pgvector`.

## Consequences
- No extra service to run, and search is transactionally consistent with the catalog.
- Relevance tuning is limited and there is no semantic or "find similar" search yet.
- Search queries are tested with diacritic and typo cases.
- A catalog migration will add `unaccent` and `pg_trgm` (the baseline migration has only `pgcrypto` and `citext`).
