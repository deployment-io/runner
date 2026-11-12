package agenttools

// verify_preview_rendered_sign_in.go is verify_preview_rendered's `as`: in a stack-path
// QA round, a check of a site preview's page signed in as a role of one of the backend
// previews the site was built to call. The browser signs in on the app's own sign-in
// page (verify-browser 1.5.0's sign_in) with the role's test user before the check.
//
// SECURITY: the agent names only the role (and, when two backends have it, the
// backend); the runner puts the test user's email and password in the check's
// input.json, which is removed after the check. The password is never in a log or a
// result: in a run that can sign in, every result, error and log line of the tool is
// redacted of the run's test user passwords. The email is a generated example.com
// address the pages show, and it is never logged.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

const (
	// renderedMaxRefusedCookies caps sign_in.refused_cookies.
	renderedMaxRefusedCookies = 20
	// renderedCookieChars caps a refused cookie's host and name.
	renderedCookieChars = 200
	// renderedSignInLogReasonChars caps a sign-in's reason in the Step log.
	renderedSignInLogReasonChars = 200
	// renderedLoggedRefusedCookies is how many refused cookies the Step log names.
	renderedLoggedRefusedCookies = 3
)

// BrowserSignInSite is a site preview QA may check signed in: its URL and the
// backend previews it was built to call whose roles' test users the browser
// can sign in as.
type BrowserSignInSite struct {
	// URL is the site preview's URL, https://<host>.
	URL      string
	Backends []BrowserSignInBackend
}

// BrowserSignInBackend is a backend preview a site signs in to: its
// repository (as QA_PREVIEWS names it), its service's browser sign-in page
// (a path on the site, e.g. /signin) and its roles' test users, in the roles'
// saved order.
type BrowserSignInBackend struct {
	Repository string
	SignInPath string
	Users      []PreviewTestUser
}

// RegisterVerifyPreviewRenderedSignIn registers verify_preview_rendered for a
// stack-path QA round whose site previews can be checked signed in (sites; none
// is RegisterVerifyPreviewRendered). passwords are the run's test user
// passwords beside the sites' own: every result, error and log line of the tool
// is redacted of all of them.
func RegisterVerifyPreviewRenderedSignIn(s *agentmcp.Server, record *PreviewRecord, run RunVerifyBrowser, logsWriter io.Writer, sites []BrowserSignInSite, passwords []string) {
	registerVerifyPreviewRendered(s, newVerifyPreviewRenderedSignIn(record, run, logsWriter, sites, passwords))
}

// newVerifyPreviewRenderedSignIn is the tool's state for
// RegisterVerifyPreviewRenderedSignIn.
func newVerifyPreviewRenderedSignIn(record *PreviewRecord, run RunVerifyBrowser, logsWriter io.Writer, sites []BrowserSignInSite, passwords []string) *verifyPreviewRendered {
	t := &verifyPreviewRendered{record: record, run: run, logsWriter: logsWriter}
	if len(sites) > 0 {
		t.signIns = map[string]BrowserSignInSite{}
		secrets := append([]string{}, passwords...)
		for _, site := range sites {
			u, err := url.Parse(site.URL)
			if err != nil || u.Hostname() == "" {
				continue
			}
			t.signIns[strings.ToLower(u.Hostname())] = site
			for _, b := range site.Backends {
				for _, user := range b.Users {
					secrets = append(secrets, user.Password)
				}
			}
		}
		slices.Sort(secrets)
		t.redact = newAPIRedactor(slices.Compact(secrets))
	}
	return t
}

// browserSignInCheck is the sign-in of one check with as: the role, the
// backend it is a role of, that backend's sign-in path and the role's test
// user.
type browserSignInCheck struct {
	role, backend string
	path          string
	user          PreviewTestUser
}

