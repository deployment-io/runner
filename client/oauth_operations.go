package client

import (
	"github.com/deployment-io/deployment-runner-kit/oauth"
)

func (r *RunnerClient) RefreshGitToken(installationID string, organizationID string) (string, error) {
	if !r.isConnected {
		return "", ErrConnection
	}
	args := oauth.RefreshGitProviderTokenArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.InstallationID = installationID
	var refreshTokenDto oauth.RefreshGitProviderTokenDtoV1
	err := r.c.Call("Oauth.RefreshGitProviderTokenV1", args, &refreshTokenDto)
	if err != nil {
		return "", err
	}
	return refreshTokenDto.Token, nil
}

// GetOpenPullRequestForBranch asks the deployment-server whether there
// is an open PR on the user's git provider for the given (repo, branch)
// — used by the Tasks re-run flow (Q15 in PLAN_tasks_verification.md)
// to decide the agent's checkout base. The runner doesn't need to know
// provider details; deployment-server dispatches on the installation's
// configured provider.
//
// repoName follows the same provider-side identifier convention as
// OpenPullRequest below.
//
// Returns the structured DTO so the caller can distinguish:
//   - Found=false                   → no PR has ever existed
//   - Found=true,  State=="open"    → iterate (branch from PR head)
//   - Found=true,  State=="closed", Merged=true   → fail loudly
//   - Found=true,  State=="closed", Merged=false  → start over from base
//
// The caller is expected to treat *any* error (RPC failure, provider
// not supporting the optional interface, transient network) as "no PR
// info available, fall back to the default base-branch path." Same
// effective behavior as before Q15 — graceful degrade.
func (r *RunnerClient) GetOpenPullRequestForBranch(organizationID, installationID, repoName, headBranch string) (oauth.GetOpenPullRequestForBranchDtoV1, error) {
	var dto oauth.GetOpenPullRequestForBranchDtoV1
	if !r.isConnected {
		return dto, ErrConnection
	}
	args := oauth.GetOpenPullRequestForBranchArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.InstallationID = installationID
	args.RepoName = repoName
	args.HeadBranch = headBranch
	if err := r.c.Call("Oauth.GetOpenPullRequestForBranchV1", args, &dto); err != nil {
		return dto, err
	}
	return dto, nil
}

// OpenPullRequest asks the deployment-server to open a PR/MR via the
// user's git provider for the given installation. Provider dispatch
// (GitHub/GitLab/BitBucket) and REST calls happen server-side; the
// runner only carries the call across the wire and surfaces the result.
//
// repoName is the provider's expected identifier:
//   - GitHub:    "owner/repo"
//   - GitLab:    "namespace/path" (server URL-encodes); numeric ID also works
//   - BitBucket: "workspace/repo_slug"
//
// Returns (prURL, prNumber, error). prURL is the user-facing web URL
// (e.g., https://github.com/.../pull/42); prNumber is the
// provider-scoped PR/MR number the user would type in chat.
func (r *RunnerClient) OpenPullRequest(organizationID, installationID, repoName,
	baseBranch, headBranch, title, body string) (string, int, error) {
	if !r.isConnected {
		return "", 0, ErrConnection
	}
	args := oauth.OpenPullRequestArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.InstallationID = installationID
	args.RepoName = repoName
	args.BaseBranch = baseBranch
	args.HeadBranch = headBranch
	args.Title = title
	args.Body = body
	var dto oauth.OpenPullRequestDtoV1
	if err := r.c.Call("Oauth.OpenPullRequestV1", args, &dto); err != nil {
		return "", 0, err
	}
	return dto.URL, dto.Number, nil
}
