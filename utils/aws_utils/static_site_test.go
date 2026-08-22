package aws_utils

import (
	"io"
	"strings"
	"testing"
)

// The empty-set guard is the only thing standing between "prune what this build
// did not produce" and "delete the site".
//
// It runs BEFORE the bucket is listed, which is why a nil client is a valid
// argument here and why the test is worth having at all: any path that reaches
// S3 with nothing to keep has already lost. An upload that silently produced no
// keys — a wrong directory, a walk that matched nothing — must fail loudly
// rather than quietly empty a live bucket.
func TestPruneStaleS3Files_RefusesAnEmptyKeepSet(t *testing.T) {
	for _, keep := range []map[string]bool{nil, {}} {
		err := PruneStaleS3Files(nil, "some-bucket", keep, io.Discard)
		if err == nil {
			t.Fatalf("PruneStaleS3Files(keep=%v) = nil; an empty keep set means "+
				"delete everything, which is never a legitimate outcome after an upload", keep)
		}
		// The message has to name the bucket: this surfaces in a customer's
		// deploy log, where "refusing to prune" alone says nothing about what.
		if !strings.Contains(err.Error(), "some-bucket") {
			t.Errorf("error %q does not name the bucket", err)
		}
	}
}

// A non-empty keep set must get past the guard and go on to list the bucket.
// Without this, the guard could be tightened into "always refuse" and the test
// above would still pass while pruning silently stopped happening.
//
// Reaching S3 with a nil client panics rather than returning, so the panic IS
// the assertion — it proves the guard let this call through.
func TestPruneStaleS3Files_ProceedsWhenSomethingIsKept(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("PruneStaleS3Files returned without reaching S3; a populated " +
				"keep set must be pruned, not refused")
		}
	}()
	//nolint:errcheck // the panic is the assertion
	PruneStaleS3Files(nil, "some-bucket", map[string]bool{"index.html": true}, io.Discard)
}

// ⚠️ S3 accepts at most 1000 keys per DeleteObjects request. This batched at
// 9000 under a comment claiming the limit was 10000, and never failed only
// because a single-bundle site is a handful of objects. One code-split build is
// already 241, so the headroom that hid this is gone.
func TestS3DeleteObjectsLimit_MatchesWhatS3Accepts(t *testing.T) {
	if s3DeleteObjectsLimit != 1000 {
		t.Errorf("s3DeleteObjectsLimit = %d, want 1000 — S3 rejects a larger "+
			"batch outright, and the failure only appears on a site big enough "+
			"to exceed it", s3DeleteObjectsLimit)
	}
}