// resolveSignIn is the sign-in of a check with as (nil without), or the tool
// error refusing it.
func (t *verifyPreviewRendered) resolveSignIn(args verifyRenderedArgs) (*browserSignInCheck, error) {
	if args.As == "" {
		if args.Backend != "" {
			return nil, fmt.Errorf("backend is only used with as")
		}
		return nil, nil
	}
	if t.readOnly {
		return nil, fmt.Errorf("as is not available on production checks")
	}
	host := args.URL
	if u, err := url.Parse(args.URL); err == nil && u.Hostname() != "" {
		host = strings.ToLower(u.Hostname())
	}
	site, ok := t.signIns[host]
	if !ok {
		return nil, fmt.Errorf("as works only on a preview that lists browser sign-in roles — got %s", capChars(host, renderedShrunkURLChars))
	}
	backends := site.Backends
	if args.Backend != "" {
		i := slices.IndexFunc(backends, func(b BrowserSignInBackend) bool { return b.Repository == args.Backend })
		if i < 0 {
			return nil, fmt.Errorf("%s is not a backend this preview signs in to (its backends: %s)", capChars(args.Backend, renderedShrunkURLChars), strings.Join(signInRepositories(backends), ", "))
		}
		backends = backends[i : i+1]
	}
	var found []browserSignInCheck
	var roles []string
	for _, b := range backends {
		for _, user := range b.Users {
			if !slices.Contains(roles, user.Role) {
				roles = append(roles, user.Role)
			}
			if user.Role == args.As {
				found = append(found, browserSignInCheck{role: user.Role, backend: b.Repository, path: b.SignInPath, user: user})
			}
		}
	}
	switch {
	case len(found) == 0:
		return nil, fmt.Errorf("no test user for role %s on this preview (its roles: %s)", logRole(args.As), strings.Join(roles, ", "))
	case len(found) > 1:
		var repositories []string
		for _, f := range found {
			repositories = append(repositories, f.backend)
		}
		return nil, fmt.Errorf("role %s is a role of more than one backend this site preview calls (%s): pass backend too", logRole(args.As), strings.Join(repositories, ", "))
	}
	return &found[0], nil
}

func signInRepositories(backends []BrowserSignInBackend) []string {
	out := make([]string, 0, len(backends))
	for _, b := range backends {
		out = append(out, b.Repository)
	}
	return out
}

// input is input.json's sign_in for a check of checkURL: the sign-in page on
// the check's own host.
func (c *browserSignInCheck) input(checkURL string) *verifyBrowserSignIn {
	host := ""
	if u, err := url.Parse(checkURL); err == nil {
		host = u.Host
	}
	path := c.path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &verifyBrowserSignIn{URL: "https://" + host + path, Email: c.user.Email, Password: c.user.Password}
}

// logSuffix is the "checking …" line's " as <role> of <backend>", "" without as.
func (c *browserSignInCheck) logSuffix() string {
	if c == nil {
		return ""
	}
	return " as " + logRole(c.role) + " of " + c.backend
}

// renderedSignIn is a result's sign_in as the agent receives it: the image's,
// with the role and backend the check signed in as.
type renderedSignIn struct {
	As       string `json:"as"`
	Backend  string `json:"backend"`
	SignedIn bool   `json:"signed_in"`
	// Reason is set when not signed in, SignedOutOnLoad when signed in.
	Reason          *string                 `json:"reason,omitempty"`
	SignedOutOnLoad *bool                   `json:"signed_out_on_load,omitempty"`
	FailedRequests  []renderedFailedRequest `json:"failed_requests"`
	RefusedCookies  []renderedRefusedCookie `json:"refused_cookies"`
}

