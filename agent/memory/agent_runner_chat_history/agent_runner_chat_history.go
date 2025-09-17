package agent_runner_chat_history

import (
	"context"

	"github.com/ankit-arora/langchaingo/llms"
	"github.com/ankit-arora/langchaingo/schema"
	"github.com/deployment-io/deployment-runner/agent/memory/file_store"
)

type AgentRunnerChatHistory struct {
	JobID   string
	AgentID string
}

func NewAgentRunnerChatHistory(jobID, agentID string) (*AgentRunnerChatHistory, error) {
	return &AgentRunnerChatHistory{
		JobID:   jobID,
		AgentID: agentID,
	}, nil
}

func (a *AgentRunnerChatHistory) AddMessage(ctx context.Context, message llms.ChatMessage) error {
	return nil
}

func (a *AgentRunnerChatHistory) AddUserMessage(ctx context.Context, message string) error {
	//return file_store.AddMessage(a.JobID, a.AgentID, message, message_enums.User)
	return nil
}

func (a *AgentRunnerChatHistory) AddAIMessage(ctx context.Context, message string) error {
	//return file_store.AddMessage(a.JobID, a.AgentID, message, message_enums.Assistant)
	return nil
}

func (a *AgentRunnerChatHistory) Clear(ctx context.Context) error {
	return nil
}

func (a *AgentRunnerChatHistory) Messages(ctx context.Context) ([]llms.ChatMessage, error) {
	return file_store.LoadMessages(a.JobID)
}

func (a *AgentRunnerChatHistory) SetMessages(ctx context.Context, messages []llms.ChatMessage) error {
	return nil
}

// Statically assert that AgentRunnerChatHistory implement the chat message history interface.
var _ schema.ChatMessageHistory = &AgentRunnerChatHistory{}
