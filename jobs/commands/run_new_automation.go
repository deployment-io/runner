package commands

import (
	"encoding/json"
	"fmt"
	"github.com/ankit-arora/langchaingo/memory"
	"github.com/ankit-arora/langchaingo/tools"
	"github.com/deployment-io/deployment-runner-kit/automations"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/automation/callbacks"
	"github.com/deployment-io/deployment-runner/automation/memory/automation_agent_chat_history"
	"github.com/deployment-io/deployment-runner/automation/memory/file_store"
	runnerTools "github.com/deployment-io/deployment-runner/automation/tools"
	"github.com/deployment-io/team-ai/agents"
	"github.com/deployment-io/team-ai/enums/agent_enums"
	"github.com/deployment-io/team-ai/options/agent_options"
	"github.com/go-playground/validator/v10"
	"golang.org/x/net/context"
	"io"
	"os"
)

const responseFormatPrompt = `Please make sure your response follows a structured JSON object format, consisting of the following keys:
1. "output" (string):
   A clear and concise message that communicates the action taken, the results, or the next steps required. Include explanations for the user when additional context or feedback is needed.

2. "need_help" (boolean):
   Indicates whether further input or clarification is required from the user to proceed.

Response Format Example:
{
  "output": "We have created a new branch called 'feature/new-feature' and pushed it to the remote repository.",
  "need_help": false
}
`

type RunNewAutomation struct {
}

func getToolForNode(nodeID string, nodesMap map[string]automations.NodeDtoV1, visited map[string]bool,
	parameters map[string]interface{}, logsWriter io.Writer, handler *callbacks.AutomationRunHandler,
	debugOpenAICalls bool) (tools.Tool, error) {
	if visited[nodeID] {
		return nil, nil
	}
	visited[nodeID] = true
	currentNode := nodesMap[nodeID]
	// Process the current node here
	if len(currentNode.Children) > 0 {
		if currentNode.NodeType.IsToolType() || currentNode.NodeType.IsTriggerType() {
			//return error
			return nil, fmt.Errorf("node %s is trigger or agent type with children", currentNode.ID)
		}
		//only agent type will have children since we have handled the trigger node in the caller
		var agentTools []tools.Tool
		for _, childNodeID := range currentNode.Children {
			toolForAgentNode, err := getToolForNode(childNodeID, nodesMap, visited, parameters, logsWriter, handler,
				debugOpenAICalls)
			if err != nil {
				return nil, err
			}
			if toolForAgentNode != nil {
				agentTools = append(agentTools, toolForAgentNode)
			}
		}
		//wrap agent node into a tool and return
		agentLlm := currentNode.LlmModelType.String()
		agentLlmVersion := currentNode.LlmApiVersion.String()
		toolWrappedOnAgent := runnerTools.GetToolWrappedOnAgent(agentTools, currentNode.ID,
			currentNode.Goal, currentNode.Backstory, agentLlm, agentLlmVersion, logsWriter, handler)
		return toolWrappedOnAgent, nil
	} else {
		if currentNode.NodeType.IsToolType() {
			nodeTool, err := runnerTools.GetToolFromType(currentNode.ToolType, runnerTools.Options{
				Parameters:       parameters,
				LogsWriter:       logsWriter,
				CallbacksHandler: handler,
				DebugOpenAICalls: debugOpenAICalls,
			})
			if err != nil {
				return nil, err
			}
			return nodeTool, nil
		} else if currentNode.NodeType.IsAgentType() {
			//wrap agent node into a tool and return
			agentLlm := currentNode.LlmModelType.String()
			agentLlmVersion := currentNode.LlmApiVersion.String()
			toolWrappedOnAgent := runnerTools.GetToolWrappedOnAgent(nil, currentNode.ID,
				currentNode.Goal, currentNode.Backstory, agentLlm, agentLlmVersion, logsWriter, handler)
			return toolWrappedOnAgent, nil
		}
	}
	return nil, fmt.Errorf("node %s doesn't have any children", currentNode.ID)
}

type AutomationAgentOutput struct {
	Output   string `json:"output" validate:"required"`
	NeedHelp *bool  `json:"need_help" validate:"required"`
}

func parseAndValidateAutomationAgentResponse(response string, automationAgentOutput *AutomationAgentOutput) error {
	// Parse the JSON response into a map
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(response), &raw); err != nil {
		return fmt.Errorf("failed to parse JSON: %w", err)
	}
	output, ok := raw["output"]
	if !ok {
		return fmt.Errorf("output key not found")
	}
	needHelp, ok := raw["need_help"]
	if !ok {
		return fmt.Errorf("need_help key not found")
	}
	o, ok := output.(string)
	if !ok {
		return fmt.Errorf("output is not a string")
	}
	nh, ok := needHelp.(bool)
	if !ok {
		return fmt.Errorf("need_help is not a boolean")
	}
	// Map the parsed response into the struct
	automationAgentOutput.Output = o
	automationAgentOutput.NeedHelp = &nh
	// Validate the struct
	validate := validator.New()
	if err := validate.Struct(automationAgentOutput); err != nil {
		return fmt.Errorf("validation error: %w", err)
	}
	return nil
}

