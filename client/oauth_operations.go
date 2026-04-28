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
