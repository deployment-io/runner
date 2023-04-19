package commands

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

func Get(p commands_enums.Type) (jobs.Command, error) {
	switch p {
	case commands_enums.CheckoutRepo:
		return &CheckoutRepository{}, nil
	case commands_enums.BuildStaticSite:
		return &BuildStaticSite{}, nil
	}
	return nil, fmt.Errorf("error getting command for %s", p)
}
