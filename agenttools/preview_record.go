package agenttools

// preview_record.go holds the per-agent-run record of the preview hosts this Task
// deployed — the SSRF allowlist verify_preview_reachable checks every URL (and every
// redirect target) against. deploy_static_site_preview adds the host of each URL it
// returns; for a host not in the record, verify_preview_reachable asks the control
// plane for the Task's previews (ListPreviewURLs) once per call and adds their hosts,
// so a preview deployed in an earlier run of the Task is accepted too.

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
)

// PreviewRecord is the set of preview hosts this Task deployed, shared by the preview
// tools of one agent run. Safe for concurrent use. A nil *PreviewRecord is an empty
// record with no lister.
type PreviewRecord struct {
	// listPreviewURLs returns the URLs of the Task's previews as the control plane
	// knows them. Optional; an error is logged to LogsWriter and treated as an empty
	// list.
	listPreviewURLs func() ([]string, error)
	logsWriter      io.Writer

	mu    sync.Mutex
	hosts map[string]struct{}
}

// NewPreviewRecord returns an empty record. listPreviewURLs (may be nil) lists the
// Task's preview URLs; logsWriter (may be nil) is the Step Job's log writer.
func NewPreviewRecord(listPreviewURLs func() ([]string, error), logsWriter io.Writer) *PreviewRecord {
	return &PreviewRecord{
		listPreviewURLs: listPreviewURLs,
		logsWriter:      logsWriter,
		hosts:           map[string]struct{}{},
	}
}

// AddURL records the host of rawURL. A URL that doesn't parse or has an empty host
// adds nothing.
func (r *PreviewRecord) AddURL(rawURL string) {
	if r == nil {
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return
	}
	r.mu.Lock()
	r.hosts[host] = struct{}{}
	r.mu.Unlock()
}

func (r *PreviewRecord) has(host string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.hosts[strings.ToLower(host)]
	return ok
}

// addListed asks the control plane for the Task's previews and records their hosts.
func (r *PreviewRecord) addListed() {
	if r == nil || r.listPreviewURLs == nil {
		return
	}
	urls, err := r.listPreviewURLs()
	if err != nil {
		if r.logsWriter != nil {
			io.WriteString(r.logsWriter, fmt.Sprintf("verify_preview_reachable: could not list the Task's previews, checking only the previews deployed in this run: %s\n", err))
		}
		return
	}
	for _, u := range urls {
		r.AddURL(u)
	}
}

// previewURLCheck is one verify_preview_reachable call's view of the record: it lists
// the Task's previews at most once, the first time a URL's host isn't recorded.
type previewURLCheck struct {
	record *PreviewRecord
	listed bool
}

func newPreviewURLCheck(record *PreviewRecord) *previewURLCheck {
	return &previewURLCheck{record: record}
}

// allowed is the SSRF guard: u must be https, carry no user info, use the default
// port (empty or 443), and its host must equal (case-insensitively) a host in the
// record. Everything else — cloud metadata, internal hosts, other CloudFront
// distributions, lookalike domains — is refused.
func (c *previewURLCheck) allowed(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if c.record.has(host) {
		return true
	}
	if c.listed {
		return false
	}
	c.listed = true
	c.record.addListed()
	return c.record.has(host)
}

// allowedRaw parses raw and applies allowed.
func (c *previewURLCheck) allowedRaw(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return c.allowed(u)
}
