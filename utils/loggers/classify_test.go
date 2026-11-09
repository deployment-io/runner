package loggers

import (
	"io"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/logs"
)

func TestClassifyAgentLine(t *testing.T) {
	cases := []struct {
		agent, line, kind, group string
	}{
		{"claude-code", "[tool] Bash: cd /work; git status --short", "tool", ""},
		{"claude-code", "[result] (success, 1094 bytes) M src/api/tasks.ts", "result", ""},
		{"claude-code", "[text] I'll start by reading the timeline builder.", "text", ""},
		{"claude-code", "[thinking] hmm", "text", ""},
		{"claude-code", "[init] model=claude-opus-5-5 tools=27", "text", ""},
		{"claude-code", "[init]", "text", ""},
		{"claude-code", "[done] status=ok turns=34", "done", ""},
		{"claude-code", "[system]", "text", GroupAgentEvents},
		{"claude-code", "[system] thinking_tokens", "text", GroupAgentEvents},
		{"claude-code", "[system] task_started", "text", GroupAgentEvents},
		{"claude-code", "[rate_limit_event]", "text", GroupAgentEvents},
		{"claude-code", "[tool_progress]", "text", GroupAgentEvents},
		{"claude-code", "[system] two words", "text", ""},
		{"claude-code", "[agentbox] agentbox v1.9.41", "text", ""},
		{"claude-code", "yarn install v1.22.22", "text", ""},
		{"claude-code", "$ husky install", "text", ""},
		{"claude-code", "Successfully uploaded file: a.js", "text", GroupFileUploads},
		{"claude-code", `{"status":"Already exists","progressDetail":{},"id":"dd051e1b8c92"}`, "text", GroupDockerProgress},
		{"", "[tool] Read: x", "tool", ""},
		{"", "[rate_limit_event]", "text", GroupAgentEvents},
		{"some-new-agent", "[result] (error, 3 bytes) no", "result", ""},
		{"codex", "▶ codex session started", "text", ""},
		{"codex", `$ /bin/sh -lc "cat package.json; cat vite.config.ts"`, "tool", ""},
		{"codex", "✎ src/a.ts", "tool", ""},
		{"codex", "✗ exit 1 · yarn test", "result", ""},
		{"codex", "✗ turn failed: stream disconnected", "error", ""},
		{"codex", "✓ turn complete · in 120 / out 30 / cached 0", "done", ""},
		{"codex", "🗨 The tests pass now.", "text", ""},
		{"codex", "… reasoning", "text", ""},
		{"codex", "[agentbox] vendor: complete", "text", ""},
		{"codex", "[rate_limit_event]", "text", ""},
		{"codex", "[tool] x", "text", ""},
		{"opencode", "⚙ bash ls", "tool", ""},
		{"opencode", "✗ error: boom", "error", ""},
		{"opencode", "🗨 hello", "text", ""},
		{"opencode", "✓ step complete", "text", ""},
		{"opencode", "$ ls", "text", ""},
		{"opencode", "[system] task_started", "text", ""},
	}
	for _, c := range cases {
		kind, group := classifyAgentLine(c.agent, c.line)
		if kind != c.kind || group != c.group {
			t.Errorf("classifyAgentLine(%q, %q) = (%q, %q), want (%q, %q)", c.agent, c.line, kind, group, c.kind, c.group)
		}
	}
}

