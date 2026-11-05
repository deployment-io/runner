package commands

import (
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/agenttools"
)

// A Task's backend preview runs its service's preview seed command right after
// the preview migrate command, on every deploy, as a one-off task the same way:
// it creates a test user for each of the service's roles, whose email and
// password it gets in its task's environment. Unlike the migrate command, its
// failure doesn't stop the deploy: the preview then has no test users.

const previewSeedCommandName = "preview seed command"

// previewSeedRun is the seed a backend preview's deploy runs; the deploy sets
// outcome.
type previewSeedRun struct {
	seed    agenttools.WebServicePreviewSeed
	outcome agenttools.PreviewSeedOutcome
}

// runPreviewSeedCommand runs seed's command, if it has one and the Job isn't a
// restart, and records how it ended on seed. A failure is only written to the
// deploy logs.
func runPreviewSeedCommand(parameters map[string]interface{}, ecsClient preDeployEcsAPI, logsClient preDeployLogsAPI,
	ecsClusterArn, taskDefinitionArn string, logsWriter io.Writer, seed *previewSeedRun) {
	if seed == nil || strings.TrimSpace(seed.seed.Command) == "" {
		return
	}
	if restartOnly, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.RestartOnly); restartOnly {
		return
	}
	environment, passwords, clash := previewSeedEnvironment(seed.seed.Users)
	var failure string
	var err error
	if len(clash) > 0 {
		failure = fmt.Sprintf("The %s didn't run: %s.", previewSeedCommandName, clash)
	} else {
		failure, err = runOneOffTask(parameters, ecsClient, logsClient, oneOffTask{
			ecsClusterArn:     ecsClusterArn,
			taskDefinitionArn: taskDefinitionArn,
			command:           seed.seed.Command,
			name:              previewSeedCommandName,
			environment:       environment,
			// A seed that prints a password shows <QA_ROLE_<KEY>_PASSWORD> instead.
			hidden:     passwords,
			logsWriter: logsWriter,
		})
	}
	if err != nil {
		failure = fmt.Sprintf("The %s couldn't start: %s.", previewSeedCommandName, err)
	}
	if len(failure) > 0 {
		seed.outcome = agenttools.PreviewSeedFailed
		writePreDeployLog(logsWriter, failure+" The preview has no test users; the deploy goes on.\n")
		return
	}
	seed.outcome = agenttools.PreviewSeedSucceeded
}

// previewSeedEnvironment is what the seed's task gets on top of the container's
// environment: QA_ROLES, the role names separated by spaces, and each role's
// QA_ROLE_<KEY>_EMAIL and QA_ROLE_<KEY>_PASSWORD; and each password by the name
// of its variable, to hide in the seed's output. clash, when set, says which two
// roles share a <KEY> (billing-admin and billing_admin), whose credentials the
// seed couldn't tell apart.
func previewSeedEnvironment(users []agenttools.PreviewTestUser) (environment []ecsTypes.KeyValuePair, passwords map[string]string, clash string) {
	roles := make([]string, 0, len(users))
	environment = make([]ecsTypes.KeyValuePair, 0, 1+2*len(users))
	passwords = make(map[string]string, len(users))
	roleByKey := make(map[string]string, len(users))
	for _, user := range users {
		roles = append(roles, user.Role)
	}
	environment = append(environment, ecsTypes.KeyValuePair{Name: aws.String("QA_ROLES"), Value: aws.String(strings.Join(roles, " "))})
	for _, user := range users {
		key := agenttools.PreviewRoleEnvKey(user.Role)
		if other, ok := roleByKey[key]; ok {
			return nil, nil, fmt.Sprintf("the roles %s and %s would both use QA_ROLE_%s_EMAIL and QA_ROLE_%s_PASSWORD",
				other, user.Role, key, key)
		}
		roleByKey[key] = user.Role
		prefix := "QA_ROLE_" + key
		environment = append(environment,
			ecsTypes.KeyValuePair{Name: aws.String(prefix + "_EMAIL"), Value: aws.String(user.Email)},
			ecsTypes.KeyValuePair{Name: aws.String(prefix + "_PASSWORD"), Value: aws.String(user.Password)})
		passwords[prefix+"_PASSWORD"] = user.Password
	}
	return environment, passwords, ""
}
