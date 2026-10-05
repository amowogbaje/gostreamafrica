# ADR-0001: Monorepo with two Go modules
Status: accepted
- One repo, `api/` and `worker/` as separate Go modules.
- Why: independent deploy/build images (worker needs FFmpeg), no accidental cross-imports of `internal/`.
- Cost: small duplicated code (config, logging). If shared code grows beyond ~200 lines, extract a `pkg/` module.
