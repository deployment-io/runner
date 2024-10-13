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
