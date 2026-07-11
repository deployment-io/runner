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
	verifyPreviewDefaultMaxWait = 120 * time.Second
	verifyPreviewMaxWaitCap     = 240 * time.Second
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
      "description": "Optional substring to require in the served response body. NOTE: an SPA's body is the HTML shell, so client-rendered text will NOT be present — use this for shell/asset markers (e.g. the <title>), not rendered UI."
    },
    "max_wait_seconds": {
      "type": "integer",
      "description": "How long to poll for the URL to go live before giving up (default 120, max 240). A first-time distribution may still be propagating."
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
		Description: "Confirm a deployed preview URL is live and serving. Polls it until it returns HTTP 200 " +
			"(a first-time distribution may take a few minutes to propagate — call again if it times out). Runs on the " +
			"runner, so it works even though your sandbox can't reach the URL directly. Only *.cloudfront.net preview " +
			"URLs are accepted. For an SPA the response is the HTML shell, so client-rendered text won't appear in the " +
			"body — check the built bundle for that.",
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
		matched := !wantContains || strings.Contains(body, contains)
		// Record only a real HTTP response — a network error (status 0), e.g. the
		// final fetch racing the deadline, must not clobber the last observed status.
		if status != 0 {
			lastStatus = status
			lastSnippet = snippet(body, verifyPreviewSnippetRunes)
		}
		if status == http.StatusOK && matched {
			res := verifyPreviewResult{
				Live:           true,
				StatusCode:     status,
				Attempts:       attempts,
				ElapsedSeconds: int(time.Since(start).Seconds()),
				BodySnippet:    lastSnippet,
				Message:        "preview is live",
			}
			if wantContains {
				t := true
				res.Matched = &t
			}
			return res
		}
		if logsWriter != nil {
			io.WriteString(logsWriter, fmt.Sprintf("verify_preview: attempt %d — status=%d, not live yet, retrying...\n", attempts, status))
		}
		select {
		case <-ctx.Done():
			msg := "timed out waiting for the preview to go live"
			if wantContains && lastStatus == http.StatusOK {
				msg = "preview responded 200 but the body did not contain the expected string (an SPA's body is the shell — client-rendered text won't be here)"
			}
			res := verifyPreviewResult{
				Live:           false,
				StatusCode:     lastStatus,
				Attempts:       attempts,
				ElapsedSeconds: int(time.Since(start).Seconds()),
				BodySnippet:    lastSnippet,
				Message:        msg,
			}
			if wantContains {
				f := false
				res.Matched = &f
			}
			return res
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
