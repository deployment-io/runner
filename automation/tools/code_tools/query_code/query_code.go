package query_code

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ankit-arora/langchaingo/callbacks"
	"github.com/ankit-arora/langchaingo/llms"
	"github.com/ankit-arora/langchaingo/llms/openai"
	"github.com/ankit-arora/langchaingo/tools"
	"github.com/deployment-io/deployment-runner-kit/automations"
	"github.com/deployment-io/deployment-runner-kit/enums/automation_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/automation/tools/code_tools/query_code/golang"
	"github.com/deployment-io/deployment-runner/automation/tools/code_tools/query_code/types"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/team-ai/enums/rpcs"
	"github.com/deployment-io/team-ai/rpc"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-playground/validator/v10"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Tool struct {
	Params           map[string]interface{}
	LogsWriter       io.Writer
	CallbacksHandler callbacks.Handler
	Entities         []automation_enums.Entity
	DebugOpenAICalls bool
}

func (t *Tool) entitiesString() string {
	entities := ""
	for index, entity := range t.Entities {
		entities += entity.String()
		if index < len(t.Entities)-1 {
			entities += ", "
		}
	}
	return entities
}

type Input struct {
	Query string `json:"query" validate:"required"`
}

func parseAndValidateInput(response string, input *Input) error {
	// Parse the JSON response into a map
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(response), &raw); err != nil {
		return fmt.Errorf("failed to parse JSON: %w", err)
	}
	query, ok := raw["query"]
	if !ok {
		return fmt.Errorf("query key not found")
	}
	q, ok := query.(string)
	if !ok {
		return fmt.Errorf("query is not a string")
	}
	// Map the parsed response into the struct
	input.Query = q
	// Validate the struct
	validate := validator.New()
	if err := validate.Struct(input); err != nil {
		return fmt.Errorf("validation error: %w", err)
	}
	return nil
}

func (t *Tool) Name() string {
	return "queryCode"
}

func (t *Tool) Description() string {
	entitiesString := t.entitiesString()
	description := `Queries the source code for %s, and returns the answer to the input query. The tool has access to the source code and checks out the source code from GitHub, GitLab, or Bitbucket.
	The tool requires the following inputs: query (the input query).`
	description = fmt.Sprintf(description, entitiesString)
	return description
}

func addEdges(dir, moduleName string, graph *types.CodeGraph, queryContent string) error {

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".go" {
			//fmt.Printf("Analyzing File for Edges: %s\n", filePath)
			err = golang.AddEdges(dir, path, queryContent, graph, moduleName)
			if err != nil {
				return err
			}

		}
		return nil
	})
	return err
}

