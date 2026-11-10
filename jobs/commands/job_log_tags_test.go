package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/logs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
)

func TestLogRoundOutputKinds(t *testing.T) {
	dir := t.TempDir()
	body := []byte("{\n  \"schema_version\": 1,\n  \"findings\": []\n}")
	body = append(body, bytes.Repeat([]byte("x"), reviewRoundResultLogMaxBytes)...)
	if err := os.WriteFile(filepath.Join(dir, agentboxResultFile), body, 0o644); err != nil {
		t.Fatal(err)
	}
	var lines []loggers.JobLog
	w := loggers.NewJobLogsWriter(true, func(l loggers.JobLog) { lines = append(lines, l) })
	w.SetStage(logs.LogStageReview)
	logRoundOutput(dir, "Review round 1", w)
	w.Close()
	lines = lines[1:] // after the section line
	if len(lines) != 6 {
		t.Fatalf("got %d lines: %+v", len(lines), lines)
	}
	if lines[0].Message != "Review round 1 result.json:" || lines[0].Kind != logs.LogKindText {
		t.Fatalf("label line = %+v", lines[0])
	}
	for _, l := range lines[1:] {
		if l.Kind != logs.LogKindJSON || l.Stage != logs.LogStageReview {
			t.Fatalf("body line = %+v", l)
		}
	}
	if lines[5].Message != "[… truncated]" {
		t.Fatalf("last line = %q", lines[5].Message)
	}
}

func TestLogRoundOutputPlainWriter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, agentboxResultFile), []byte("{\n  \"a\": 1\n}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logRoundOutput(dir, "QA round 1", &buf)
	if want := "QA round 1 result.json:\n{\n  \"a\": 1\n}\n"; buf.String() != want {
		t.Fatalf("written = %q, want %q", buf.String(), want)
	}
}

func TestSpawnAgentType(t *testing.T) {
	cases := []struct {
		env  []string
		want string
	}{
		{nil, "claude-code"},
		{[]string{"AGENT_TYPE="}, "claude-code"},
		{[]string{"MODEL=x", "AGENT_TYPE=codex"}, "codex"},
		{[]string{"AGENT_TYPE=claude-code", "AGENT_TYPE=opencode"}, "opencode"},
	}
	for _, c := range cases {
		if got := spawnAgentType(c.env); got != c.want {
			t.Errorf("spawnAgentType(%q) = %q, want %q", c.env, got, c.want)
		}
	}
}
