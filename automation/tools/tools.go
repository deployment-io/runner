package tools

import (
	"fmt"
	"github.com/ankit-arora/langchaingo/callbacks"
	"github.com/ankit-arora/langchaingo/tools"
	"github.com/deployment-io/deployment-runner-kit/enums/automation_enums"
	"github.com/deployment-io/deployment-runner/automation/tools/agent_wrapper"
	"github.com/deployment-io/deployment-runner/automation/tools/code_tools/query_code"
	"github.com/deployment-io/deployment-runner/automation/tools/get_application_logs"
	"github.com/deployment-io/deployment-runner/automation/tools/get_cpu_memory_usage"
	"github.com/deployment-io/deployment-runner/automation/tools/send_email"
	"io"
)

func GetToolFromType(toolType automation_enums.ToolType, options Options) (tools.Tool, error) {
	switch toolType {
	case automation_enums.GetCPUMemoryUsage:
		return &get_cpu_memory_usage.Tool{
			Parameters:       options.Parameters,
			LogsWriter:       options.LogsWriter,
			CallbacksHandler: options.CallbacksHandler,
			Entities:         toolType.GetEntities(),
		}, nil
	case automation_enums.SendEmail:
		tool, err := send_email.NewTool(options.Parameters, options.LogsWriter, options.CallbacksHandler,
			options.DebugOpenAICalls)
		if err != nil {
			return nil, err
		}
		return tool, nil
	case automation_enums.GetApplicationLogs:
		return &get_application_logs.Tool{
			Params:           options.Parameters,
			LogsWriter:       options.LogsWriter,
			CallbacksHandler: options.CallbacksHandler,
			Entities:         toolType.GetEntities(),
			DebugOpenAICalls: options.DebugOpenAICalls,
		}, nil
	case automation_enums.QueryCode:
		return &query_code.Tool{
			Params:           options.Parameters,
			LogsWriter:       options.LogsWriter,
			CallbacksHandler: options.CallbacksHandler,
			Entities:         toolType.GetEntities(),
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

func GetToolWrappedOnAgent(agentTools []tools.Tool, agentName, agentGoal, agentBackstory, llm string,
	logsWriter io.Writer, handler callbacks.Handler) tools.Tool {
	return &agent_wrapper.Tool{
		AgentTools:      agentTools,
		AgentName:       agentName,
		AgentGoal:       agentGoal,
		AgentBackstory:  agentBackstory,
		AgentLLM:        llm,
		LogsWriter:      logsWriter,
		CallbackHandler: handler,
	}
}