func addNodes(dir, moduleName string, graph *types.CodeGraph, queryContent string) error {
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".go" {
			//fmt.Printf("Analyzing File for Nodes: %s\n", path)
			err = golang.AddNodes(dir, path, queryContent, graph, moduleName)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

const queriesFilesDir = "./automation/tools/code_tools/queries"

func extractFunctionContent(node *types.CodeNode) (string, error) {
	content, err := os.ReadFile(node.Path)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %v", err)
	}

	lines := strings.Split(string(content), "\n")
	start := strings.Split(node.StartPosition, ":")
	end := strings.Split(node.EndPosition, ":")

	if len(start) != 2 || len(end) != 2 {
		return "", fmt.Errorf("invalid position format")
	}

	startLine := 0
	endLine := 0
	_, err = fmt.Sscanf(start[0], "%d", &startLine)
	if err != nil {
		return "", fmt.Errorf("invalid start line number for the function")
	}
	_, err = fmt.Sscanf(end[0], "%d", &endLine)
	if err != nil {
		return "", fmt.Errorf("invalid end line number for the function")
	}

	if startLine == 0 || endLine == 0 || startLine > len(lines) || endLine > len(lines) {
		return "", fmt.Errorf("invalid line numbers")
	}

	functionLines := lines[startLine-1 : endLine]
	return strings.Join(functionLines, "\n"), nil
}

var httpClient = rpc.NewHTTPClient(rpcs.AzureOpenAI, true, true, 2)

func (t *Tool) getAnswerFromFunctionUsingLLM(query string, nodeIDs []int64, graph *types.CodeGraph) (string, error) {
	automationData, err := jobs.GetParameterValue[automations.AutomationDataDtoV1](t.Params, parameters_enums.AutomationData)
	if err != nil {
		return "", fmt.Errorf("failed to get automation data: %w", err)
	}

	llm, err := openai.New(openai.WithModel(automationData.LlmCodeQueryModelType.String()), openai.WithAPIType(openai.APITypeAzure),
		openai.WithAPIVersion("2024-02-01"), openai.WithHTTPClient(httpClient),
		openai.WithCallback(t.CallbacksHandler), openai.WithBaseURL(automationData.OpenAIBaseUrl),
		openai.WithToken(automationData.OpenAIAPIKey))
	if err != nil {
		return "", err
	}
	ctx := context.Background()

	// Find the node with the first ID and extract its function content
	if len(nodeIDs) > 0 {
		var functionContents string
		for _, nodeID := range nodeIDs {
			node, exists := graph.IDsToNodesMap[nodeID]
			if exists {
				functionContent, err := extractFunctionContent(node)
				if err != nil {
					return "", fmt.Errorf("error extracting function content: %w", err)
				}
				functionContents += fmt.Sprintf("%s\n", functionContent)
			}
		}
		userMessage := fmt.Sprintf("Based on these functions:\n\n%s\n\nAnswer this question: %s", functionContents, query)
		content := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, "You are an expert in understanding code."),
			llms.TextParts(llms.ChatMessageTypeHuman, userMessage),
		}

		chatCompletion, err := llm.GenerateContent(ctx, content, llms.WithModel(automationData.LlmCodeQueryModelType.String()))
		if err != nil {
			return "", fmt.Errorf("failed to get function summary from LLM: %w", err)
		}

		if len(chatCompletion.Choices) == 0 {
			return "", fmt.Errorf("no response from LLM")
		}

		return chatCompletion.Choices[0].Content, nil
	}
	return "I'm not able to answer the query based on the code", nil
}

func (t *Tool) findRelevantFunctions(question string, graph *types.CodeGraph) ([]int64, error) {
	automationData, err := jobs.GetParameterValue[automations.AutomationDataDtoV1](t.Params, parameters_enums.AutomationData)
	if err != nil {
		return nil, fmt.Errorf("failed to get automation data: %w", err)
	}

	llm, err := openai.New(openai.WithModel(automationData.LlmCodeQueryModelType.String()), openai.WithAPIType(openai.APITypeAzure),
		openai.WithAPIVersion("2024-02-01"), openai.WithHTTPClient(httpClient),
		openai.WithCallback(t.CallbacksHandler), openai.WithBaseURL(automationData.OpenAIBaseUrl),
		openai.WithToken(automationData.OpenAIAPIKey))
	if err != nil {
		return nil, err
	}
	ctx := context.Background()

	// Create a list of function descriptions
	type FunctionInfo struct {
		ID          int64
		Description string
	}
	var functionInfos []FunctionInfo

	for _, node := range graph.NameToNodesMap {
		if node.Type == "function" || node.Type == "method" {
			functionInfos = append(functionInfos, FunctionInfo{
				ID:          node.ID(),
				Description: node.Description,
			})
		}
	}

	userMessage := fmt.Sprintf("Given this question: '%s'\nAnalyze these function descriptions and return the IDs of functions that might contain the answer, ordered by relevance. Only return the IDs separated by commas, nothing else:\n\n", question)
	for _, info := range functionInfos {
		userMessage += fmt.Sprintf("ID %d: %s\n", info.ID, info.Description)
	}

	content := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, "You are an expert in understanding code."),
		llms.TextParts(llms.ChatMessageTypeHuman, userMessage),
	}

	chatCompletion, err := llm.GenerateContent(ctx, content, llms.WithModel(automationData.LlmCodeQueryModelType.String()))
	if err != nil {
		return nil, fmt.Errorf("failed to get function summary from LLM: %w", err)
	}

	if len(chatCompletion.Choices) == 0 {
		return nil, fmt.Errorf("no response from LLM")
	}

	// Parse the response into a slice of node IDs
	response := chatCompletion.Choices[0].Content
	idStrs := strings.Split(strings.ReplaceAll(response, " ", ""), ",")
	var nodeIDs []int64

	for _, idStr := range idStrs {
		var id int64
		_, err := fmt.Sscanf(idStr, "%d", &id)
		if err != nil {
			continue
		}
		nodeIDs = append(nodeIDs, id)
	}

	return nodeIDs, nil

}

