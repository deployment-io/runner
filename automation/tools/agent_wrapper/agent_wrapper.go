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
	AgentRole       string
	AgentGoal       string
	AgentBackstory  string
	AgentLLM        string
	LogsWriter      io.Writer
	CallbackHandler callbacks.Handler
}

const maxIterations = 10

func (t *Tool) newAgent() (llm_implementations.AgentInterface, error) {
	httpClient := rpc.NewHTTPClient(rpcs.AzureOpenAI, false, true, 2)
	return llm_implementations.Get(llm_implementation_enums.OpenAIFunctionAgent, agent_options.WithBackstory(t.AgentBackstory),
		agent_options.WithRole(t.AgentRole),
		agent_options.WithMaxIterations(maxIterations),
		agent_options.WithLLM(t.AgentLLM),
		agent_options.WithHttpClient(httpClient),
		agent_options.WithCallbackHandler(t.CallbackHandler),
	)
}

func (t *Tool) Name() string {
	return t.AgentName
}

func (t *Tool) Description() string {
	return t.AgentGoal
}

func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	agent, err := t.newAgent()
	if err != nil {
		io.WriteString(t.LogsWriter, fmt.Sprintf("Error getting agent with role: %s : %s\n", t.AgentRole, err))
		return "There was an error. We'll get back to you.", nil
	}
	output, err := agent.Do(ctx, input, agent_options.WithToolChoice("auto"),
		agent_options.WithTools(t.AgentTools), agent_options.WithCallback(t.CallbackHandler))
	if err != nil {
		io.WriteString(t.LogsWriter, fmt.Sprintf("Error getting output for the agent with role: %s : %s\n", t.AgentRole, err))
		return "There was an error. We'll get back to you.", nil
	}
	return output, nil
}

var _ tools.Tool = &Tool{}
