package aws_utils

import (
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The empty-set guard is the only thing standing between "prune what this build
// did not produce" and "delete the site".
//
// It runs BEFORE the bucket is listed, which is why a nil client is a valid
// argument here: any path that reaches S3 with nothing to keep has already
// lost. An upload that silently produced no keys — a wrong directory, a walk
// that matched nothing — must fail loudly rather than quietly empty a live
// bucket.
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

func objects(keys ...string) []s3Types.Object {
	out := make([]s3Types.Object, 0, len(keys))
	for _, k := range keys {
		out = append(out, s3Types.Object{Key: aws.String(k)})
	}
	return out
}

// The set difference is the one computation whose bug deletes a live file, so
// it is pinned directly rather than inferred from what a client would have
// been asked to delete.
func TestStaleS3Objects_DeletesExactlyWhatTheBuildDidNotProduce(t *testing.T) {
	listed := objects(
		"index.html",                   // in both builds — kept
		"assets/app-NEW111.js",         // this build — kept
		"assets/app-OLD999.js",         // previous build — stale
		"assets/vendor-antd-OLD999.js", // previous build — stale
	)
	keep := map[string]bool{
		"index.html":           true,
		"assets/app-NEW111.js": true,
		// Keys the upload wrote but the listing may not show yet (S3 list is
		// eventually consistent in edge cases) are simply absent from listed —
		// they must not confuse the difference.
		"assets/extra-NEW111.js": true,
	}

	stale := staleS3Objects(listed, keep)

	got := map[string]bool{}
	for _, id := range stale {
		got[aws.ToString(id.Key)] = true
	}
	want := []string{"assets/app-OLD999.js", "assets/vendor-antd-OLD999.js"}
	if len(got) != len(want) {
		t.Fatalf("staleS3Objects returned %d keys %v, want exactly %v", len(got), got, want)
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("previous build's %q was not marked stale; it would never be cleaned up", k)
		}
	}
	// The inverse matters more: a kept key marked stale is a live file deleted.
	for k := range got {
		if keep[k] {
			t.Errorf("%q is in the current build and was marked stale — this deletes a live file", k)
		}
	}
}

// An unchanged build produces no deletions: every listed object is in the keep
// set, so the prune must be a no-op rather than a rewrite.
func TestStaleS3Objects_UnchangedBuildDeletesNothing(t *testing.T) {
	listed := objects("index.html", "assets/app-SAME.js")
	keep := map[string]bool{"index.html": true, "assets/app-SAME.js": true}
	if stale := staleS3Objects(listed, keep); len(stale) != 0 {
		t.Errorf("staleS3Objects = %v on an unchanged build, want none", stale)
	}
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
