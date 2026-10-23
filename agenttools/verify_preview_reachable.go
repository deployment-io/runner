package agenttools

// verify_preview_reachable.go implements the agent-invoked `verify_preview_reachable` MCP tool: after
// deploying a preview, the agent calls this to confirm the URL is actually live and
// serving. It runs on the RUNNER, not the agent — the agentbox proxy allowlist
// blocks arbitrary outbound hosts, so the agent can't reach the CloudFront URL
// itself; the runner (open network) polls it and returns the result.
//
// SECURITY: the runner fetches an agent-supplied URL, which is an SSRF vector — a
// prompt-injected agent could ask it to fetch cloud-metadata (169.254.169.254), an
// internal host, or someone else's site and exfiltrate the response through the tool
// result. So verify_preview_reachable validates every URL against the record of
// previews this Task deployed (PreviewRecord): it fetches a URL only when it is https
// on the default port, carries no user info, and its host is a preview host this
// Task deployed — one deploy_static_site_preview or deploy_web_service_preview
// returned in this run, or one the control plane lists for the Task. Redirects are followed only when their target
// passes the same check. That record is the whole SSRF defense.

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
	// Kept UNDER the agent's MCP client tool-call timeout (~120s observed for codex).
	// If a single verify call ran longer, the client abandons it — a messy client-side
	// timeout plus an orphaned server-side poll — instead of getting our clean
	// "not live yet, call again" result. First-deploy CloudFront propagation (~130s)
	// therefore takes two calls: the tool returns cleanly and the agent retries.
	verifyPreviewReachableDefaultMaxWait = 90 * time.Second
	verifyPreviewReachableMaxWaitCap     = 110 * time.Second
	verifyPreviewReachablePollInterval   = 5 * time.Second
	verifyPreviewReachablePerRequestTO   = 10 * time.Second
	verifyPreviewReachableSnippetRunes   = 500
	verifyPreviewReachableReadLimitBytes = 64 * 1024
	// verifyPreviewReachableMaxRedirects matches net/http's default redirect limit.
	verifyPreviewReachableMaxRedirects = 10
)

const verifyPreviewReachableInputSchema = `{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The preview URL returned by deploy_static_site_preview, or the health_url deploy_web_service_preview returned. Only previews this Task deployed are accepted; redirects are followed only when they stay on such a preview."
    },
    "contains": {
      "type": "string",
      "description": "Optional ADVISORY substring check against the response body. It does NOT affect liveness — a 200 is always live — and is reported separately as matched. An SPA's body is the HTML shell, so client-rendered text will NOT be found here; for an SPA omit this and confirm the change in the built bundle instead."
    },
    "max_wait_seconds": {
      "type": "integer",
      "description": "How long to poll for the URL to first return 200 before returning (default 90, max 110). Kept under the agent tool-call timeout so the call returns cleanly; if it's not live yet, just call verify_preview_reachable again — a first-time distribution can take ~2 calls to catch."
    }
  },
  "required": ["url"]
}`

// verifyPreviewReachableResult is the JSON the agent receives.
type verifyPreviewReachableResult struct {
	Live           bool   `json:"live"`
	StatusCode     int    `json:"status_code"`
	Attempts       int    `json:"attempts"`
	ElapsedSeconds int    `json:"elapsed_seconds"`
	Matched        *bool  `json:"matched,omitempty"` // set only when `contains` was requested
	BodySnippet    string `json:"body_snippet,omitempty"`
	Message        string `json:"message,omitempty"`
}

// RegisterVerifyPreviewReachable registers the verify_preview_reachable tool. record is the
// agent run's preview record (shared with the preview deploy tools) that every URL is
// validated against. logsWriter is the Step Job's log writer; poll progress streams there.
func RegisterVerifyPreviewReachable(s *agentmcp.Server, record *PreviewRecord, logsWriter io.Writer) {
	s.Register(agentmcp.Tool{
		Name: "verify_preview_reachable",
		Description: "Confirm a deployed preview URL is live. Polls it until it returns HTTP 200 (a first-time " +
			"distribution can take a few minutes to propagate), and returns live=true as soon as it does — that 200 is " +
			"your success signal. Runs on the runner, so it works even though your sandbox can't reach the URL directly. " +
			"Only a preview URL this Task deployed (the URL deploy_static_site_preview or deploy_web_service_preview returned) is accepted, and " +
			"redirects are followed only when they stay on such a preview. Do NOT pass `contains` expecting rendered SPA text — an " +
			"SPA's body is the HTML shell, so verify the change in the built bundle instead.",
		InputSchema: json.RawMessage(verifyPreviewReachableInputSchema),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handleVerifyPreviewReachable(ctx, record, logsWriter, args)
		},
	})
}

