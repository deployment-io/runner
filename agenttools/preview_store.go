package agenttools

// preview_store.go defines the control-plane seam the preview deploy tools close over.
// agenttools owns these types and knows nothing about the RPC, the update pipeline, or
// the deployment DTO — the commands layer supplies a PreviewStore implementation that
// bridges to deployment-server. One seam serves every service type: the static tool
// reads/writes the CloudFront* fields of PreviewState; web-service and database tools
// (when they land) use their own fields on the same struct, with no change to this
// interface — a new type is a new field here plus a mapping line in the commands layer,
// not a new method.

// PreviewState is a neutral snapshot of a preview service's persisted cloud resources —
// what a prior deploy created and a later deploy reuses. Each service type populates
// only its own fields. It is deliberately NOT the control-plane deployment DTO, so
// agenttools stays decoupled from it; the commands layer maps between the two.
type PreviewState struct {
	// Static site (S3 + CloudFront).
	CloudFrontDistributionID  string
	CloudFrontDistributionArn string
	CloudFrontDomainName      string

	// Web service (ALB + ECS) and database (RDS) resources land here as those tools
	// arrive — e.g. LoadBalancerDNS / TargetGroupArn / EcsServiceArn / Port, or
	// RdsDatabaseArn / RdsEndpoint. Adding a field (not a method) keeps PreviewStore
	// fixed and every existing deploy tool untouched.
}

// PreviewStore is the find-or-create + persist seam for a task's preview services. An
// implementation is bound to ONE serviceType (the static tool gets a static-bound store,
// a web-service tool a web-bound one), so callers never pass a type token through here.
type PreviewStore interface {
	// EnsurePreview find-or-creates the persisted record for the named service under the
	// task's ephemeral env and returns its id plus any resources a prior deploy saved
	// (zero-valued PreviewState on the first deploy). Idempotent; round-trips the server.
	EnsurePreview(serviceName string) (previewID string, existing PreviewState, err error)

	// SavePreview persists freshly created resources back onto the record so the next
	// call / step / runner reuses them. Best-effort (fire-and-forget for the caller).
	SavePreview(previewID string, resources PreviewState)
}
