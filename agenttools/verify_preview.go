package agenttools

// verify_preview.go implements the agent-invoked `verify_preview` MCP tool: after
// deploying a preview, the agent calls this to confirm the URL is actually live and
// serving. It runs on the RUNNER, not the agent — the agentbox proxy allowlist
// blocks arbitrary outbound hosts, so the agent can't reach the CloudFront URL
// itself; the runner (open network) polls it and returns the result.
//
// SECURITY: the runner fetches an agent-supplied URL, which is an SSRF vector — a
// prompt-injected agent could ask it to fetch cloud-metadata (169.254.169.254) or
// an internal host and exfiltrate the response through the tool result. So
// verify_preview ONLY fetches hosts under *.cloudfront.net (every preview is a
// CloudFront distribution). That allowlist is the whole SSRF defense; widen it
// deliberately when custom preview domains land (Cw).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

const (
	verifyPreviewDefaultMaxWait = 240 * time.Second
	verifyPreviewMaxWaitCap     = 300 * time.Second
	verifyPreviewPollInterval   = 5 * time.Second
	verifyPreviewPerRequestTO   = 10 * time.Second
	verifyPreviewSnippetRunes   = 500
	verifyPreviewReadLimitBytes = 64 * 1024
)

const verifyPreviewInputSchema = `{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The preview URL returned by deploy_preview (an https://*.cloudfront.net URL). Only CloudFront preview hosts are accepted."
    },
    "contains": {
      "type": "string",
      "description": "Optional ADVISORY substring check against the response body. It does NOT affect liveness — a 200 is always live — and is reported separately as matched. An SPA's body is the HTML shell, so client-rendered text will NOT be found here; for an SPA omit this and confirm the change in the built bundle instead."
    },
    "max_wait_seconds": {
      "type": "integer",
      "description": "How long to poll for the URL to first return 200 before giving up (default 240, max 300). The call returns as soon as it's live, so a larger value only pushes out the give-up point. A first-time distribution can take a few minutes to propagate."
    }
  },
  "required": ["url"]
}`

// verifyPreviewResult is the JSON the agent receives.
type verifyPreviewResult struct {
	Live           bool   `json:"live"`
	StatusCode     int    `json:"status_code"`
	Attempts       int    `json:"attempts"`
	ElapsedSeconds int    `json:"elapsed_seconds"`
	Matched        *bool  `json:"matched,omitempty"` // set only when `contains` was requested
	BodySnippet    string `json:"body_snippet,omitempty"`
	Message        string `json:"message,omitempty"`
}

// RegisterVerifyPreview registers the verify_preview tool. logsWriter is the Step
// Job's log writer; poll progress streams there.
func RegisterVerifyPreview(s *agentmcp.Server, logsWriter io.Writer) {
	s.Register(agentmcp.Tool{
		Name: "verify_preview",
		Description: "Confirm a deployed preview URL is live. Polls it until it returns HTTP 200 (a first-time " +
			"distribution can take a few minutes to propagate), and returns live=true as soon as it does — that 200 is " +
			"your success signal. Runs on the runner, so it works even though your sandbox can't reach the URL directly. " +
			"Only *.cloudfront.net preview URLs are accepted. Do NOT pass `contains` expecting rendered SPA text — an " +
			"SPA's body is the HTML shell, so verify the change in the built bundle instead.",
		InputSchema: json.RawMessage(verifyPreviewInputSchema),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handleVerifyPreview(ctx, logsWriter, args)
		},
	})
}

