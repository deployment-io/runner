package agent_wrapper

import (
	"context"
	"fmt"
	"github.com/ankit-arora/langchaingo/callbacks"
	"github.com/ankit-arora/langchaingo/tools"
	"github.com/deployment-io/team-ai/enums/llm_implementation_enums"
	"github.com/deployment-io/team-ai/enums/rpcs"
	"github.com/deployment-io/team-ai/llm_implementations"
	"github.com/deployment-io/team-ai/options/agent_options"
	"github.com/deployment-io/team-ai/rpc"
	"io"
)

type Tool struct {
	AgentTools      []tools.Tool
	AgentName       string
	AgentGoal       string
	AgentBackstory  string
	AgentLLM        string
	AgentApiVersion string
	LogsWriter      io.Writer
	CallbackHandler callbacks.Handler
}

const maxIterations = 10

func (t *Tool) newAgent() (llm_implementations.AgentInterface, error) {
	httpClient := rpc.NewHTTPClient(rpcs.AzureOpenAI, true, true, 2)
	return llm_implementations.Get(llm_implementation_enums.OpenAIFunctionAgent, agent_options.WithBackstory(t.AgentBackstory),
		agent_options.WithMaxIterations(maxIterations),
		agent_options.WithLLM(t.AgentLLM),
		agent_options.WithApiVersion(t.AgentApiVersion),
		agent_options.WithHttpClient(httpClient),
		agent_options.WithCallbackHandler(t.CallbackHandler),
	)
}

func (t *Tool) Name() string {
	return t.AgentName
}

func (t *Tool) getCustomAgentToolsInfo() string {
	toolsInfo := ""
	for _, tool := range t.AgentTools {
		toolsInfo += "Name : " + tool.Name() + "\n" + "Description : " + tool.Description() + "\n"
	}
	return toolsInfo
}

func (t *Tool) Description() string {
	description := `Calls a custom AI agent with the following goal: %s
The custom agent has access to the following tools and can use them to complete the goal: 
%s`
	description = fmt.Sprintf(description, t.AgentGoal, t.getCustomAgentToolsInfo())
	return description
}

func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	agent, err := t.newAgent()
	if err != nil {
		io.WriteString(t.LogsWriter, fmt.Sprintf("Error getting agent with name: %s : %s\n", t.AgentName, err))
		return "There was an error. We'll get back to you.", nil
	}
	output, err := agent.Do(ctx, input, agent_options.WithToolChoice("auto"),
		agent_options.WithTools(t.AgentTools), agent_options.WithCallback(t.CallbackHandler))
	if err != nil {
		io.WriteString(t.LogsWriter, fmt.Sprintf("Error getting output for the agent with name: %s : %s\n", t.AgentName, err))
		return "There was an error. We'll get back to you.", nil
	}
	return output, nil
}

var _ tools.Tool = &Tool{}