func (r *RunNewAutomation) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	organizationIdFromJob, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	jobID, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	automationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.AutomationID)
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	automationData, err := jobs.GetParameterValue[automations.AutomationDataDtoV1](parameters, parameters_enums.AutomationData)
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	openAIAPIKey := automationData.OpenAIAPIKey
	//TODO set open ai api key in environment for now
	err = os.Setenv("OPENAI_API_KEY", openAIAPIKey)
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	openAIBaseUrl := automationData.OpenAIBaseUrl
	err = os.Setenv("OPENAI_BASE_URL", openAIBaseUrl)
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	//used as a singleton
	debug, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.DebugAutomation)
	debugOpenAICalls, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.DebugOpenAICallsInAutomation)
	automationRunHandler := &callbacks.AutomationRunHandler{
		LogsWriter:       logsWriter,
		Debug:            debug,
		DebugOpenAICalls: debugOpenAICalls,
	}
	automationLlmType := automationData.LlmModelType.String()
	automationLlmApiVersion := automationData.LlmApiVersion.String()
	automationAgent, err := agents.GetAgentToAssist(agent_enums.AutomationAgent, automationLlmType, automationLlmApiVersion,
		"", automationRunHandler)
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	var input string
	if automationData.ExtraHelpAvailable {
		input = fmt.Sprintf("Following extra information is available to run the goal. Information is: %s", automationData.Goal)
	} else {
		input = fmt.Sprintf("Run automation to achieve the provided goal based on the shared information and tools. Goal is: %s", automationData.Goal)
	}
	var automationTools []tools.Tool
	startNode, startNodeExists := automationData.NodesMap[automationData.StartNodeID]
	if !startNodeExists {
		err = fmt.Errorf("start node doesn't exists")
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	for _, childNodeID := range startNode.Children {
		var toolForNode tools.Tool
		toolForNode, err = getToolForNode(childNodeID, automationData.NodesMap, map[string]bool{}, parameters, logsWriter,
			automationRunHandler, debugOpenAICalls)
		if err != nil {
			updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
				JobID:    jobID,
				Output:   "There was an error running your automation. Please try again later.",
				NeedHelp: false,
			})
			return parameters, err
		}
		if toolForNode != nil {
			automationTools = append(automationTools, toolForNode)
		}
	}
	err = file_store.AddChatHistory(jobID, automationID, automationData.ChatHistory)
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	defer file_store.DeleteFile(jobID)
	automationChatHistory, err := automation_agent_chat_history.NewAutomationAgentChatMessageHistory(jobID, automationID)
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	automationConversationBuffer := memory.NewConversationBuffer(memory.WithMemoryKey("chat_history"),
		memory.WithReturnMessages(true), memory.WithChatHistory(automationChatHistory))
	output, err := automationAgent.Do(context.TODO(), input, agent_options.WithToolChoice("auto"),
		agent_options.WithTools(automationTools), agent_options.WithMemory(automationConversationBuffer),
		agent_options.WithJSONMode(true))
	if err != nil {
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	//check if output is in the proper json format. If not call the automation agent again to get the data back in correct format
	numRetires := 10
	automationAgentOutput := &AutomationAgentOutput{}
	jsonParseErr := parseAndValidateAutomationAgentResponse(output, automationAgentOutput)
	for jsonParseErr != nil && numRetires > 0 {
		newInput := fmt.Sprintf("We got an error while parsing the response. The error is : %s. %s", jsonParseErr.Error(),
			responseFormatPrompt)
		var agentErr error
		output, agentErr = automationAgent.Do(context.TODO(), newInput, agent_options.WithToolChoice("auto"),
			agent_options.WithTools(automationTools), agent_options.WithMemory(automationConversationBuffer))
		if agentErr != nil {
			return parameters, agentErr
		}
		automationAgentOutput = &AutomationAgentOutput{}
		jsonParseErr = parseAndValidateAutomationAgentResponse(output, automationAgentOutput)
		numRetires--
	}
	if jsonParseErr != nil {
		//send the output as an error status
		updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your automation. Please try again later.",
			NeedHelp: false,
		})
		return parameters, jsonParseErr
	}
	//send the output and status back
	updateAutomationOutputPipeline.Add(organizationIdFromJob, automations.UpdateResponseDtoV1{
		JobID:    jobID,
		Output:   automationAgentOutput.Output,
		NeedHelp: *automationAgentOutput.NeedHelp,
	})
	return parameters, nil
}
