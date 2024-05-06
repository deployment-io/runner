package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/previews"
)

func (r *RunnerClient) UpdatePreviews(updatePreviews []previews.UpdatePreviewDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := previews.UpdatePreviewsArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID()
	args.Token = r.token
	args.Previews = updatePreviews
	var reply previews.UpdatePreviewsReplyV1
	err := r.c.Call("Previews.UpdateV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}

func (r *RunnerClient) GetPreviewData(previewIDs []string) ([]previews.GetPreviewDtoV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := previews.GetPreviewsArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID()
	args.Token = r.token
	args.IDs = previewIDs
	var reply previews.GetPreviewsReplyV1
	err := r.c.Call("Previews.GetV1", args, &reply)
	if err != nil {
		return nil, err
	}
	return reply.Previews, nil
}
