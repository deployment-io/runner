package main

import (
	"github.com/joho/godotenv"
	"os"
)

func getEnvironmentForSaas() (service, organizationId, token, region, dockerImage, memory, taskExecutionRoleArn,
	taskRoleArn, awsAccountID string) {
	//TODO load .env
	//ignoring err
	_ = godotenv.Load()
	organizationId = os.Getenv("OrganizationID")
	service = os.Getenv("Service")
	token = os.Getenv("Token")
	region = os.Getenv("Region")
	dockerImage = os.Getenv("DockerImage")
	memory = os.Getenv("Memory")
	taskExecutionRoleArn = os.Getenv("ExecutionRoleArn")
	taskRoleArn = os.Getenv("TaskRoleArn")
	awsAccountID = os.Getenv("AWSAccountID")
	return
}
