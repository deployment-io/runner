package agenttools

// verify_preview_rendered_result.go turns what the verify-browser container left
// behind into the text the agent receives: the image's result.json, re-capped (the
// runner doesn't trust the container to have applied the contract's caps) and kept
// under 16 KB, with a one-line summary — or an error saying why QA couldn't check the
// page.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Caps of the verify-browser contract, re-applied here.
const (
	renderedMaxPageErrors       = 10
	renderedPageErrorChars      = 500
	renderedMaxConsoleErrors    = 20
	renderedConsoleErrorChars   = 300
	renderedMaxFailedRequests   = 20
	renderedMaxBlocked          = 10
	renderedDetailChars         = 300
	renderedDOMTextChars        = 1000
	renderedURLChars            = 2000
	renderedShortChars          = 40
	renderedImageErrorChars     = 1500
	renderedShrunkURLChars      = 200
	renderedShrunkDetailChars   = 100
	renderedShrunkPageErrorChar = 200
	renderedMaxOutline          = 60
	renderedOutlineTextChars    = 120
	renderedOutlineHrefChars    = 500
)

type renderedStepResult struct {
	Index  int    `json:"index"`
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
}

type renderedAssertionResult struct {
	Index  int    `json:"index"`
	Type   string `json:"type"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type renderedFailedRequest struct {
	URL     string `json:"url"`
	Method  string `json:"method"`
	Status  int    `json:"status"`
	Failure string `json:"failure"`
}

// renderedBlockedRequest is one request a read-only check refused
// (verify-browser 1.3.0+): a method other than GET, HEAD or OPTIONS, or a
// refused channel (WEBSOCKET, SHAREDWORKER, WEBTRANSPORT, WEBRTC).
type renderedBlockedRequest struct {
	URL    string `json:"url"`
	Method string `json:"method"`
}

type renderedTimings struct {
	DOMContentLoaded *int `json:"dom_content_loaded,omitempty"`
	Load             *int `json:"load,omitempty"`
	LCP              *int `json:"lcp,omitempty"`
}

// renderedOutlineEntry is one entry of the page outline (verify-browser 1.1.0+).
type renderedOutlineEntry struct {
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Level *int   `json:"level,omitempty"`
	Href  string `json:"href,omitempty"`
}

// renderedViewport is the window size the check used (verify-browser 1.2.0+).
type renderedViewport struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Preset string `json:"preset,omitempty"`
	Mobile bool   `json:"mobile"`
}

// renderedResult is the image's result.json as the agent receives it, plus message.
type renderedResult struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
	Verdict       string `json:"verdict"`
	Message       string `json:"message"`
	HTTPStatus    int    `json:"http_status"`
	FinalURL      string `json:"final_url"`
	// Viewport is absent from results before 1.2.0, and then from the reply.
	Viewport           *renderedViewport         `json:"viewport,omitempty"`
	Steps              []renderedStepResult      `json:"steps"`
	Assertions         []renderedAssertionResult `json:"assertions"`
	PageErrors         []string                  `json:"page_errors"`
	ConsoleErrors      []string                  `json:"console_errors"`
	FailedRequests     []renderedFailedRequest   `json:"failed_requests"`
	BlockedNavigations []string                  `json:"blocked_navigations"`
	// BlockedRequests is present in read-only checks only.
	BlockedRequests []renderedBlockedRequest `json:"blocked_requests,omitempty"`
	TimingsMS       renderedTimings          `json:"timings_ms"`
	DOMTextSnippet  string                   `json:"dom_text_snippet"`
	// Outline is absent from 1.0.0 results, and then from the reply.
	Outline []renderedOutlineEntry `json:"outline,omitempty"`
	// SignIn is how a check with as signed in (verify-browser 1.5.0+); absent
	// from every other check's reply.
	SignIn *renderedSignIn `json:"sign_in,omitempty"`
	// Truncated is set when parts of the result were shortened or dropped to keep
	// it under verifyRenderedMaxResultBytes.
	Truncated bool `json:"truncated,omitempty"`
}

// renderVerifyBrowserOutcome returns the tool's JSON text and the verdict of a
// check without as, or an error that says QA couldn't check the page and why
// (renderVerifyBrowserCheck).
func renderVerifyBrowserOutcome(o VerifyBrowserOutcome, wantViewport bool) (string, string, error) {
	text, r, err := renderVerifyBrowserCheck(o, renderedCheck{wantViewport: wantViewport})
	if err != nil {
		return "", "", err
	}
	return text, r.Verdict, nil
}

// renderedCheck is what a check asked of the browser that its result must
// show: wantViewport is whether the agent asked for a window size, signIn the
// check's sign-in (nil without as).
type renderedCheck struct {
	wantViewport bool
	signIn       *browserSignInCheck
}

// renderVerifyBrowserCheck returns the tool's JSON text and the result it was
// made from, or an error that says QA couldn't check the page and why. A
// completed result without a viewport the agent asked for is from an image that
// ignored the request, and is not passed off as seen at that size. A check with
// as must have sign_in, which the reply carries with the role and backend; a
// check without as never has one in its reply.
func renderVerifyBrowserCheck(o VerifyBrowserOutcome, check renderedCheck) (string, *renderedResult, error) {
	switch {
	case o.TimedOut:
		return "", nil, couldntCheck("the browser did not finish in time and was stopped", o.Output)
	case o.OOMKilled:
		return "", nil, couldntCheck("the browser ran out of memory (the QA browser has 1 GiB)", o.Output)
	case len(o.Result) == 0:
		return "", nil, couldntCheck(fmt.Sprintf("the browser exited (code %d) without a result", o.ExitCode), o.Output)
	}
	var r renderedResult
	if err := json.Unmarshal(o.Result, &r); err != nil {
		return "", nil, couldntCheck("the browser's result is not valid JSON", o.Output)
	}
	if r.SchemaVersion != 1 {
		return "", nil, couldntCheck(fmt.Sprintf("the browser's result has schema_version %d, want 1", r.SchemaVersion), o.Output)
	}
	switch {
	case r.Status == "error":
		return "", nil, fmt.Errorf("QA couldn't check the page: %s", capChars(r.Error, renderedImageErrorChars))
	case r.Status != "completed" || (r.Verdict != "pass" && r.Verdict != "fail"):
		return "", nil, couldntCheck(fmt.Sprintf("the browser's result has status %q and verdict %q", capChars(r.Status, renderedShortChars), capChars(r.Verdict, renderedShortChars)), o.Output)
	}
	if check.signIn == nil {
		r.SignIn = nil
	} else {
		if r.SignIn == nil {
			return "", nil, signInIgnoredError(o.ImageRef)
		}
		r.SignIn.As, r.SignIn.Backend = check.signIn.role, check.signIn.backend
	}
	if check.wantViewport && r.Viewport == nil {
		return "", nil, olderImageError(o.ImageRef)
	}
	capRenderedResult(&r)
	text, err := fitRenderedResult(&r)
	if err != nil {
		return "", nil, err
	}
	return text, &r, nil
}

// olderImageError is the error for a requested viewport the image ignored.
func olderImageError(imageRef string) error {
	browser := "this runner's QA browser"
	if imageRef != "" {
		browser += " (" + imageRef + ")"
	}
	return fmt.Errorf("%s can't set the window size; it needs %s or later", browser, verifyRenderedViewportImage)
}

// couldntCheck is the error for a check with no usable result; it carries the tail of
// the container's output.
func couldntCheck(why, output string) error {
	msg := "QA couldn't check the page: " + why
	if tail := strings.TrimSpace(tailBytes(output, verifyRenderedOutputTailBytes)); tail != "" {
		msg += "\nLast output of the QA browser:\n" + tail
	}
	return errors.New(msg)
}

// tailBytes returns the last n bytes of s, starting on a rune boundary.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !isRuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// capChars truncates s to max runes, marking the cut.
func capChars(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func capStrings(list []string, n, chars int) []string {
	if len(list) > n {
		list = list[:n]
	}
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = capChars(s, chars)
	}
	return out
}

// capRenderedResult applies the contract's caps.
func capRenderedResult(r *renderedResult) {
	r.Error = ""
	r.FinalURL = capChars(r.FinalURL, renderedURLChars)
	if r.Viewport != nil {
		r.Viewport.Preset = capChars(r.Viewport.Preset, renderedShortChars)
	}
	if len(r.Steps) > verifyRenderedMaxSteps {
		r.Steps = r.Steps[:verifyRenderedMaxSteps]
	}
	for i := range r.Steps {
		r.Steps[i].Action = capChars(r.Steps[i].Action, renderedShortChars)
		r.Steps[i].Error = capChars(r.Steps[i].Error, renderedDetailChars)
	}
	if len(r.Assertions) > verifyRenderedMaxAssertions {
		r.Assertions = r.Assertions[:verifyRenderedMaxAssertions]
	}
	for i := range r.Assertions {
		r.Assertions[i].Type = capChars(r.Assertions[i].Type, renderedShortChars)
		r.Assertions[i].Detail = capChars(r.Assertions[i].Detail, renderedDetailChars)
	}
	r.PageErrors = capStrings(r.PageErrors, renderedMaxPageErrors, renderedPageErrorChars)
	r.ConsoleErrors = capStrings(r.ConsoleErrors, renderedMaxConsoleErrors, renderedConsoleErrorChars)
	r.FailedRequests = capFailedRequests(r.FailedRequests)
	r.BlockedNavigations = capStrings(r.BlockedNavigations, renderedMaxBlocked, renderedURLChars)
	if len(r.BlockedRequests) > renderedMaxBlocked {
		r.BlockedRequests = r.BlockedRequests[:renderedMaxBlocked]
	}
	for i := range r.BlockedRequests {
		b := &r.BlockedRequests[i]
		b.URL = capChars(b.URL, renderedURLChars)
		b.Method = capChars(b.Method, renderedShortChars)
	}
	r.DOMTextSnippet = capChars(r.DOMTextSnippet, renderedDOMTextChars)
	capRenderedSignIn(r.SignIn)
	if len(r.Outline) > renderedMaxOutline {
		r.Outline = r.Outline[:renderedMaxOutline]
	}
	for i := range r.Outline {
		e := &r.Outline[i]
		e.Kind = capChars(e.Kind, renderedShortChars)
		e.Text = capChars(e.Text, renderedOutlineTextChars)
		e.Href = capChars(e.Href, renderedOutlineHrefChars)
	}
	// Lists the image left out are empty, not null.
	if r.Steps == nil {
		r.Steps = []renderedStepResult{}
	}
	if r.Assertions == nil {
		r.Assertions = []renderedAssertionResult{}
	}
	if r.FailedRequests == nil {
		r.FailedRequests = []renderedFailedRequest{}
	}
	r.Message = renderedSummary(r)
}

// capFailedRequests applies failed_requests' caps; nil stays nil.
func capFailedRequests(list []renderedFailedRequest) []renderedFailedRequest {
	if len(list) > renderedMaxFailedRequests {
		list = list[:renderedMaxFailedRequests]
	}
	for i := range list {
		f := &list[i]
		f.URL = capChars(f.URL, renderedURLChars)
		f.Method = capChars(f.Method, renderedShortChars)
		f.Failure = capChars(f.Failure, renderedDetailChars)
	}
	return list
}

// signInFailedRequests is r's sign-in's failed requests (nil without one), for
// the shrinks that shorten failed_requests to shorten them too.
func (r *renderedResult) signInFailedRequests() *[]renderedFailedRequest {
	if r.SignIn == nil {
		return new([]renderedFailedRequest)
	}
	return &r.SignIn.FailedRequests
}

// renderedShrinks are applied in order until the result fits: the least useful
// parts go first.
var renderedShrinks = []func(r *renderedResult){
	func(r *renderedResult) { r.ConsoleErrors = capStrings(r.ConsoleErrors, 5, 200) },
	func(r *renderedResult) {
		r.FinalURL = capChars(r.FinalURL, renderedShrunkURLChars)
		r.BlockedNavigations = capStrings(r.BlockedNavigations, renderedMaxBlocked, renderedShrunkURLChars)
		for i := range r.BlockedRequests {
			r.BlockedRequests[i].URL = capChars(r.BlockedRequests[i].URL, renderedShrunkURLChars)
		}
		for _, list := range []*[]renderedFailedRequest{&r.FailedRequests, r.signInFailedRequests()} {
			for i := range *list {
				(*list)[i].URL = capChars((*list)[i].URL, renderedShrunkURLChars)
			}
		}
	},
	func(r *renderedResult) { r.ConsoleErrors = []string{} },
	func(r *renderedResult) {
		r.PageErrors = capStrings(r.PageErrors, renderedMaxPageErrors, renderedShrunkPageErrorChar)
	},
	// The outline is halved a few times, then dropped, before the DOM text is cut.
	halveOutline,
	halveOutline,
	halveOutline,
	func(r *renderedResult) { r.Outline = nil },
	func(r *renderedResult) { r.DOMTextSnippet = capChars(r.DOMTextSnippet, 300) },
	func(r *renderedResult) {
		for i := range r.Steps {
			r.Steps[i].Error = capChars(r.Steps[i].Error, renderedShrunkDetailChars)
		}
		for i := range r.Assertions {
			r.Assertions[i].Detail = capChars(r.Assertions[i].Detail, renderedShrunkDetailChars)
		}
		for _, list := range []*[]renderedFailedRequest{&r.FailedRequests, r.signInFailedRequests()} {
			for i := range *list {
				(*list)[i].Failure = capChars((*list)[i].Failure, renderedShrunkDetailChars)
			}
		}
	},
	func(r *renderedResult) {
		r.PageErrors = capStrings(r.PageErrors, 3, renderedShrunkPageErrorChar)
		for _, list := range []*[]renderedFailedRequest{&r.FailedRequests, r.signInFailedRequests()} {
			if len(*list) > 5 {
				*list = (*list)[:5]
			}
		}
		r.DOMTextSnippet = ""
	},
}

func halveOutline(r *renderedResult) { r.Outline = r.Outline[:len(r.Outline)/2] }

// fitRenderedResult marshals r, shrinking it until it is under the size cap. Counts
// in the summary are taken before shrinking, so they stay true.
func fitRenderedResult(r *renderedResult) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	for _, shrink := range renderedShrinks {
		if len(b) <= verifyRenderedMaxResultBytes {
			break
		}
		shrink(r)
		if !r.Truncated {
			r.Truncated = true
			r.Message += " (result shortened to fit)"
		}
		if b, err = json.Marshal(r); err != nil {
			return "", err
		}
	}
	if len(b) > verifyRenderedMaxResultBytes {
		return "", fmt.Errorf("QA couldn't report on the page: its result is larger than %d bytes even after shortening", verifyRenderedMaxResultBytes)
	}
	return string(b), nil
}

// renderedSummary is the one-line message, e.g. "pass — 3 of 3 assertions passed;
// no page errors; no failed requests".
func renderedSummary(r *renderedResult) string {
	var parts []string
	if r.SignIn != nil {
		parts = append(parts, r.SignIn.phrase())
	}
	if r.HTTPStatus < 200 || r.HTTPStatus > 299 {
		if r.HTTPStatus == 0 {
			parts = append(parts, "the first load got no response")
		} else {
			parts = append(parts, fmt.Sprintf("the first load returned HTTP %d", r.HTTPStatus))
		}
	}
	if len(r.Steps) > 0 {
		parts = append(parts, stepsSummary(r.Steps))
	}
	if len(r.Assertions) > 0 {
		passed := 0
		for _, a := range r.Assertions {
			if a.Passed {
				passed++
			}
		}
		parts = append(parts, fmt.Sprintf("%d of %d assertions passed", passed, len(r.Assertions)))
	}
	parts = append(parts, countPhrase(len(r.PageErrors), "page error", "page errors"))
	parts = append(parts, countPhrase(len(r.FailedRequests), "failed request", "failed requests"))
	if n := len(r.BlockedNavigations); n > 0 {
		parts = append(parts, countPhrase(n, "blocked navigation", "blocked navigations"))
	}
	if n := len(r.BlockedRequests); n > 0 {
		parts = append(parts, countPhrase(n, "blocked request", "blocked requests"))
	}
	if n := len(r.Outline); n > 0 {
		parts = append(parts, "outline: "+countPhrase(n, "entry", "entries"))
	}
	return r.Verdict + viewportPhrase(r.Viewport) + " — " + strings.Join(parts, "; ")
}

// viewportPhrase is " at 412×839 (mobile)", or "" for a result without a viewport.
func viewportPhrase(v *renderedViewport) string {
	if v == nil {
		return ""
	}
	phrase := fmt.Sprintf(" at %d×%d", v.Width, v.Height)
	if v.Preset != "" {
		phrase += " (" + v.Preset + ")"
	}
	return phrase
}

func stepsSummary(steps []renderedStepResult) string {
	for _, s := range steps {
		if !s.OK {
			return fmt.Sprintf("step %d (%s) failed", s.Index, s.Action)
		}
	}
	return fmt.Sprintf("all %d steps succeeded", len(steps))
}

func countPhrase(n int, one, many string) string {
	switch n {
	case 0:
		return "no " + many
	case 1:
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
