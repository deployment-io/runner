package automation_agent_chat_history

import (
	"context"
	"github.com/ankit-arora/langchaingo/llms"
	"github.com/ankit-arora/langchaingo/schema"
	"github.com/deployment-io/deployment-runner/automation/memory/file_store"
)

type AutomationAgentChatHistory struct {
	JobID        string
	AutomationID string
}

func NewAutomationAgentChatMessageHistory(jobID, automationID string) (*AutomationAgentChatHistory, error) {
	return &AutomationAgentChatHistory{
		JobID:        jobID,
		AutomationID: automationID,
	}, nil
}

func (a *AutomationAgentChatHistory) AddMessage(ctx context.Context, message llms.ChatMessage) error {
	return nil
}

func (a *AutomationAgentChatHistory) AddUserMessage(ctx context.Context, message string) error {
	//return file_store.AddMessage(a.JobID, a.AutomationID, message, message_enums.User)
	return nil
}

func (a *AutomationAgentChatHistory) AddAIMessage(ctx context.Context, message string) error {
	//return file_store.AddMessage(a.JobID, a.AutomationID, message, message_enums.Assistant)
	return nil
}

func (a *AutomationAgentChatHistory) Clear(ctx context.Context) error {
	return nil
}

func (a *AutomationAgentChatHistory) Messages(ctx context.Context) ([]llms.ChatMessage, error) {
	return file_store.LoadMessages(a.JobID)
}

func (a *AutomationAgentChatHistory) SetMessages(ctx context.Context, messages []llms.ChatMessage) error {
	return nil
}

// Statically assert that AutomationAgentChatHistory implement the chat message history interface.
var _ schema.ChatMessageHistory = &AutomationAgentChatHistory{}
