package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/notifications"
)

func (r *RunnerClient) SendNotifications(sendNotifications []notifications.SendNotificationDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := notifications.SendNotificationsArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.Notifications = sendNotifications
	var reply notifications.SendNotificationsReplyV1
	err := r.c.Call("Notifications.SendV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