func (t *Tool) getFunctionSummaryFromLLM(graph *types.CodeGraph) error {
	automationData, err := jobs.GetParameterValue[automations.AutomationDataDtoV1](t.Params, parameters_enums.AutomationData)
	if err != nil {
		return fmt.Errorf("failed to get automation data: %w", err)
	}

	llm, err := openai.New(openai.WithModel(automationData.LlmCodeQueryModelType.String()), openai.WithAPIType(openai.APITypeAzure),
		openai.WithAPIVersion("2024-02-01"), openai.WithHTTPClient(httpClient),
		openai.WithCallback(t.CallbacksHandler), openai.WithBaseURL(automationData.OpenAIBaseUrl),
		openai.WithToken(automationData.OpenAIAPIKey))
	if err != nil {
		return err
	}
	ctx := context.Background()

	for _, node := range graph.NameToNodesMap {
		if node.Type == "function" || node.Type == "method" {
			functionContent, err := extractFunctionContent(node)
			if err != nil {
				return fmt.Errorf("failed to extract function content: %w", err)
			}

			userMessage := fmt.Sprintf("Analyze this function and explain what it does in one sentence:\n\n%s", functionContent)

			content := []llms.MessageContent{
				llms.TextParts(llms.ChatMessageTypeSystem, "You are an expert in understanding code."),
				llms.TextParts(llms.ChatMessageTypeHuman, userMessage),
			}

			chatCompletion, err := llm.GenerateContent(ctx, content, llms.WithModel(automationData.LlmCodeQueryModelType.String()))

			if err != nil {
				return fmt.Errorf("failed to get function summary from LLM: %w", err)
			}

			if len(chatCompletion.Choices) > 0 {
				node.Description = chatCompletion.Choices[0].Content
			} else {
				return fmt.Errorf("no response from LLM")
			}
		}
	}
	return nil
}

// queryCodebase analyzes all Go files in the specified directory using Tree-sitter with the provided query
func queryCodebase(dir string, graph *types.CodeGraph) error {
	moduleName, err := golang.GetModuleName(dir)
	if err != nil {
		return fmt.Errorf("failed to get module name: %v", err)
	}
	// Load query from a file
	queryFilePath := filepath.Join(queriesFilesDir, "tree-sitter-go-tags.scm")
	queryContent, err := os.ReadFile(queryFilePath)
	if err != nil {
		return fmt.Errorf("failed to read query file: %v", err)
	}
	err = addNodes(dir, moduleName, graph, string(queryContent))
	if err != nil {
		return fmt.Errorf("failed to add nodes: %v", err)
	}
	err = addEdges(dir, moduleName, graph, string(queryContent))
	if err != nil {
		return fmt.Errorf("failed to add edges: %v", err)
	}
	return nil
}