func handleVerifyPreviewReachable(ctx context.Context, record *PreviewRecord, logsWriter io.Writer, rawArgs json.RawMessage) (string, error) {
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
	check := newPreviewURLCheck(record)
	if !check.allowedRaw(target) {
		return "", fmt.Errorf("url must be a preview URL this Task deployed (the URL deploy_static_site_preview or deploy_web_service_preview returned) — got %q", target)
	}

	maxWait := verifyPreviewReachableDefaultMaxWait
	if args.MaxWaitSeconds > 0 {
		maxWait = time.Duration(args.MaxWaitSeconds) * time.Second
		if maxWait > verifyPreviewReachableMaxWaitCap {
			maxWait = verifyPreviewReachableMaxWaitCap
		}
	}

	// After a deploy that created the host's distribution, "live" also needs one
	// other file of that upload served: a new distribution's edges don't all get
	// to read the bucket at once, so the front page can answer while the rest of
	// the site still returns 403.
	host := urlHost(target)
	probePath, probeToken := record.pendingProbe(host)
	probe := ""
	if probePath != "" {
		if p, ok := previewProbeURL(host, probePath); ok && check.allowedRaw(p) {
			probe = p
		}
	}
	res, probeServed := pollPreview(ctx, target, args.Contains, probe, probePath, maxWait, verifyPreviewReachablePollInterval, check.allowed, logsWriter)
	if probeServed {
		record.probeServed(host, probeToken)
	}
	b, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// previewProbeURL is https://<host>/<probePath> with each path segment escaped. ok
// is false unless the result parses back to host, so a probe path can never point
// the request elsewhere.
func previewProbeURL(host, probePath string) (string, bool) {
	segs := strings.Split(probePath, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	raw := "https://" + host + "/" + strings.Join(segs, "/")
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || !strings.EqualFold(u.Host, host) {
		return "", false
	}
	return raw, true
}

// previewHTTPTransport is the transport preview polling uses; nil is
// http.DefaultTransport. Tests point it at a local server.
var previewHTTPTransport http.RoundTripper

// redirectBlockedError is returned by the redirect policy when a redirect target
// isn't a preview this Task deployed.
type redirectBlockedError struct {
	host string
}

func (e *redirectBlockedError) Error() string {
	return fmt.Sprintf("redirected to %s, which is not a preview this Task deployed", e.host)
}

// previewRedirectPolicy is the http.Client CheckRedirect for preview polling: it
// follows a redirect only when allow accepts its target, and otherwise stops with a
// *redirectBlockedError (and onBlocked is called with it), so a preview can't bounce
// the runner to an arbitrary host. It also keeps net/http's redirect limit.
func previewRedirectPolicy(allow func(*url.URL) bool, onBlocked func(*redirectBlockedError)) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if allow == nil || !allow(req.URL) {
			blocked := &redirectBlockedError{host: req.URL.Hostname()}
			if onBlocked != nil {
				onBlocked(blocked)
			}
			// Stop here and hand the redirect response itself back to the caller.
			return http.ErrUseLastResponse
		}
		if len(via) >= verifyPreviewReachableMaxRedirects {
			return fmt.Errorf("stopped after %d redirects", verifyPreviewReachableMaxRedirects)
		}
		return nil
	}
}

// pollURL GETs target until it returns 200 or maxWait elapses. The handler validates
// target before calling this; allowRedirect validates each redirect target, and a
// redirect it refuses ends the poll with live=false.
func pollURL(ctx context.Context, target, contains string, maxWait, interval time.Duration, allowRedirect func(*url.URL) bool, logsWriter io.Writer) verifyPreviewReachableResult {
	res, _ := pollPreview(ctx, target, contains, "", "", maxWait, interval, allowRedirect, logsWriter)
	return res
}

// verifyPreviewProbePendingMessage is the result message when the front page
// answered but the probe file didn't within the call.
const verifyPreviewProbePendingMessage = "The front page answers, but the rest of the site isn't served everywhere yet (a new distribution is still propagating). Call verify_preview_reachable again — it returns as soon as it is."

