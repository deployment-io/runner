package hostinfo

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

// imdsTimeout bounds the metadata lookup. IMDS is a link-local address
// that answers in single-digit milliseconds when present and hangs when
// absent, so this is short on purpose: the instance type is a display
// nicety and must never delay a runner's first ping to the control
// plane, which is what marks the runner alive.
const imdsTimeout = 2 * time.Second

var (
	instanceTypeOnce  sync.Once
	instanceTypeCache string
)

// InstanceType returns the EC2 instance type of the host (e.g.
// "m6a.large"), or an empty string on anything that is not an EC2
// instance or where IMDS is unreachable.
//
// Best-effort by design. Every caller treats the empty string as "not
// known" and the model field is omitempty, so a failure here degrades to
// a blank column rather than an error.
//
// Note this is the RUNNER reaching IMDS, which is allowed and expected.
// It is the agentbox and build CONTAINERS that are firewalled off from
// 169.254.169.254 (see the ExtraHosts pins in run_agent_step.go), so
// that untrusted agent or build code cannot reach the host's IAM
// credentials. Nothing here is exposed to those containers.
func InstanceType() string {
	instanceTypeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), imdsTimeout)
		defer cancel()

		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return
		}
		out, err := imds.NewFromConfig(cfg).GetMetadata(ctx, &imds.GetMetadataInput{
			Path: "instance-type",
		})
		if err != nil {
			return
		}
		defer out.Content.Close()

		buf := make([]byte, 64)
		n, _ := out.Content.Read(buf)
		if n > 0 {
			instanceTypeCache = string(buf[:n])
		}
	})
	return instanceTypeCache
}