func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	if len(input) == 0 {
		return "Input cannot be empty. Please provide a valid query.", nil
	}
	if t.CallbacksHandler != nil {
		info := fmt.Sprintf("Querying code with input: %s", input)
		if !t.DebugOpenAICalls {
			if len(info) > 200 {
				info = info[:200] + "..."
			}
		}
		t.CallbacksHandler.HandleToolStart(ctx, info)
	}
	inputJsonObj := &Input{}
	jsonParseErr := parseAndValidateInput(input, inputJsonObj)
	if jsonParseErr != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error parsing input: %s", jsonParseErr))
		}
		return fmt.Sprintf("Code was not analyzed due to the following reason: %s", jsonParseErr), nil
	}

	//1. check out the code from the repository
	repoCloneUrl, err := jobs.GetParameterValue[string](t.Params, parameters_enums.RepoCloneUrl)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting repository clone url: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	repoBranch, err := jobs.GetParameterValue[string](t.Params, parameters_enums.RepoBranch)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting repository branch: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	repoProviderToken, err := jobs.GetParameterValue[string](t.Params, parameters_enums.RepoProviderToken)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting repository provider token: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	repoGitProvider, err := jobs.GetParameterValue[string](t.Params, parameters_enums.RepoGitProvider)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting repository git provider: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	repoCloneUrlWithToken, err := commandUtils.GetRepoUrlWithToken(repoGitProvider, repoProviderToken, repoCloneUrl)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting repository clone url with token: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	io.WriteString(t.LogsWriter, fmt.Sprintf("Checking out branch %s for repository: %s\n", repoBranch, repoCloneUrl))
	var repoDirectoryPath string
	repoDirectoryPath, err = commandUtils.GetRepositoryDirectoryPath(t.Params)

	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting repository directory path: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	var repository *git.Repository
	repository, err = commandUtils.CloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken,
		repoGitProvider, t.LogsWriter)
	if err != nil {
		if commandUtils.IsErrorAuthenticationRequired(err) {
			repoProviderToken, err = commandUtils.RefreshGitToken(t.Params)
			if err != nil {
				if t.CallbacksHandler != nil {
					t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error refreshing git token: %s", err))
				}
				return "There was an error. We'll get back to you.", nil
			}
			repoCloneUrlWithToken, err = commandUtils.GetRepoUrlWithToken(repoGitProvider, repoProviderToken, repoCloneUrl)
			if err != nil {
				if t.CallbacksHandler != nil {
					t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting repository clone url with token: %s", err))
				}
				return "There was an error. We'll get back to you.", nil
			}
			repository, err = commandUtils.CloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, repoGitProvider, t.LogsWriter)
			if err != nil {
				if t.CallbacksHandler != nil {
					t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error cloning repository: %s", err))
				}
				return "There was an error. We'll get back to you.", nil
			}
			jobs.SetParameterValue(t.Params, parameters_enums.RepoProviderToken, repoProviderToken)
		} else {
			if t.CallbacksHandler != nil {
				t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error cloning repository: %s", err))
			}
			return "There was an error. We'll get back to you.", nil
		}
	}
	var worktree *git.Worktree
	worktree, err = repository.Worktree()
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting worktree: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	err = commandUtils.FetchRepository(repository, repoProviderToken, repoGitProvider, t.LogsWriter)
	if err != nil {
		if commandUtils.IsErrorAuthenticationRequired(err) {
			repoProviderToken, err = commandUtils.RefreshGitToken(t.Params)
			if err != nil {
				if t.CallbacksHandler != nil {
					t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error refreshing git token: %s", err))
				}
				return "There was an error. We'll get back to you.", nil
			}
			err = commandUtils.FetchRepository(repository, repoProviderToken, repoGitProvider, t.LogsWriter)
			if err != nil {
				if t.CallbacksHandler != nil {
					t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error fetching repository: %s", err))
				}
				return "There was an error. We'll get back to you.", nil
			}
			jobs.SetParameterValue(t.Params, parameters_enums.RepoProviderToken, repoProviderToken)
		} else {
			if t.CallbacksHandler != nil {
				t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error fetching repository: %s", err))
			}
			return "There was an error. We'll get back to you.", nil
		}
	}

	referenceName := plumbing.NewRemoteReferenceName("origin", repoBranch)
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: referenceName,
	})
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error checking out branch: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	username := commandUtils.GetUsernameForProvider(repoGitProvider)

	// Ensure submodules are updated after the checkout
	err = commandUtils.UpdateSubmodules(repository, username, repoProviderToken, t.LogsWriter)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error updating submodules: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	//root directory added to repo directory
	//TODO assume the repo root is root directory for now
	//repoDirectoryPath = addRootDirectory(parameters, repoDirectoryPath)

	//go through each file to analyze
	knowledgeGraph := types.NewCodeGraph()
	// Analyze the directory using the Tree-sitter query
	err = queryCodebase(repoDirectoryPath, knowledgeGraph)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error analyzing codebase: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	err = t.getFunctionSummaryFromLLM(knowledgeGraph)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error analyzing functions with OpenAI: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	nodeIDs, err := t.findRelevantFunctions(inputJsonObj.Query, knowledgeGraph)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error finding relevant functions: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	out, err := t.getAnswerFromFunctionUsingLLM(inputJsonObj.Query, nodeIDs, knowledgeGraph)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting answer from function using LLM: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	return out, nil
}

func (t *Tool) Parameters() map[string]any {
	parameters := map[string]any{
		"properties": map[string]any{
			"query": map[string]string{"title": "query", "type": "string", "description": "The input query related to the code."},
		},
		"required": []string{"query"},
		"type":     "object",
	}
	return parameters
}

var _ tools.ToolWithParameters = &Tool{}
