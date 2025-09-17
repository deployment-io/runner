package tools

import (
	"fmt"
	"io"

	"github.com/ankit-arora/langchaingo/callbacks"
	"github.com/ankit-arora/langchaingo/tools"
	"github.com/deployment-io/deployment-runner-kit/enums/agent_enums"
	"github.com/deployment-io/deployment-runner/agent/tools/agent_wrapper"
	"github.com/deployment-io/deployment-runner/agent/tools/code_tools/query_code"
	"github.com/deployment-io/deployment-runner/agent/tools/get_application_logs"
	"github.com/deployment-io/deployment-runner/agent/tools/get_cpu_memory_usage"
	"github.com/deployment-io/deployment-runner/agent/tools/send_email"
)

func GetToolFromType(toolType agent_enums.ToolType, options Options) (tools.Tool, error) {
	switch toolType {
	case agent_enums.GetCPUMemoryUsage:
		return &get_cpu_memory_usage.Tool{
			Parameters:       options.Parameters,
			LogsWriter:       options.LogsWriter,
			CallbacksHandler: options.CallbacksHandler,
		}, nil
	case agent_enums.SendEmail:
		tool, err := send_email.NewTool(options.Parameters, options.LogsWriter, options.CallbacksHandler,
			options.DebugOpenAICalls)
		if err != nil {
			return nil, err
		}
		return tool, nil
	case agent_enums.GetApplicationLogs:
		return &get_application_logs.Tool{
			Params:           options.Parameters,
			LogsWriter:       options.LogsWriter,
			CallbacksHandler: options.CallbacksHandler,
			DebugOpenAICalls: options.DebugOpenAICalls,
		}, nil
	case agent_enums.QueryCode:
		return &query_code.Tool{
			Params:           options.Parameters,
			LogsWriter:       options.LogsWriter,
			CallbacksHandler: options.CallbacksHandler,
			DebugOpenAICalls: options.DebugOpenAICalls,
		}, nil
	default:
		return nil, fmt.Errorf("tool type %s not supported", toolType.String())
	}
}

type Options struct {
	Parameters       map[string]interface{}
	LogsWriter       io.Writer
	CallbacksHandler callbacks.Handler
	DebugOpenAICalls bool
}

func GetToolWrappedOnAgent(agentTools []tools.Tool, agentID, agentName, agentGoal, agentBackstory, llm, llmVersion string,
	logsWriter io.Writer, handler callbacks.Handler) tools.Tool {
	return &agent_wrapper.Tool{
		AgentTools:      agentTools,
		AgentName:       agentName,
		AgentID:         agentID,
		AgentGoal:       agentGoal,
		AgentBackstory:  agentBackstory,
		AgentLLM:        llm,
		AgentApiVersion: llmVersion,
		LogsWriter:      logsWriter,
		CallbackHandler: handler,
	}
}