// renderedRefusedCookie is a cookie the browser refused while signing in.
type renderedRefusedCookie struct {
	Host   string `json:"host"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// UnmarshalJSON reads the image's sign_in; as and backend are the runner's, so
// the image can't set them.
func (s *renderedSignIn) UnmarshalJSON(b []byte) error {
	var image struct {
		SignedIn        bool                    `json:"signed_in"`
		Reason          string                  `json:"reason"`
		SignedOutOnLoad bool                    `json:"signed_out_on_load"`
		FailedRequests  []renderedFailedRequest `json:"failed_requests"`
		RefusedCookies  []renderedRefusedCookie `json:"refused_cookies"`
	}
	if err := json.Unmarshal(b, &image); err != nil {
		return err
	}
	*s = renderedSignIn{SignedIn: image.SignedIn, FailedRequests: image.FailedRequests, RefusedCookies: image.RefusedCookies}
	if image.SignedIn {
		s.SignedOutOnLoad = &image.SignedOutOnLoad
	} else {
		s.Reason = &image.Reason
	}
	return nil
}

// capRenderedSignIn applies sign_in's caps; lists the image left out are empty.
func capRenderedSignIn(s *renderedSignIn) {
	if s == nil {
		return
	}
	if s.Reason != nil {
		reason := capChars(*s.Reason, renderedDetailChars)
		s.Reason = &reason
	}
	s.FailedRequests = capFailedRequests(s.FailedRequests)
	if s.FailedRequests == nil {
		s.FailedRequests = []renderedFailedRequest{}
	}
	if len(s.RefusedCookies) > renderedMaxRefusedCookies {
		s.RefusedCookies = s.RefusedCookies[:renderedMaxRefusedCookies]
	}
	for i := range s.RefusedCookies {
		c := &s.RefusedCookies[i]
		c.Host = capChars(c.Host, renderedCookieChars)
		c.Name = capChars(c.Name, renderedCookieChars)
		c.Reason = capChars(c.Reason, renderedShortChars)
	}
	if s.RefusedCookies == nil {
		s.RefusedCookies = []renderedRefusedCookie{}
	}
}

// phrase is the message's first part: "signed in as owner", or "not signed in
// as owner (<reason>)".
func (s *renderedSignIn) phrase() string {
	if s.SignedIn {
		return "signed in as " + s.As
	}
	if s.Reason == nil || *s.Reason == "" {
		return "not signed in as " + s.As
	}
	return "not signed in as " + s.As + " (" + *s.Reason + ")"
}

// signInIgnoredError is the error for a check with as whose result has no
// sign_in: an image older than 1.5.0 ignored it.
func signInIgnoredError(imageRef string) error {
	browser := "this runner's QA browser"
	if imageRef != "" {
		browser += " (" + imageRef + ")"
	}
	return fmt.Errorf("QA couldn't check the page: %s ignored the sign-in; it needs verify-browser %s or later", browser, verifyBrowserSignInVersion)
}

// logSignIn writes how a check with as signed in to the Step log (nothing for
// a check without): never an email or a password.
func (t *verifyPreviewRendered) logSignIn(s *renderedSignIn) {
	if s == nil {
		return
	}
	role := logRole(s.As)
	if s.SignedIn {
		t.logf("verify_preview_rendered: signed in as %s", role)
	} else {
		reason := ""
		if s.Reason != nil {
			reason = *s.Reason
		}
		t.logf("verify_preview_rendered: not signed in as %s: %s", role, capChars(strings.Join(strings.Fields(reason), " "), renderedSignInLogReasonChars))
	}
	if n := len(s.RefusedCookies); n > 0 {
		var named []string
		for _, c := range s.RefusedCookies[:min(n, renderedLoggedRefusedCookies)] {
			named = append(named, fmt.Sprintf("%s from %s (%s)", oneLine(c.Name), oneLine(c.Host), oneLine(c.Reason)))
		}
		t.logf("verify_preview_rendered: the browser refused %d cookie(s): %s", n, strings.Join(named, ", "))
	}
}

// oneLine is s with its whitespace runs, line breaks included, as one space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// redactFragmentMin is the shortest part of a password redactOutcome redacts
// at the end of a string the image capped: shorter, a string's last letters
// would too often happen to start one; what is left shows too little of a
// 24-character password to matter.
const redactFragmentMin = 4

// redactOutcome is o redacted of the run's test user passwords (in a run with a
// set) before anything shortens it, so no cut — the runner's caps, shrinks and
// output tails — can split a password and leave a part the exact match misses.
// Cuts made before o got here are redacted too: every string of result.json
// the image capped (ending in "…") loses a password's start just before the
// mark, an error's stderr tail a password's end at its start, and the
// container output, when its start was cut, a password's end at
// its start.
func (t *verifyPreviewRendered) redactOutcome(o VerifyBrowserOutcome) VerifyBrowserOutcome {
	if t.signIns == nil || len(t.redact.secrets) == 0 {
		return o
	}
	o.Output = t.redact.cutEdges(o.Output, o.OutputCut, 1)
	if len(o.Result) > 0 {
		o.Result = t.redact.result(o.Result)
	}
	return o
}

// result is result.json with every string (and key) redacted; one that isn't
// JSON is redacted as bytes (it is reported as not valid JSON, never shown).
func (r apiRedactor) result(b []byte) []byte {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return r.bytes(b)
	}
	out, err := json.Marshal(r.jsonValue(v))
	if err != nil {
		return r.bytes(b)
	}
	return out
}

func (r apiRedactor) jsonValue(v any) any {
	switch x := v.(type) {
	case string:
		return r.capped(r.stderrTail(x))
	case []any:
		for i := range x {
			x[i] = r.jsonValue(x[i])
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[r.capped(k)] = r.jsonValue(val)
		}
		return out
	}
	return v
}

// capped is s redacted, and when it ends in "…" (the image's cap mark) also
// of a password's start of at least redactFragmentMin bytes before the mark.
func (r apiRedactor) capped(s string) string {
	const mark = "…"
	if !strings.HasSuffix(s, mark) {
		return r.text(s)
	}
	return r.cutEdges(strings.TrimSuffix(s, mark), false, redactFragmentMin) + mark
}

// stderrTailMark is what an error result's message is followed by when the
// image adds the last 1000 bytes of its script's stderr — a tail whose start
// may be cut, and not redacted when the check had no sign_in.
const stderrTailMark = "\nlast output of the browser script:\n"

// stderrTail is s redacted and, after each stderrTailMark in it, also of a
// password's end of at least redactFragmentMin bytes the image's cut left at
// the tail's start.
func (r apiRedactor) stderrTail(s string) string {
	parts := strings.Split(r.text(s), stderrTailMark)
	for i := 1; i < len(parts); i++ {
		if n := r.edge(parts[i], true); n >= redactFragmentMin {
			parts[i] = apiRedacted + parts[i][n:]
		}
	}
	return strings.Join(parts, stderrTailMark)
}

// cutEdges is s redacted, also of a password's start of at least min bytes at
// its end and, with head, of a password's end of at least min bytes at its
// start.
func (r apiRedactor) cutEdges(s string, head bool, min int) string {
	s = r.text(s)
	if n := r.edge(s, false); n >= min {
		s = s[:len(s)-n] + apiRedacted
	}
	if head {
		if n := r.edge(s, true); n >= min {
			s = apiRedacted + s[n:]
		}
	}
	return s
}

// edge is the length of the longest part of a password (shorter than it) that
// s ends with and that starts the password, or with head, that s starts with
// and that ends the password.
func (r apiRedactor) edge(s string, head bool) int {
	longest := 0
	for _, secret := range r.secrets {
		for k := min(len(secret)-1, len(s)); k > longest; k-- {
			if head && strings.HasPrefix(s, string(secret[len(secret)-k:])) ||
				!head && strings.HasSuffix(s, string(secret[:k])) {
				longest = k
				break
			}
		}
	}
	return longest
}

// unescapeRedactedMark is the JSON text with every "\u003credacted\u003e" — the
// mark as json.Marshal writes it — written as "<redacted>". An occurrence after
// an odd number of backslashes isn't an escape but text, and is kept.
func unescapeRedactedMark(text string) string {
	const escaped = `\u003credacted\u003e`
	var b strings.Builder
	for {
		i := strings.Index(text, escaped)
		if i < 0 {
			break
		}
		slashes := 0
		for j := i - 1; j >= 0 && text[j] == '\\'; j-- {
			slashes++
		}
		b.WriteString(text[:i])
		if slashes%2 == 0 {
			b.WriteString(apiRedacted)
		} else {
			b.WriteString(escaped)
		}
		text = text[i+len(escaped):]
	}
	b.WriteString(text)
	return b.String()
}