// pollPreview is pollURL with a probe: when probe (the URL of probePath on the
// same preview) is set, a 200 from target is live only once probe has also returned
// 200 within the same maxWait. probeServed reports that it did.
func pollPreview(ctx context.Context, target, contains, probe, probePath string, maxWait, interval time.Duration, allowRedirect func(*url.URL) bool, logsWriter io.Writer) (res verifyPreviewReachableResult, probeServed bool) {
	// Floor the interval. The guaranteed sleep between attempts is what caps the
	// attempt count (together with the maxWait deadline); a non-positive interval
	// would turn the loop into a tight spin, so never allow that regardless of caller.
	if interval <= 0 {
		interval = verifyPreviewReachablePollInterval
	}
	ctx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	var blocked *redirectBlockedError
	client := &http.Client{
		Transport:     previewHTTPTransport,
		Timeout:       verifyPreviewReachablePerRequestTO,
		CheckRedirect: previewRedirectPolicy(allowRedirect, func(e *redirectBlockedError) { blocked = e }),
	}
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
			lastSnippet = snippet(body, verifyPreviewReachableSnippetRunes)
		}
		if blocked != nil {
			if logsWriter != nil {
				io.WriteString(logsWriter, fmt.Sprintf("verify_preview_reachable: %s — not following it\n", blocked.Error()))
			}
			return verifyPreviewReachableResult{
				Live:           false,
				StatusCode:     lastStatus,
				Attempts:       attempts,
				ElapsedSeconds: int(time.Since(start).Seconds()),
				BodySnippet:    lastSnippet,
				Message:        blocked.Error(),
			}, false
		}
		// A 200 means the URL is reachable → LIVE. `contains` is a separate content
		// assertion, reported but NOT a gate: we do NOT keep polling a live URL hoping
		// a substring appears (the served body is already the deployed content, and
		// for an SPA the visible text is client-rendered so it's never in the shell).
		if status == http.StatusOK {
			res := verifyPreviewReachableResult{
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
			if probe == "" {
				return res, false
			}
			return waitForProbe(ctx, client, probe, probePath, res, start, interval, func() *redirectBlockedError { return blocked }, logsWriter)
		}
		// Not reachable yet (status 0 / non-200). Log periodically, not every tick.
		if logsWriter != nil && (attempts == 1 || attempts%6 == 0) {
			io.WriteString(logsWriter, fmt.Sprintf("verify_preview_reachable: attempt %d — status=%d, not reachable yet, still polling...\n", attempts, status))
		}
		select {
		case <-ctx.Done():
			return verifyPreviewReachableResult{
				Live:           false,
				StatusCode:     lastStatus,
				Attempts:       attempts,
				ElapsedSeconds: int(time.Since(start).Seconds()),
				BodySnippet:    lastSnippet,
				Message:        "not reachable yet — a first-time CloudFront distribution takes a couple of minutes to start serving. This is normal; call verify_preview_reachable again to keep waiting (it returns as soon as it's live).",
			}, false
		case <-time.After(interval):
		}
	}
}

// waitForProbe polls probe (through pollPreview's client, so the same timeouts and
// redirect policy, until ctx's deadline) until it returns 200. live is the front
// page's live result; it stays live only if the probe is served.
func waitForProbe(ctx context.Context, client *http.Client, probe, probePath string, live verifyPreviewReachableResult,
	start time.Time, interval time.Duration, blocked func() *redirectBlockedError, logsWriter io.Writer) (verifyPreviewReachableResult, bool) {
	if logsWriter != nil {
		io.WriteString(logsWriter, fmt.Sprintf("verify_preview_reachable: the front page answers; waiting for %s to be served too\n", probePath))
	}
	notLive := func(message string) (verifyPreviewReachableResult, bool) {
		res := live
		res.Live = false
		res.ElapsedSeconds = int(time.Since(start).Seconds())
		res.Message = message
		return res, false
	}
	for {
		status, _, _ := fetchOnce(ctx, client, probe)
		if b := blocked(); b != nil {
			if logsWriter != nil {
				io.WriteString(logsWriter, fmt.Sprintf("verify_preview_reachable: %s — not following it\n", b.Error()))
			}
			return notLive(b.Error())
		}
		if status == http.StatusOK {
			live.ElapsedSeconds = int(time.Since(start).Seconds())
			return live, true
		}
		select {
		case <-ctx.Done():
			return notLive(verifyPreviewProbePendingMessage)
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
	b, _ := io.ReadAll(io.LimitReader(resp.Body, verifyPreviewReachableReadLimitBytes))
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
