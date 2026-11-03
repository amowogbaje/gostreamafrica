package creators

import "context"

// ScanTarget names the object to check.
type ScanTarget struct {
	Bucket string
	Key    string
	Size   int64
}

// Scanner checks an uploaded object for malware. A real implementation (ClamAV, a cloud scanning
// service) plugs in here without touching the upload flow.
type Scanner interface {
	// Scan returns the name of the threat found, or "" for a clean file. An error means the scan
	// could not be done; the upload is then NOT accepted (fail closed) and can be retried.
	Scan(ctx context.Context, target ScanTarget) (threat string, err error)
}

// NoopScanner accepts everything. It is the local-development default and is logged at start-up
// so nobody mistakes it for protection.
type NoopScanner struct{}

// Scan implements Scanner.
func (NoopScanner) Scan(context.Context, ScanTarget) (string, error) { return "", nil }