func handleVerifyPreview(ctx context.Context, logsWriter io.Writer, rawArgs json.RawMessage) (string, error) {
	var args struct {
		URL            string `json:"url"`
		Contains       string `json:"contains"`
		MaxWaitSeconds int    `json:"max_wait_seconds"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	target := strings.TrimSpace(args.URL)
	if target == "" {
		return "", fmt.Errorf("url is required")
	}
	if !isAllowedPreviewURL(target) {
		return "", fmt.Errorf("url must be an https://*.cloudfront.net preview URL (got %q) — verify_preview only fetches CloudFront preview hosts", target)
	}

	maxWait := verifyPreviewDefaultMaxWait
	if args.MaxWaitSeconds > 0 {
		maxWait = time.Duration(args.MaxWaitSeconds) * time.Second
		if maxWait > verifyPreviewMaxWaitCap {
			maxWait = verifyPreviewMaxWaitCap
		}
	}

	res := pollURL(ctx, target, args.Contains, maxWait, verifyPreviewPollInterval, logsWriter)
	b, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// isAllowedPreviewURL is the SSRF guard: only https URLs whose host is under
// *.cloudfront.net. Blocks cloud-metadata IPs, localhost, internal hosts, and
// lookalike domains (evil.cloudfront.net.attacker.com ends in .attacker.com, and
// bare cloudfront.net has no leading dot).
func isAllowedPreviewURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, ".cloudfront.net")
}

// pollURL GETs target until it returns 200 (and, when contains != "", the body
// holds it) or maxWait elapses. Host-agnostic — the handler applies the SSRF
// allowlist before calling this.
func pollURL(ctx context.Context, target, contains string, maxWait, interval time.Duration, logsWriter io.Writer) verifyPreviewResult {
	// Floor the interval. The guaranteed sleep between attempts is what caps the
	// attempt count (together with the maxWait deadline); a non-positive interval
	// would turn the loop into a tight spin, so never allow that regardless of caller.
	if interval <= 0 {
		interval = verifyPreviewPollInterval
	}
	ctx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	client := &http.Client{Timeout: verifyPreviewPerRequestTO}
	start := time.Now()
	wantContains := contains != ""

	attempts := 0
	var lastStatus int
	var lastSnippet string
	for {
		attempts++
		status, body, _ := fetchOnce(ctx, client, target)
		// Record only a real HTTP response — a network error (status 0), e.g. the
		// final fetch racing the deadline, must not clobber the last observed status.
		if status != 0 {
			lastStatus = status
			lastSnippet = snippet(body, verifyPreviewSnippetRunes)
		}
		// A 200 means the URL is reachable → LIVE. `contains` is a separate content
		// assertion, reported but NOT a gate: we do NOT keep polling a live URL hoping
		// a substring appears (the served body is already the deployed content, and
		// for an SPA the visible text is client-rendered so it's never in the shell).
		if status == http.StatusOK {
			res := verifyPreviewResult{
				Live:           true,
				StatusCode:     status,
				Attempts:       attempts,
				ElapsedSeconds: int(time.Since(start).Seconds()),
				BodySnippet:    lastSnippet,
				Message:        "preview is live",
			}
			if wantContains {
				matched := strings.Contains(body, contains)
				res.Matched = &matched
				if !matched {
					res.Message = "preview is LIVE (HTTP 200), but the expected substring was not found in the response body. For an SPA this is expected — the page text is client-rendered, not in the HTML shell — so treat the 200 as success and confirm the change in the built bundle."
				}
			}
			return res
		}
		// Not reachable yet (status 0 / non-200). Log periodically, not every tick.
		if logsWriter != nil && (attempts == 1 || attempts%6 == 0) {
			io.WriteString(logsWriter, fmt.Sprintf("verify_preview: attempt %d — status=%d, not reachable yet, still polling...\n", attempts, status))
		}
		select {
		case <-ctx.Done():
			return verifyPreviewResult{
				Live:           false,
				StatusCode:     lastStatus,
				Attempts:       attempts,
				ElapsedSeconds: int(time.Since(start).Seconds()),
				BodySnippet:    lastSnippet,
				Message:        "timed out waiting for the preview to become reachable — a first-time distribution can take a few minutes to propagate; call again (optionally with a larger max_wait_seconds).",
			}
		case <-time.After(interval):
		}
	}
}

func fetchOnce(ctx context.Context, client *http.Client, target string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", "deployment-io-verify/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, verifyPreviewReadLimitBytes))
	return resp.StatusCode, string(b), nil
}

// snippet returns the first maxRunes runes of s (trimmed), rune-safe, with an
// ellipsis when truncated.
func snippet(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}
