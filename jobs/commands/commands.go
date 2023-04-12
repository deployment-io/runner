package commands

import (
	"fmt"
	"github.com/deployment-io/jobs-runner-kit/enums/commands_enums"
	"github.com/deployment-io/jobs-runner-kit/jobs/types"
)

func Get(p commands_enums.Type) (types.Command, error) {
	switch p {
	case commands_enums.CheckoutRepo:
		return &CheckoutRepository{}, nil
	}
	return nil, fmt.Errorf("error getting command for %s", p)
}
