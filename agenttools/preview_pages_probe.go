package agenttools

// preview_pages_probe.go finds out whether a backend preview serves pages: a
// server-rendered app (Express with templates, Next.js, Rails, Django, …) answers
// its front page with a web page, an API doesn't. A stack-path QA round probes each
// backend preview it deployed and tells QA, which then checks its pages in the
// browser like a site preview's.
//
// The probe fetches only the backend preview's own URLs (its health URL and /), and
// follows a redirect only when it stays on that preview's host.

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The probe's waits; tests shorten them.
var (
	// previewPagesHealthWait is how long a probe waits for the health check to
	// answer 200, from its start.
	previewPagesHealthWait = 10 * time.Minute
	// previewPagesHealthInterval is how often it polls the health check.
	previewPagesHealthInterval = 10 * time.Second
	// previewPagesRetryDelay is the wait before GET / is tried again.
	previewPagesRetryDelay = 5 * time.Second
)

const (
	// previewPagesRetries is how many more times GET / is tried after a
	// transport error or a 502, 503 or 504.
	previewPagesRetries = 2
	// previewPagesErrorChars caps the error a failed GET / reports.
	previewPagesErrorChars = 200
)

// PreviewPagesProbe is what ProbeServesPages found: whether the backend preview
// serves pages, and why, e.g. "GET / answered 200 text/html".
type PreviewPagesProbe struct {
	ServesPages bool
	Reason      string
}

// ProbeServesPages reports whether the backend preview at previewURL serves pages:
// once its health check (healthURL) answers 200, GET / with Accept: text/html
// answers 2xx with a text/html Content-Type. Anything else — a health check that
// didn't answer within previewPagesHealthWait, another answer, or none — is an API.
func ProbeServesPages(ctx context.Context, previewURL, healthURL string) PreviewPagesProbe {
	front, err := url.Parse(previewURL)
	if err != nil || front.Hostname() == "" {
		return PreviewPagesProbe{Reason: "GET / failed: " + capChars(fmt.Sprintf("its URL %q isn't a URL", previewURL), previewPagesErrorChars)}
	}
	front = &url.URL{Scheme: front.Scheme, Host: front.Host, Path: "/"}
	sameHost := func(u *url.URL) bool {
		return u.Scheme == front.Scheme && strings.EqualFold(u.Hostname(), front.Hostname())
	}
	if !pollURL(ctx, healthURL, "", previewPagesHealthWait, previewPagesHealthInterval, sameHost, nil).Live {
		return PreviewPagesProbe{Reason: "its health check didn't answer within 10 minutes"}
	}
	client := &http.Client{
		Transport:     previewHTTPTransport,
		Timeout:       verifyPreviewReachablePerRequestTO,
		CheckRedirect: previewRedirectPolicy(sameHost, nil),
	}
	status, contentType, err := getFrontPage(ctx, client, front.String())
	for retry := 0; retry < previewPagesRetries && (err != nil || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout); retry++ {
		select {
		case <-ctx.Done():
			return PreviewPagesProbe{Reason: "GET / failed: " + ctx.Err().Error()}
		case <-time.After(previewPagesRetryDelay):
		}
		status, contentType, err = getFrontPage(ctx, client, front.String())
	}
	if err != nil {
		line, _, _ := strings.Cut(err.Error(), "\n")
		return PreviewPagesProbe{Reason: "GET / failed: " + capChars(strings.TrimSpace(line), previewPagesErrorChars)}
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "" {
		return PreviewPagesProbe{Reason: fmt.Sprintf("GET / answered %d with no content type", status)}
	}
	return PreviewPagesProbe{
		ServesPages: status >= 200 && status < 300 && mediaType == "text/html",
		Reason:      fmt.Sprintf("GET / answered %d %s", status, mediaType),
	}
}

// getFrontPage GETs target asking for a web page, and returns the final
// response's status and Content-Type without reading its body.
func getFrontPage(ctx context.Context, client *http.Client, target string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "deployment-io-verify/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Content-Type"), nil
}
