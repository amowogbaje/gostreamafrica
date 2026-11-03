// Package creators owns creator uploads: starting a resumable multipart upload straight to
// object storage, finishing it, validating the file, queueing the transcode job, and cleaning
// up uploads that were abandoned. (Creator profiles and publishing arrive in later phases.)
//
// The API never carries video bytes. It hands the browser presigned part URLs; the browser
// sends each part directly to the object store (ADR-0006, ADR-0014). The state machine and the
// contract with the worker are documented in docs/upload-flow.md.
package creators
