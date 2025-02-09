package callbacks

import (
	"context"
	"fmt"
	"github.com/ankit-arora/langchaingo/callbacks"
	"github.com/ankit-arora/langchaingo/llms"
	"io"
	"strings"
	"sync"
)

// AutomationRunHandler is a callback handler used for observing and logging LLM runs for an automation.
type AutomationRunHandler struct {
	callbacks.SimpleHandler
	LogsWriter       io.Writer
	Debug            bool
	DebugOpenAICalls bool
	sync.Mutex
}

var _ callbacks.Handler = &AutomationRunHandler{}

func (a *AutomationRunHandler) HandleToolStart(_ context.Context, info string) {
	if a.Debug {
		a.Lock()
		defer a.Unlock()
		io.WriteString(a.LogsWriter, fmt.Sprintf("%s:\n", removeNewLines(info)))
	}
}

func (a *AutomationRunHandler) HandleToolEnd(_ context.Context, info string) {
	if a.Debug {
		a.Lock()
		defer a.Unlock()
		io.WriteString(a.LogsWriter, fmt.Sprintf("%s\n", removeNewLines(info)))
	}
}

func (a *AutomationRunHandler) HandleToolError(_ context.Context, err error) {
	if a.Debug {
		a.Lock()
		defer a.Unlock()
		io.WriteString(a.LogsWriter, fmt.Sprintf("Exiting tool with error: %s\n", err))
	}
}

func removeNewLines(s any) string {
	return strings.ReplaceAll(fmt.Sprint(s), "\n", " ")
}

func (a *AutomationRunHandler) HandleLLMError(_ context.Context, err error) {
	if a.DebugOpenAICalls {
		a.Lock()
		defer a.Unlock()
		io.WriteString(a.LogsWriter, fmt.Sprintf("Exiting LLM with error: %s\n", err))
	}
}

func (a *AutomationRunHandler) HandleLLMGenerateContentStart(_ context.Context, ms []llms.MessageContent) {
	if a.DebugOpenAICalls {
		a.Lock()
		defer a.Unlock()
		io.WriteString(a.LogsWriter, "Entering LLM with messages:\n")
		for _, m := range ms {
			// TODO: Implement logging of other content types
			var buf strings.Builder
			for _, t := range m.Parts {
				if t, ok := t.(llms.TextContent); ok {
					buf.WriteString(t.Text)
				}
			}
			io.WriteString(a.LogsWriter, fmt.Sprintf("Role: %s\n", m.Role))
			io.WriteString(a.LogsWriter, fmt.Sprintf("Text: %s\n", buf.String()))
		}
	}
}

func (a *AutomationRunHandler) HandleLLMGenerateContentEnd(_ context.Context, res *llms.ContentResponse) {
	if a.DebugOpenAICalls {
		a.Lock()
		defer a.Unlock()
		io.WriteString(a.LogsWriter, "Exiting LLM with response:\n")
		for _, c := range res.Choices {
			if c.Content != "" {
				io.WriteString(a.LogsWriter, fmt.Sprintf("Content: %s\n", c.Content))
			}
			if c.StopReason != "" {
				io.WriteString(a.LogsWriter, fmt.Sprintf("StopReason: %s\n", c.StopReason))
			}
			if len(c.GenerationInfo) > 0 {
				io.WriteString(a.LogsWriter, "GenerationInfo:\n")
				for k, v := range c.GenerationInfo {
					io.WriteString(a.LogsWriter, fmt.Sprintf("%20s: %v\n", k, v))
				}
			}
			//TODO might have to change to tool calls
			if c.FuncCall != nil {
				io.WriteString(a.LogsWriter, fmt.Sprintf("FuncCall: %s %s\n", c.FuncCall.Name,
					c.FuncCall.Arguments))
			}
		}
	}
}

func (a *AutomationRunHandler) HandleStreamingFunc(ctx context.Context, chunk []byte) {
}