func TestNoiseGroup(t *testing.T) {
	cases := map[string]string{
		`{"status":"Pushing","progressDetail":{"current":1},"id":"abc"}`:                               GroupDockerProgress,
		`{"status":"Already exists","progressDetail":{},"id":"dd051e1b8c92"}`:                          GroupDockerProgress,
		`{"status":"Download complete","progressDetail":{},"id":"338554e9113f"}`:                       GroupDockerProgress,
		`{"status":"Pull complete","id":"338554e9113f"}`:                                               GroupDockerProgress,
		`{"status":"Layer already exists","progressDetail":{},"id":"338554e9113f"}`:                    GroupDockerProgress,
		`#7 sha256:2d7b2a3f0c1e 1.05MB / 3.4MB 0.3s`:                                                   GroupDockerProgress,
		`#7 extracting sha256:2d7b2a3f0c1e 0.1s`:                                                       GroupDockerProgress,
		`{"status":"Digest: sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc"}`: "",
		`{"status":"Status: Downloaded newer image"}`:                                                  "",
		`{"stream":" ---> 097edadd4e88\n"}`:                                                            "",
		`{"aux":{"ID":"sha256:abc"}}`:                                                                  "",
		`{"errorDetail":{"message":"failed"},"error":"failed"}`:                                        "",
		`{"status":"Pushing","id":"abc","error":"denied"}`:                                             "",
		`{"status":"","id":"abc"}`:                                                                     "",
		`#5 [2/6] RUN yarn install`:                                                                    "",
		`#5 12.3 yarn install v1.22.22`:                                                                "",
		`Successfully uploaded file: assets/AccountActivation-Bj3oyXT2.js`:                             GroupFileUploads,
		`Successfully uploaded files`:                                                                  "",
		`[rate_limit_event]`:                                                                           "",
	}
	for line, want := range cases {
		if got := noiseGroup(line); got != want {
			t.Errorf("noiseGroup(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestTaskStepFixture feeds lines from a real Task Step job's log, each from
// its own source, through a tagged writer with the stage set for each
// command.
func TestTaskStepFixture(t *testing.T) {
	h := newHarness(true)
	w := h.w
	runner := func(s string) { io.WriteString(w, s+"\n") }
	vendor := NewSource(w)
	claude := AgentOutput(w, "claude-code")
	codex := AgentOutput(w, "codex")
	put := func(dst io.Writer, s string) { io.WriteString(dst, s+"\n") }
	group := func(name string, n int) *logs.JobLogGroupV1 { return &logs.JobLogGroupV1{Name: name, Count: n} }

	w.SetStage(logs.LogStageSetup)
	runner("Checking out repo deployment-io/dashboard into /tmp/<org>/<task>/0-deployment-io/dashboard")
	w.SetStage(logs.LogStageSetup)
	runner("Materialized 9 context file(s) into /work/context")
	w.SetStage(logs.LogStageImplement)
	runner("Disk before agent step: 25.0 GiB free on /")
	runner("Vendoring dependencies into shared cache")
	put(vendor, "[agentbox] agentbox v1.9.41")
	put(vendor, "yarn install v1.22.22")
	put(vendor, "$ husky install")
	put(vendor, "[agentbox] vendor: complete")
	EndSource(vendor)
	runner("Agent: claude-code via Claude Subscription — claude-opus-5-5")
	put(claude, "[init] model=claude-opus-5-5 tools=27 mcp_servers=1 cwd=/work")
	put(claude, "[system] thinking_tokens")
	put(claude, "[system] task_started")
	put(claude, "[rate_limit_event]")
	put(claude, "[tool_progress]")
	put(claude, "[tool] Bash: cd /work/0-deployment-io/dashboard; git status --short; git log --oneline -1")
	put(claude, "[result] (success, 1094 bytes) M src/api/tasks.ts M src/pages/Dashboard/Tasks/TaskPage/ActivityTimeline.tsx")
	put(claude, "[text] I'll start by reading the timeline builder.")
	put(claude, "[done] status=ok turns=34 tokens=66/31.0k duration=7m32s summary=A deployed Task's page now shows all six stages…")
	EndSource(claude)
	w.SetStage(logs.LogStageReview)
	put(codex, "▶ codex session started")
	put(codex, `$ /bin/sh -lc "cat package.json; cat vite.config.ts"`)
	put(codex, "✗ exit 1 · yarn test")
	put(codex, "🗨 The tests pass now.")
	put(codex, "✓ turn complete · in 120 / out 30 / cached 0")
	EndSource(codex)
	runner("Review round 1: effort high")
	runner("Review round 1 result.json:")
	body := WithKind(w, logs.LogKindJSON)
	put(body, "{\n  \"schema_version\": 1,\n  \"findings\": [],\n[… truncated]")
	EndSource(body)
	runner("[MUST FIX] correctness/medium at 0-deployment-io/dashboard/src/pages/Dashboard/Tasks/TaskPage/index.tsx:844 — Polling resets the open Start modal's execution settings.")
	w.SetStage(logs.LogStageQA)
	runner("QA round 1: building the preview of 0-deployment-io/dashboard")
	runner(`{"status":"Already exists","progressDetail":{},"id":"dd051e1b8c92"}`)
	runner(`{"status":"Download complete","progressDetail":{},"id":"338554e9113f"}`)
	runner(`{"status":"Digest: sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc"}`)
	runner(`{"stream":" ---> 097edadd4e88\n"}`)
	runner(`{"errorDetail":{"message":"failed"},"error":"failed"}`)
	runner("Successfully uploaded file: assets/AccountActivation-Bj3oyXT2.js")
	runner("Successfully uploaded file: assets/ActionButton-Bqy40fPV.js")
	runner("verify_preview_reachable: attempt 1 — status=503, not reachable yet, still polling...")
	w.SetStage(logs.LogStagePR)
	runner("Committed a8ec2a0 and pushed tasks/6af0de8f-task-page-show-deploy-and-qa-o for repo deployment-io/dashboard")
	w.SetStage(logs.LogStagePR)
	runner("Opened PR #259 for repo deployment-io/dashboard: https://github.com/deployment-io/dashboard/pull/259")
	io.WriteString(WithKind(w, logs.LogKindDone), "Successfully executed - 6af07ee65450b39314197b0a\n")
	w.Close()

	assertLines(t, h.snapshot(), []JobLog{
		{Message: "Checking out repo deployment-io/dashboard into /tmp/<org>/<task>/0-deployment-io/dashboard", Stage: "setup", Kind: "text"},
		{Message: "Materialized 9 context file(s) into /work/context", Stage: "setup", Kind: "text"},
		{Message: "--- Implement stage ---", Stage: "implement", Kind: "section"},
		{Message: "Disk before agent step: 25.0 GiB free on /", Stage: "implement", Kind: "text"},
		{Message: "Vendoring dependencies into shared cache", Stage: "implement", Kind: "text"},
		{Message: "[agentbox] agentbox v1.9.41", Stage: "implement", Kind: "text"},
		{Message: "yarn install v1.22.22", Stage: "implement", Kind: "text"},
		{Message: "$ husky install", Stage: "implement", Kind: "text"},
		{Message: "[agentbox] vendor: complete", Stage: "implement", Kind: "text"},
		{Message: "Agent: claude-code via Claude Subscription — claude-opus-5-5", Stage: "implement", Kind: "text"},
		{Message: "[init] model=claude-opus-5-5 tools=27 mcp_servers=1 cwd=/work", Stage: "implement", Kind: "text"},
		{Message: "[tool_progress]", Stage: "implement", Kind: "text", Group: group(GroupAgentEvents, 4)},
		{Message: "[tool] Bash: cd /work/0-deployment-io/dashboard; git status --short; git log --oneline -1", Stage: "implement", Kind: "tool"},
		{Message: "[result] (success, 1094 bytes) M src/api/tasks.ts M src/pages/Dashboard/Tasks/TaskPage/ActivityTimeline.tsx", Stage: "implement", Kind: "result"},
		{Message: "[text] I'll start by reading the timeline builder.", Stage: "implement", Kind: "text"},
		{Message: "[done] status=ok turns=34 tokens=66/31.0k duration=7m32s summary=A deployed Task's page now shows all six stages…", Stage: "implement", Kind: "done"},
		{Message: "--- Review stage ---", Stage: "review", Kind: "section"},
		{Message: "▶ codex session started", Stage: "review", Kind: "text"},
		{Message: `$ /bin/sh -lc "cat package.json; cat vite.config.ts"`, Stage: "review", Kind: "tool"},
		{Message: "✗ exit 1 · yarn test", Stage: "review", Kind: "result"},
		{Message: "🗨 The tests pass now.", Stage: "review", Kind: "text"},
		{Message: "✓ turn complete · in 120 / out 30 / cached 0", Stage: "review", Kind: "done"},
		{Message: "Review round 1: effort high", Stage: "review", Kind: "text"},
		{Message: "Review round 1 result.json:", Stage: "review", Kind: "text"},
		{Message: "{", Stage: "review", Kind: "json"},
		{Message: `"schema_version": 1,`, Stage: "review", Kind: "json"},
		{Message: `"findings": [],`, Stage: "review", Kind: "json"},
		{Message: "[… truncated]", Stage: "review", Kind: "json"},
		{Message: "[MUST FIX] correctness/medium at 0-deployment-io/dashboard/src/pages/Dashboard/Tasks/TaskPage/index.tsx:844 — Polling resets the open Start modal's execution settings.", Stage: "review", Kind: "text"},
		{Message: "--- QA stage ---", Stage: "qa", Kind: "section"},
		{Message: "QA round 1: building the preview of 0-deployment-io/dashboard", Stage: "qa", Kind: "text"},
		{Message: `{"status":"Download complete","progressDetail":{},"id":"338554e9113f"}`, Stage: "qa", Kind: "text", Group: group(GroupDockerProgress, 2)},
		{Message: `{"status":"Digest: sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc"}`, Stage: "qa", Kind: "text"},
		{Message: `{"stream":" ---> 097edadd4e88\n"}`, Stage: "qa", Kind: "text"},
		{Message: `{"errorDetail":{"message":"failed"},"error":"failed"}`, Stage: "qa", Kind: "text"},
		{Message: "Successfully uploaded file: assets/ActionButton-Bqy40fPV.js", Stage: "qa", Kind: "text", Group: group(GroupFileUploads, 2)},
		{Message: "verify_preview_reachable: attempt 1 — status=503, not reachable yet, still polling...", Stage: "qa", Kind: "text"},
		{Message: "--- Pull request ---", Stage: "pr", Kind: "section"},
		{Message: "Committed a8ec2a0 and pushed tasks/6af0de8f-task-page-show-deploy-and-qa-o for repo deployment-io/dashboard", Stage: "pr", Kind: "text"},
		{Message: "Opened PR #259 for repo deployment-io/dashboard: https://github.com/deployment-io/dashboard/pull/259", Stage: "pr", Kind: "text"},
		{Message: "Successfully executed - 6af07ee65450b39314197b0a", Stage: "pr", Kind: "done"},
	})
}
