package aws_utils

import (
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The empty-set guard is the only thing standing between "mark what this build
// did not produce" and "schedule the whole site for deletion".
//
// It runs BEFORE anything touches S3 — before the lifecycle rule is ensured and
// before the bucket is listed — which is why a nil client is a valid argument
// here: any path that reaches S3 with nothing to keep has already lost. An
// upload that silently produced no keys — a wrong directory, a walk that
// matched nothing — must fail loudly rather than quietly put a live bucket on a
// seven-day timer.
func TestMarkStaleS3Files_RefusesAnEmptyKeepSet(t *testing.T) {
	for _, keep := range []map[string]bool{nil, {}} {
		err := MarkStaleS3Files(nil, "some-bucket", keep, io.Discard)
		if err == nil {
			t.Fatalf("MarkStaleS3Files(keep=%v) = nil; an empty keep set means "+
				"mark everything, which is never a legitimate outcome after an upload", keep)
		}
		// The message has to name the bucket: this surfaces in a customer's
		// deploy log, where "refusing to mark" alone says nothing about what.
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

// The set difference is the one computation whose bug puts a live file on an
// expiry clock, so it is pinned directly rather than inferred from what a
// client would have been asked to tag.
func TestStaleS3Objects_MarksExactlyWhatTheBuildDidNotProduce(t *testing.T) {
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
	// The inverse matters more: a kept key marked stale is a live file S3
	// deletes a week from now, long after the deploy that caused it.
	for k := range got {
		if keep[k] {
			t.Errorf("%q is in the current build and was marked stale — this expires a live file", k)
		}
	}
}

// An unchanged build marks nothing: every listed object is in the keep set, so
// the pass must be a no-op rather than a rewrite.
func TestStaleS3Objects_UnchangedBuildMarksNothing(t *testing.T) {
	listed := objects("index.html", "assets/app-SAME.js")
	keep := map[string]bool{"index.html": true, "assets/app-SAME.js": true}
	if stale := staleS3Objects(listed, keep); len(stale) != 0 {
		t.Errorf("staleS3Objects = %v on an unchanged build, want none", stale)
	}
}

// A rollback re-uploads files an earlier deploy already tagged. Nothing
// un-tags them: the upload itself does, because an S3 overwrite without
// explicit tagging replaces the object with an empty tag set. What this pins is
// the half that lives in this package — the re-uploaded key is in keep, so the
// marking pass does not put it straight back on the clock.
func TestStaleS3Objects_RollbackReuploadIsNotReMarked(t *testing.T) {
	// The bucket after the rollback's upload: the old build's files are back
	// (fresh, untagged) alongside what the build being rolled back left.
	listed := objects("index.html", "assets/app-OLD999.js", "assets/app-NEW111.js")
	keep := map[string]bool{"index.html": true, "assets/app-OLD999.js": true}

	for _, id := range staleS3Objects(listed, keep) {
		if aws.ToString(id.Key) == "assets/app-OLD999.js" {
			t.Fatal("the rolled-back-to build's file was marked stale again; the upload " +
				"cleared its tag and this would immediately restore it")
		}
	}
}

// The rule is what turns a tag into a deletion, so its shape is pinned here
// rather than left to be discovered on a customer's bucket a week later.
func TestWithStaleLifecycleRule_AppendsATagScopedSevenDayRule(t *testing.T) {
	rules, changed := withStaleLifecycleRule(nil)
	if !changed {
		t.Fatal("withStaleLifecycleRule(nil) reported no change; a bucket with no " +
			"configuration is the first-deploy case and must get the rule")
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2 (stale expiry + incomplete-MPU cleanup)", len(rules))
	}
	var rule s3Types.LifecycleRule
	var found bool
	for _, r := range rules {
		if aws.ToString(r.ID) == staleLifecycleRuleID {
			rule, found = r, true
		}
	}
	if !found {
		t.Fatalf("no rule with ID %q — the ID is how the next deploy recognises "+
			"its own rule, so a mismatch appends a duplicate every time", staleLifecycleRuleID)
	}
	if rule.Status != s3Types.ExpirationStatusEnabled {
		t.Errorf("rule status = %q, want Enabled; a disabled rule expires nothing "+
			"and the bucket grows without limit", rule.Status)
	}
	if rule.Expiration == nil || aws.ToInt32(rule.Expiration.Days) != staleObjectExpiryDays {
		t.Errorf("rule expiration = %v, want Days=%d", rule.Expiration, staleObjectExpiryDays)
	}
	// ⚠️ The filter MUST be the tag. Scoped to a prefix or to nothing, the rule
	// expires by age alone — and a site deployed once and then left running
	// refreshes no LastModified, so on day 7 it deletes itself.
	tagFilter, ok := rule.Filter.(*s3Types.LifecycleRuleFilterMemberTag)
	if !ok {
		t.Fatalf("rule filter is %T, want a tag filter; anything else expires "+
			"objects the current build still serves", rule.Filter)
	}
	if aws.ToString(tagFilter.Value.Key) != staleObjectTagKey || aws.ToString(tagFilter.Value.Value) != staleObjectTagValue {
		t.Errorf("rule filters on %s=%s, want %s=%s — the marking pass writes the "+
			"latter, and a rule watching anything else never fires",
			aws.ToString(tagFilter.Value.Key), aws.ToString(tagFilter.Value.Value),
			staleObjectTagKey, staleObjectTagValue)
	}
}

// The uploader writes every file via multipart upload, and a deploy that dies
// mid-upload strands the finished parts invisibly: not listed, not deletable by
// DeleteAllS3Files, billed forever, and able to fail teardown's DeleteBucket
// with BucketNotEmpty on a bucket that lists as empty. The cleanup rule is what
// makes that self-healing — and it must NOT be tag-scoped, both because S3
// rejects AbortIncompleteMultipartUpload on a tag filter and because stranded
// parts were never tagged by anything.
func TestWithStaleLifecycleRule_AddsWholeBucketIncompleteMPUCleanup(t *testing.T) {
	rules, _ := withStaleLifecycleRule(nil)
	var rule s3Types.LifecycleRule
	var found bool
	for _, r := range rules {
		if aws.ToString(r.ID) == abortIncompleteMPURuleID {
			rule, found = r, true
		}
	}
	if !found {
		t.Fatalf("no rule with ID %q", abortIncompleteMPURuleID)
	}
	if rule.Status != s3Types.ExpirationStatusEnabled {
		t.Errorf("rule status = %q, want Enabled", rule.Status)
	}
	if rule.AbortIncompleteMultipartUpload == nil ||
		aws.ToInt32(rule.AbortIncompleteMultipartUpload.DaysAfterInitiation) != abortIncompleteMPUDays {
		t.Errorf("AbortIncompleteMultipartUpload = %v, want DaysAfterInitiation=%d",
			rule.AbortIncompleteMultipartUpload, abortIncompleteMPUDays)
	}
	prefix, ok := rule.Filter.(*s3Types.LifecycleRuleFilterMemberPrefix)
	if !ok || prefix.Value != "" {
		t.Errorf("filter = %#v, want an empty-prefix (whole bucket) filter — "+
			"stranded parts belong to no tag and no prefix", rule.Filter)
	}
	if rule.Expiration != nil {
		t.Errorf("cleanup rule carries Expiration %v; a whole-bucket rule that "+
			"expires OBJECTS is the abandoned-site self-delete this package "+
			"exists to avoid", rule.Expiration)
	}
}

// A bucket configured by an older runner has the stale rule but not the MPU
// cleanup. The next deploy must add only what is missing, not duplicate what
// is there.
func TestWithStaleLifecycleRule_UpgradesAPartiallyConfiguredBucket(t *testing.T) {
	older, _ := withStaleLifecycleRule(nil)
	justStale := older[:1]
	if aws.ToString(justStale[0].ID) != staleLifecycleRuleID {
		t.Fatalf("test setup: expected the stale rule first, got %q", aws.ToString(justStale[0].ID))
	}

	rules, changed := withStaleLifecycleRule(justStale)
	if !changed {
		t.Fatal("an older bucket gained no rules; fleets never converge")
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	count := 0
	for _, r := range rules {
		if aws.ToString(r.ID) == staleLifecycleRuleID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("stale rule appears %d times; the upgrade duplicated an existing rule", count)
	}
}

// This runs on every deploy of every site. Appending unconditionally would add
// a rule per deploy until the bucket hit the 1000-rule limit and deploys began
// failing on a bucket that was already configured correctly.
func TestWithStaleLifecycleRule_IsIdempotent(t *testing.T) {
	once, _ := withStaleLifecycleRule(nil)
	twice, changed := withStaleLifecycleRule(once)
	if changed {
		t.Error("withStaleLifecycleRule reported a change on a bucket that already " +
			"has the rule; that is a needless PutBucketLifecycleConfiguration per deploy")
	}
	if len(twice) != 2 {
		t.Fatalf("got %d rules after two passes, want 2 — a rule is being appended repeatedly", len(twice))
	}
}

// PutBucketLifecycleConfiguration replaces the bucket's ENTIRE configuration.
// Whatever else the bucket was configured to do — a customer's own rule, an
// incomplete-multipart cleanup — has to come back out of the merge, or the
// first deploy after this shipped would quietly delete it.
func TestWithStaleLifecycleRule_PreservesUnrelatedRules(t *testing.T) {
	existing := []s3Types.LifecycleRule{{
		ID:         aws.String("customers-own-rule"),
		Status:     s3Types.ExpirationStatusEnabled,
		Filter:     &s3Types.LifecycleRuleFilterMemberPrefix{Value: "logs/"},
		Expiration: &s3Types.LifecycleExpiration{Days: aws.Int32(30)},
	}}

	rules, changed := withStaleLifecycleRule(existing)

	if !changed {
		t.Fatal("withStaleLifecycleRule reported no change; the bucket has rules but not ours")
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3 (theirs + our two)", len(rules))
	}
	if aws.ToString(rules[0].ID) != "customers-own-rule" {
		t.Errorf("the pre-existing rule is gone; a blind put of just our rule "+
			"deletes the bucket's other lifecycle configuration. Rules: %v", rules)
	}
	// The caller's slice must not be rewritten under it either.
	if len(existing) != 1 {
		t.Errorf("the input slice was mutated (len %d)", len(existing))
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
