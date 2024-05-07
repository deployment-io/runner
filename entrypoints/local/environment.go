package main

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/joho/godotenv"
	"os"
	"strings"
)

func getEnvironmentForLocal() (userId, token, service string, targetCloud runner_enums.TargetCloud, err error) {
	//ignoring err
	_ = godotenv.Load()
	userKey := os.Getenv("UserKey")
	userId, found := strings.CutPrefix(userKey, "du_")
	if !found {
		err = fmt.Errorf("invalid user key. Please get a valid user key from https://app.deployment.io")
		return
	}
	token = os.Getenv("UserSecret")
	if len(token) == 0 {
		err = fmt.Errorf("invalid user secret. Please get a valid user secret from https://app.deployment.io")
		return
	}
	targetCloudStr := os.Getenv("TargetCloud")
	targetCloud, err = runner_enums.GetTargetCloudFromString(targetCloudStr)
	if err != nil {
		return
	}
	if len(os.Getenv("Service")) > 0 {
		service = os.Getenv("Service")
	} else {
		service = serviceFromBuild
	}
	if len(service) == 0 {
		err = fmt.Errorf("invalid service")
		return
	}
	return
}
