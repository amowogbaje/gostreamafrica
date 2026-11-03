# ADR-0015: A small standard-library S3 client instead of an SDK
Status: Accepted

## Context
Phase 2 needs a handful of S3 calls (multipart upload, presigned part URLs, head, ranged read, listing, delete) against
SeaweedFS locally and R2 or S3 in the cloud (ADR-0006, ADR-0014). The AWS SDK for Go v2 pulls in roughly fifteen modules
and changes its default checksum behaviour between releases, which S3-compatible stores have rejected before.

## Decision
`internal/platform/s3` implements SigV4 (header signing and query presigning) and those calls with the standard library only
(about 400 lines, no new dependency). It is verified three ways: AWS's published SigV4 test vectors, a fake-server test of every
operation, and a test against a real server (`TEST_S3_ENDPOINT`, plus `make s3-check`). Presigned part URLs sign the Content-Length,
so storage refuses a part of any other size.

## Consequences
- We own the signing code. If the needs grow past this small set, switch to the official SDK behind the same `creators.ObjectStore` interface.
- Adding an operation means adding the XML shape and a test, not a dependency.
