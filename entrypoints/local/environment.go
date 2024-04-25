package main

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/joho/godotenv"
	"os"
	"strings"
)

var serviceFromBuild string

func getEnvironmentForLocal() (userId, organizationId, token, service string, targetCloud runner_enums.TargetCloud, err error) {
	//ignoring err
	_ = godotenv.Load()
	userKey := os.Getenv("UserKey")
	userKeySplit := strings.Split(userKey, ":")
	if len(userKeySplit) != 2 {
		err = fmt.Errorf("invalid user key")
		return
	}
	userId = userKeySplit[0]
	organizationId = userKeySplit[1]
	targetCloudStr := os.Getenv("TargetCloud")
	targetCloud, err = runner_enums.GetTargetCloudFromString(targetCloudStr)
	if err != nil {
		return
	}
	token = os.Getenv("UserSecret")
	if len(token) == 0 {
		err = fmt.Errorf("invalid token")
		return
	}
	if len(serviceFromBuild) > 0 {
		service = serviceFromBuild
	} else {
		service = os.Getenv("Service")
	}
	if len(service) == 0 {
		err = fmt.Errorf("invalid service")
		return
	}
	return
}
