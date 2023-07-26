package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/certificates"
)

func (r *RunnerClient) UpdateCertificates(updateCertificates []certificates.UpdateCertificateDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := certificates.UpdateCertificatesArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.Certificates = updateCertificates
	var reply certificates.UpdateCertificatesReplyV1
	err := r.c.Call("Certificates.UpdateV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
