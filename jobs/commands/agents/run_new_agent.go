package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/ankit-arora/langchaingo/memory"
	"github.com/ankit-arora/langchaingo/tools"
	agentTypes "github.com/deployment-io/deployment-runner-kit/agents"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/agent/callbacks"
	"github.com/deployment-io/deployment-runner/agent/memory/agent_runner_chat_history"
	"github.com/deployment-io/deployment-runner/agent/memory/file_store"
	runnerTools "github.com/deployment-io/deployment-runner/agent/tools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/team-ai/agents"
	"github.com/deployment-io/team-ai/enums/agent_enums"
	"github.com/deployment-io/team-ai/options/agent_options"
	"github.com/go-playground/validator/v10"
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

type RunNewAgent struct {
}

func getToolForNode(nodeID string, nodesMap map[string]agentTypes.NodeDtoV1, visited map[string]bool,
	parameters map[string]interface{}, logsWriter io.Writer, handler *callbacks.AgentRunHandler,
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
		toolWrappedOnAgent := runnerTools.GetToolWrappedOnAgent(agentTools, currentNode.ID, currentNode.Name,
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
			toolWrappedOnAgent := runnerTools.GetToolWrappedOnAgent(nil, currentNode.ID, currentNode.Name,
				currentNode.Goal, currentNode.Backstory, agentLlm, agentLlmVersion, logsWriter, handler)
			return toolWrappedOnAgent, nil
		}
	}
	return nil, fmt.Errorf("node %s doesn't have any children", currentNode.ID)
}

type AgentOutput struct {
	Output   string `json:"output" validate:"required"`
	NeedHelp *bool  `json:"need_help" validate:"required"`
}

func parseAndValidateAgentResponse(response string, agentOutput *AgentOutput) error {
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
	agentOutput.Output = o
	agentOutput.NeedHelp = &nh
	// Validate the struct
	validate := validator.New()
	if err := validate.Struct(agentOutput); err != nil {
		return fmt.Errorf("validation error: %w", err)
	}
	return nil
}

func (r *RunNewAgent) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	organizationIdFromJob, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	jobID, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	agentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.AgentID)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	agentData, err := jobs.GetParameterValue[agentTypes.AgentDataDtoV1](parameters, parameters_enums.AgentData)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	openAIAPIKey := agentData.OpenAIAPIKey
	//TODO set open ai api key in environment for now
	err = os.Setenv("OPENAI_API_KEY", openAIAPIKey)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	openAIBaseUrl := agentData.OpenAIBaseUrl
	err = os.Setenv("OPENAI_BASE_URL", openAIBaseUrl)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	//used as a singleton
	debug, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.DebugAgent)
	debugOpenAICalls, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.DebugOpenAICallsInAgent)
	agentRunHandler := &callbacks.AgentRunHandler{
		LogsWriter:       logsWriter,
		Debug:            debug,
		DebugOpenAICalls: debugOpenAICalls,
	}
	agentLlmType := agentData.LlmModelType.String()
	agentLlmApiVersion := agentData.LlmApiVersion.String()
	agentRunner, err := agents.GetAgentToAssist(agent_enums.AgentRunner, agentLlmType, agentLlmApiVersion,
		"", agentRunHandler)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	var input string
	if agentData.ExtraHelpAvailable {
		input = fmt.Sprintf("Following extra information is available to run the goal. Information is: %s", agentData.Goal)
	} else {
		input = fmt.Sprintf("Run agent to achieve the provided goal based on the shared information and tools. Goal is: %s", agentData.Goal)
	}
	var agentTools []tools.Tool
	startNode, startNodeExists := agentData.NodesMap[agentData.StartNodeID]
	if !startNodeExists {
		err = fmt.Errorf("start node doesn't exists")
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	for _, childNodeID := range startNode.Children {
		var toolForNode tools.Tool
		toolForNode, err = getToolForNode(childNodeID, agentData.NodesMap, map[string]bool{}, parameters, logsWriter,
			agentRunHandler, debugOpenAICalls)
		if err != nil {
			commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
				JobID:    jobID,
				Output:   "There was an error running your agent. Please try again later.",
				NeedHelp: false,
			})
			return parameters, err
		}
		if toolForNode != nil {
			agentTools = append(agentTools, toolForNode)
		}
	}
	err = file_store.AddChatHistory(jobID, agentID, agentData.ChatHistory)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	defer file_store.DeleteFile(jobID)
	agentChatHistory, err := agent_runner_chat_history.NewAgentRunnerChatHistory(jobID, agentID)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	agentConversationBuffer := memory.NewConversationBuffer(memory.WithMemoryKey("chat_history"),
		memory.WithReturnMessages(true), memory.WithChatHistory(agentChatHistory))
	var agentOptions []agent_options.Execution
	agentOptions = append(agentOptions, agent_options.WithTools(agentTools), agent_options.WithMemory(agentConversationBuffer),
		agent_options.WithJSONMode(true))
	// Only set tool choice if we have tools
	if len(agentTools) > 0 {
		agentOptions = append(agentOptions, agent_options.WithToolChoice("auto"))
	}
	output, err := agentRunner.Do(context.TODO(), input, agentOptions...)
	if err != nil {
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, err
	}
	//check if output is in the proper json format. If not call the agent agent again to get the data back in correct format
	numRetires := 10
	agentOutput := &AgentOutput{}
	jsonParseErr := parseAndValidateAgentResponse(output, agentOutput)
	for jsonParseErr != nil && numRetires > 0 {
		newInput := fmt.Sprintf("We got an error while parsing the response. The error is : %s. %s", jsonParseErr.Error(),
			responseFormatPrompt)
		var agentErr error
		output, agentErr = agentRunner.Do(context.TODO(), newInput, agentOptions...)
		if agentErr != nil {
			return parameters, agentErr
		}
		agentOutput = &AgentOutput{}
		jsonParseErr = parseAndValidateAgentResponse(output, agentOutput)
		numRetires--
	}
	if jsonParseErr != nil {
		//send the output as an error status
		commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
			JobID:    jobID,
			Output:   "There was an error running your agent. Please try again later.",
			NeedHelp: false,
		})
		return parameters, jsonParseErr
	}
	//send the output and status back
	commandUtils.UpdateAgentOutputPipeline.Add(organizationIdFromJob, agentTypes.UpdateResponseDtoV1{
		JobID:    jobID,
		Output:   agentOutput.Output,
		NeedHelp: *agentOutput.NeedHelp,
	})
	return parameters, nil
}
