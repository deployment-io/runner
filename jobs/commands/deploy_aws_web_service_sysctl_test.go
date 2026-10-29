package commands

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

func TestAllowUnprivilegedPortsLinux(t *testing.T) {
	def := ecsTypes.ContainerDefinition{
		Name:       aws.String("app"),
		Privileged: aws.Bool(false),
		PortMappings: []ecsTypes.PortMapping{{
			ContainerPort: aws.Int32(80),
		}},
	}
	allowUnprivilegedPorts(&def, ecsTypes.OSFamilyLinux)

	if len(def.SystemControls) != 1 {
		t.Fatalf("SystemControls = %d entries, want 1", len(def.SystemControls))
	}
	sc := def.SystemControls[0]
	if aws.ToString(sc.Namespace) != "net.ipv4.ip_unprivileged_port_start" || aws.ToString(sc.Value) != "0" {
		t.Fatalf("SystemControl = %s=%s, want net.ipv4.ip_unprivileged_port_start=0", aws.ToString(sc.Namespace), aws.ToString(sc.Value))
	}
	if aws.ToBool(def.Privileged) {
		t.Fatal("Privileged must stay false")
	}

	// Applying it again (e.g. after customize functions) must not duplicate the entry.
	allowUnprivilegedPorts(&def, ecsTypes.OSFamilyLinux)
	if len(def.SystemControls) != 1 {
		t.Fatalf("SystemControls = %d entries after reapply, want 1", len(def.SystemControls))
	}
}

func TestAllowUnprivilegedPortsWindows(t *testing.T) {
	def := ecsTypes.ContainerDefinition{Name: aws.String("app")}
	allowUnprivilegedPorts(&def, ecsTypes.OSFamilyWindowsServer2022Core)
	if len(def.SystemControls) != 0 {
		t.Fatalf("SystemControls = %v, want none for Windows", def.SystemControls)
	}
}
