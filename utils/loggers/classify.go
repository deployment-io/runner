package loggers

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/logs"
)

// Names of the groups of noise a Task Step job's log folds into one line.
const (
	GroupDockerProgress = "docker progress"
	GroupFileUploads    = "file uploads"
	GroupAgentEvents    = "agent events"
)

const (
	agentTypeClaudeCode = "claude-code"
	agentTypeCodex      = "codex"
	agentTypeOpencode   = "opencode"
)

var (
	buildKitLayerProgress = regexp.MustCompile(`^#\d+ (extracting )?sha256:[0-9a-f]+`)
	claudeSystemEvent     = regexp.MustCompile(`^\[system\]( [a-z_]+)?$`)
	claudeLoneEvent       = regexp.MustCompile(`^\[([a-z_]+)\]$`)
)

// claudeFormatterTypes are the bracketed types agentbox's claude formatter
// prints with content of their own. A bare "[text]" is one of those with
// empty content, not the formatter's default "[<type>]" case.
var claudeFormatterTypes = map[string]bool{
	"tool": true, "result": true, "text": true, "thinking": true, "init": true, "done": true, "system": true,
}

type prefixKind struct {
	prefix string
	kind   string
}

var claudeCodePrefixes = []prefixKind{
	{"[tool] ", logs.LogKindTool},
	{"[result] ", logs.LogKindResult},
	{"[text] ", logs.LogKindText},
	{"[thinking] ", logs.LogKindText},
	{"[init] ", logs.LogKindText},
	{"[done] ", logs.LogKindDone},
}

var codexPrefixes = []prefixKind{
	{"$ ", logs.LogKindTool},
	{"✎ ", logs.LogKindTool},
	// A command the agent ran exited non-zero: a tool result, not a run error.
	{"✗ exit ", logs.LogKindResult},
	{"✗ ", logs.LogKindError},
	{"✓ ", logs.LogKindDone},
	{"🗨 ", logs.LogKindText},
	{"… ", logs.LogKindText},
	{"▶ ", logs.LogKindText},
}

var opencodePrefixes = []prefixKind{
	{"⚙ ", logs.LogKindTool},
	{"✗ ", logs.LogKindError},
	{"🗨 ", logs.LogKindText},
	// "✓ step complete" follows every model step; it is not the end of the run.
	{"✓ ", logs.LogKindText},
}

// classifyAgentLine returns the kind of one line of an agent container's
// output, from agentbox's formatter prefixes, and the group it folds into
// ("" for none). An empty or unknown agent type uses the claude-code rules.
func classifyAgentLine(agentType, line string) (kind string, group string) {
	if strings.HasPrefix(line, "[agentbox] ") {
		return logs.LogKindText, ""
	}
	var prefixes []prefixKind
	switch agentType {
	case agentTypeCodex:
		prefixes = codexPrefixes
	case agentTypeOpencode:
		prefixes = opencodePrefixes
	default:
		if line == "[init]" {
			return logs.LogKindText, ""
		}
		if isClaudeAgentEvent(line) {
			return logs.LogKindText, GroupAgentEvents
		}
		prefixes = claudeCodePrefixes
	}
	for _, p := range prefixes {
		if strings.HasPrefix(line, p.prefix) {
			return p.kind, ""
		}
	}
	return logs.LogKindText, noiseGroup(line)
}

// isClaudeAgentEvent reports whether line is one of the claude formatter's
// event lines: "[system]", "[system] <word>" or the default case "[<type>]".
func isClaudeAgentEvent(line string) bool {
	if claudeSystemEvent.MatchString(line) {
		return true
	}
	if m := claudeLoneEvent.FindStringSubmatch(line); m != nil {
		return !claudeFormatterTypes[m[1]]
	}
	return false
}

// noiseGroup returns the group a line from any source folds into: Docker
// progress or a file upload. "" means the line is not noise.
func noiseGroup(line string) string {
	if strings.HasPrefix(line, "Successfully uploaded file: ") {
		return GroupFileUploads
	}
	if isDockerProgress(line) {
		return GroupDockerProgress
	}
	return ""
}

// isDockerProgress reports whether line is a Docker JSON progress message
// (a status with an id or progressDetail, and no error) or a BuildKit layer
// progress line.
func isDockerProgress(line string) bool {
	if strings.HasPrefix(line, "#") {
		return buildKitLayerProgress.MatchString(line)
	}
	if !strings.HasPrefix(line, "{") {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return false
	}
	var status string
	if raw, ok := m["status"]; !ok || json.Unmarshal(raw, &status) != nil || status == "" {
		return false
	}
	if _, ok := m["error"]; ok {
		return false
	}
	if _, ok := m["errorDetail"]; ok {
		return false
	}
	_, hasID := m["id"]
	_, hasProgress := m["progressDetail"]
	return hasID || hasProgress
}
