package file_store

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ankit-arora/langchaingo/llms"
	"github.com/deployment-io/deployment-runner-kit/agents"
	"github.com/deployment-io/deployment-runner-kit/enums/message_enums"
)

type MessageJsonFormat struct {
	JobID   string             `json:"job_id"`
	AgentID string             `json:"agent_id"`
	Content string             `json:"content"`
	Type    message_enums.Type `json:"type"`
}

func getFilePath(jobID string) string {
	// Ensure the directory structure exists before returning the file path
	dirPath := "/tmp/agent/memory/file_store/"
	if err := os.MkdirAll(dirPath, 0755); err != nil && !os.IsExist(err) {
		fmt.Printf("error creating directory structure: %s\n", err)
		return ""
	}
	return "/tmp/agent/memory/file_store/" + jobID
}

func AddChatHistory(jobID, agentID string, chatHistory []agents.MessageDataDtoV1) error {
	if len(chatHistory) == 0 {
		return nil
	}
	filePath := getFilePath(jobID)

	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("error opening file for adding chat history: %s", err)
	}
	defer file.Close()

	for _, message := range chatHistory {
		contentBase64 := base64.StdEncoding.EncodeToString([]byte(message.Content))
		var messageBytes []byte
		messageBytes, err = json.Marshal(MessageJsonFormat{
			JobID:   jobID,
			AgentID: agentID,
			Content: contentBase64,
			Type:    message.Type,
		})
		if err != nil {
			return fmt.Errorf("error marshalling json: %s", err)
		}
		if _, err = file.WriteString(string(messageBytes) + "\n"); err != nil {
			return fmt.Errorf("error writing message to file: %s", err)
		}
	}
	return nil
}

func AddMessage(jobID, agentID, content string, messageType message_enums.Type) error {
	contentBase64 := base64.StdEncoding.EncodeToString([]byte(content))
	messageBytes, err := json.Marshal(MessageJsonFormat{
		JobID:   jobID,
		AgentID: agentID,
		Content: contentBase64,
		Type:    messageType,
	})
	if err != nil {
		return fmt.Errorf("error marshalling json: %s", err)
	}
	filePath := getFilePath(jobID)

	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("error opening file for adding a message: %s", err)
	}
	defer file.Close()

	if _, err = file.WriteString(string(messageBytes) + "\n"); err != nil {
		return fmt.Errorf("error writing message to file: %s", err)
	}
	return nil
}

func LoadMessages(jobID string) ([]llms.ChatMessage, error) {
	filePath := getFilePath(jobID)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil, nil
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("error opening file for loading messages: %s", err)
	}
	defer file.Close()

	var messages []llms.ChatMessage
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		var messageJson MessageJsonFormat
		if err = json.Unmarshal([]byte(line), &messageJson); err != nil {
			return nil, fmt.Errorf("error unmarshalling line: %s", err)
		}
		var decodedContent []byte
		decodedContent, err = base64.StdEncoding.DecodeString(messageJson.Content)
		if err != nil {
			return nil, fmt.Errorf("error decoding base64 content: %s", err)
		}
		// Convert MessageJsonFormat to llms.ChatMessage
		var chatMessage llms.ChatMessage
		switch messageJson.Type {
		case message_enums.Assistant:
			chatMessage = llms.AIChatMessage{Content: string(decodedContent)}
		case message_enums.User:
			chatMessage = llms.HumanChatMessage{Content: string(decodedContent)}
		default:
			// Handle other message types or log an error
			continue
		}
		messages = append(messages, chatMessage)
	}

	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("error scanning file: %s", err)
	}

	return messages, nil
}

func DeleteFile(jobID string) error {
	filePath := getFilePath(jobID)
	return os.Remove(filePath)
}
