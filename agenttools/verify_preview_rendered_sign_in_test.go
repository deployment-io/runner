package agenttools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	signInSite     = "https://d111.cloudfront.net"
	ownerPassword  = "OwnerPassw0rdOwnerPassw0"
	memberPassword = "MemberPassw0rdMemberPass"
	adminPassword  = "AdminPassw0rdAdminPassw0"
	runPassword    = "OtherPassw0rdOtherPassw0"
)

// signInSites is one site calling two backends: owner is a role of both.
func signInSites() []BrowserSignInSite {
	return []BrowserSignInSite{{URL: signInSite, Backends: []BrowserSignInBackend{
		{Repository: "1-acme/api", SignInPath: "/signin", Users: []PreviewTestUser{
			{Role: "owner", Email: "qa-owner-aaaa1111@example.com", Password: ownerPassword},
			{Role: "member", Email: "qa-member-aaaa1111@example.com", Password: memberPassword},
		}},
		{Repository: "2-acme/auth", SignInPath: "/login", Users: []PreviewTestUser{
			{Role: "owner", Email: "qa-owner-bbbb2222@example.com", Password: adminPassword},
		}},
	}}}
}

func newSignInTool(fb *fakeBrowser, sites []BrowserSignInSite) (*verifyPreviewRendered, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	record := renderedRecord(signInSite+"/", "https://d222.cloudfront.net/")
	return newVerifyPreviewRenderedSignIn(record, fb.run, logs, sites, []string{runPassword}), logs
}

// signInResult is passResult with sign_in.
func signInResult(signIn string) []byte {
	return []byte(strings.Replace(string(passResult()), `"dom_text_snippet":"Hello"`, `"dom_text_snippet":"Hello","sign_in":`+signIn, 1))
}

func TestVerifyRenderedAs_InputHasTheSignIn(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: signInResult(`{"signed_in":true,"signed_out_on_load":false,"failed_requests":[],"refused_cookies":[]}`)}}
	tool, logs := newSignInTool(fb, signInSites())
	if _, err := call(t, tool, `{"url":"https://D111.cloudfront.net/","as":" member ","steps":[{"action":"back"}]}`); err != nil {
		t.Fatal(err)
	}
	in := fb.input(t)
	want := verifyBrowserSignIn{URL: "https://D111.cloudfront.net/signin", Email: "qa-member-aaaa1111@example.com", Password: memberPassword}
	if in.SignIn == nil || *in.SignIn != want {
		t.Errorf("sign_in = %+v, want %+v", in.SignIn, want)
	}
	req := fb.requests[0]
	if req.MinImageVersion != "1.5.0" || req.MinImageFeature != "signing in" {
		t.Errorf("MinImageVersion %q, MinImageFeature %q", req.MinImageVersion, req.MinImageFeature)
	}
	if in.TimeoutMS != 60000 || req.Timeout != 60*time.Second {
		t.Errorf("timeout_ms %d, timeout %s; want 60 s", in.TimeoutMS, req.Timeout)
	}
	if !strings.Contains(logs.String(), "(1 steps, 0 assertions, timeout 60s) as member of 1-acme/api\n") {
		t.Errorf("logs = %q", logs.String())
	}
	if !strings.Contains(logs.String(), "verify_preview_rendered: signed in as member\n") {
		t.Errorf("logs lack the sign-in line: %q", logs.String())
	}
	if strings.Contains(logs.String(), "example.com") || strings.Contains(logs.String(), memberPassword) {
		t.Errorf("logs carry the test user: %q", logs.String())
	}

	// timeout_seconds still wins.
	fb = &fakeBrowser{outcome: fb.outcome}
	tool, _ = newSignInTool(fb, signInSites())
	if _, err := call(t, tool, `{"url":"`+signInSite+`/","as":"member","timeout_seconds":30}`); err != nil {
		t.Fatal(err)
	}
	if in := fb.input(t); in.TimeoutMS != 30000 {
		t.Errorf("timeout_ms = %d, want 30000", in.TimeoutMS)
	}
}

func TestVerifyRenderedAs_RoleOfTwoBackends(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: signInResult(`{"signed_in":true,"failed_requests":[],"refused_cookies":[]}`)}}
	tool, _ := newSignInTool(fb, signInSites())
	_, err := call(t, tool, `{"url":"`+signInSite+`/","as":"owner"}`)
	if want := "role owner is a role of more than one backend this site preview calls (1-acme/api, 2-acme/auth): pass backend too"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(fb.requests) != 0 {
		t.Fatal("browser ran")
	}
	out, err := call(t, tool, `{"url":"`+signInSite+`/","as":"owner","backend":" 2-acme/auth "}`)
	if err != nil {
		t.Fatal(err)
	}
	if in := fb.input(t); in.SignIn == nil || in.SignIn.URL != signInSite+"/login" || in.SignIn.Email != "qa-owner-bbbb2222@example.com" || in.SignIn.Password != adminPassword {
		t.Errorf("sign_in = %+v", in.SignIn)
	}
	var r struct {
		SignIn map[string]any `json:"sign_in"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.SignIn["as"] != "owner" || r.SignIn["backend"] != "2-acme/auth" {
		t.Errorf("sign_in = %v", r.SignIn)
	}
}

func TestVerifyRenderedAs_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name, args, want string
		readOnly, noSet  bool
	}{
		{"backend without as", `{"url":"` + signInSite + `/","backend":"1-acme/api"}`, "backend is only used with as", false, false},
		{"backend without as, no set", `{"url":"` + signInSite + `/","backend":"1-acme/api","as":" "}`, "backend is only used with as", false, true},
		{"production", `{"url":"` + signInSite + `/","as":"owner"}`, "as is not available on production checks", true, false},
		{"no set", `{"url":"` + signInSite + `/","as":"owner"}`, "as works only on a preview that lists browser sign-in roles — got d111.cloudfront.net", false, true},
		{"not a signing-in site", `{"url":"https://d222.cloudfront.net/","as":"owner"}`, "as works only on a preview that lists browser sign-in roles — got d222.cloudfront.net", false, false},
		{"unknown backend", `{"url":"` + signInSite + `/","as":"owner","backend":"9-acme/x"}`, "9-acme/x is not a backend this preview signs in to (its backends: 1-acme/api, 2-acme/auth)", false, false},
		{"unknown role", `{"url":"` + signInSite + `/","as":"admin"}`, "no test user for role admin on this preview (its roles: owner, member)", false, false},
		{"role not of the given backend", `{"url":"` + signInSite + `/","as":"member","backend":"2-acme/auth"}`, "no test user for role member on this preview (its roles: owner)", false, false},
		{"invalid role name", `{"url":"` + signInSite + `/","as":"Bad Role"}`, "no test user for role ? on this preview", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
			sites := signInSites()
			if tc.noSet {
				sites = nil
			}
			tool, _ := newSignInTool(fb, sites)
			tool.readOnly = tc.readOnly
			_, err := call(t, tool, tc.args)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if len(fb.requests) != 0 {
				t.Error("browser ran")
			}
			if tool.calls != 1 {
				t.Errorf("calls = %d, want the refusal counted", tool.calls)
			}
		})
	}
}

// A check without as is today's, in a run with a set or without.
func TestVerifyRenderedWithoutAs_Unchanged(t *testing.T) {
	const args = `{"url":"` + signInSite + `/","steps":[{"action":"click","selector":"a"}]}`
	run := func(tool *verifyPreviewRendered, fb *fakeBrowser) (string, []byte, VerifyBrowserRequest) {
		out, err := call(t, tool, args)
		if err != nil {
			t.Fatal(err)
		}
		return out, fb.requests[0].Input, fb.requests[0]
	}
	// The image answers with a sign_in nobody asked for: it is dropped.
	result := signInResult(`{"signed_in":true,"failed_requests":[],"refused_cookies":[]}`)
	fbToday := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	today, _ := newRenderedTool(renderedRecord(signInSite+"/", "https://d222.cloudfront.net/"), fbToday)
	wantOut, wantIn, wantReq := run(today, fbToday)
	for name, sites := range map[string][]BrowserSignInSite{"with a set": signInSites(), "without": nil} {
		fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: result}}
		tool, _ := newSignInTool(fb, sites)
		out, in, req := run(tool, fb)
		if out != wantOut || string(in) != string(wantIn) || req.MinImageVersion != wantReq.MinImageVersion || req.MinImageFeature != "" || req.Timeout != wantReq.Timeout {
			t.Errorf("%s: out %s\ninput %s\nwant %s\n%s", name, out, in, wantOut, wantIn)
		}
		if strings.Contains(string(in), "sign_in") {
			t.Errorf("%s: input has sign_in", name)
		}
	}
}

func TestVerifyRenderedAs_ResultWithoutSignInRefused(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult(), ImageRef: "docker.io/deploymenthq/verify-browser:1.4.0"}}
	tool, _ := newSignInTool(fb, signInSites())
	_, err := call(t, tool, `{"url":"`+signInSite+`/","as":"member"}`)
	want := "QA couldn't check the page: this runner's QA browser (docker.io/deploymenthq/verify-browser:1.4.0) ignored the sign-in; it needs verify-browser 1.5.0 or later"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestVerifyRenderedAs_ResultCarriesTheSignInCapped(t *testing.T) {
	var failed, cookies []string
	for i := 0; i < 25; i++ {
		failed = append(failed, `{"url":"https://d222.cloudfront.net/api/session","method":"POST","status":401,"failure":"`+strings.Repeat("f", 400)+`"}`)
		cookies = append(cookies, `{"host":"`+strings.Repeat("h", 300)+`","name":"sid","reason":"`+strings.Repeat("r", 100)+`"}`)
	}
	signIn := `{"signed_in":false,"reason":"still on the sign-in form 10 seconds after submitting","failed_requests":[` + strings.Join(failed, ",") + `],"refused_cookies":[` + strings.Join(cookies, ",") + `],"as":"admin","backend":"9-x/y"}`
	result := strings.Replace(string(signInResult(signIn)), `"verdict":"pass"`, `"verdict":"fail"`, 1)
	result = strings.Replace(result, `"passed":true`, `"passed":false`, 1)
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: []byte(result)}}
	tool, logs := newSignInTool(fb, signInSites())
	out, err := call(t, tool, `{"url":"`+signInSite+`/","as":"member"}`)
	if err != nil {
		t.Fatal(err)
	}
	// The reply is decoded as the agent reads it (renderedSignIn's own
	// decoding is the image's, which has no as or backend).
	type agentSignIn renderedSignIn
	var r struct {
		Message string       `json:"message"`
		SignIn  *agentSignIn `json:"sign_in"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	s := r.SignIn
	if s == nil || s.As != "member" || s.Backend != "1-acme/api" || s.SignedIn || s.Reason == nil || s.SignedOutOnLoad != nil {
		t.Fatalf("sign_in = %+v", s)
	}
	if len(s.FailedRequests) != 20 || len([]rune(s.FailedRequests[0].Failure)) != renderedDetailChars {
		t.Errorf("failed_requests: %d, failure of %d", len(s.FailedRequests), len([]rune(s.FailedRequests[0].Failure)))
	}
	if len(s.RefusedCookies) != 20 || len([]rune(s.RefusedCookies[0].Host)) != 200 || len([]rune(s.RefusedCookies[0].Reason)) != 40 {
		t.Errorf("refused_cookies: %d, host %d, reason %d", len(s.RefusedCookies), len([]rune(s.RefusedCookies[0].Host)), len([]rune(s.RefusedCookies[0].Reason)))
	}
	want := "fail — not signed in as member (still on the sign-in form 10 seconds after submitting); 0 of 1 assertions passed; no page errors; no failed requests"
	if r.Message != want {
		t.Errorf("message = %q\nwant      %q", r.Message, want)
	}
	if strings.Contains(out, `"signed_out_on_load"`) {
		t.Errorf("a failed sign-in has signed_out_on_load: %s", out)
	}
	for _, line := range []string{
		"verify_preview_rendered: not signed in as member: still on the sign-in form 10 seconds after submitting\n",
		"verify_preview_rendered: the browser refused 20 cookie(s): sid from " + strings.Repeat("h", 199) + "…",
	} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("logs lack %q:\n%s", line, logs.String())
		}
	}
	if n := strings.Count(logs.String(), " from "); n != 3 {
		t.Errorf("logs name %d cookies, want 3", n)
	}

	// Signed in: the first part is the sign-in, and signed_out_on_load is there.
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{Result: signInResult(`{"signed_in":true,"signed_out_on_load":true}`)}}
	tool, _ = newSignInTool(fb, signInSites())
	out, err = call(t, tool, `{"url":"`+signInSite+`/","as":"member"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"message":"pass — signed in as member; 1 of 1 assertions passed;`) || !strings.Contains(out, `"sign_in":{"as":"member","backend":"1-acme/api","signed_in":true,"signed_out_on_load":true,"failed_requests":[],"refused_cookies":[]}`) {
		t.Errorf("result = %s", out)
	}
}

// The shrinks that shorten failed_requests shorten the sign-in's too.
func TestRenderVerifyBrowserCheck_ShrinksSignInFailedRequests(t *testing.T) {
	var failed []string
	for i := 0; i < 20; i++ {
		failed = append(failed, `{"url":"https://d222.cloudfront.net/`+strings.Repeat("u", 1500)+`","method":"POST","status":401,"failure":"`+strings.Repeat("f", 300)+`"}`)
	}
	result := `{"sign_in":{"signed_in":true,"failed_requests":[` + strings.Join(failed, ",") + `],"refused_cookies":[]},` + strings.TrimPrefix(string(bigResult()), "{")
	text, r, err := renderVerifyBrowserCheck(VerifyBrowserOutcome{Result: []byte(result)}, renderedCheck{signIn: &browserSignInCheck{role: "member", backend: "1-acme/api"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > verifyRenderedMaxResultBytes || !r.Truncated {
		t.Fatalf("%d bytes, truncated %v", len(text), r.Truncated)
	}
	for _, f := range r.SignIn.FailedRequests {
		if len([]rune(f.URL)) > renderedShrunkURLChars {
			t.Fatalf("a sign-in failed request's url kept %d characters", len([]rune(f.URL)))
		}
	}
}

func TestVerifyRenderedSignIn_Redaction(t *testing.T) {
	// A password in a result string is redacted; the email is kept.
	result := strings.Replace(string(signInResult(`{"signed_in":true,"failed_requests":[],"refused_cookies":[]}`)), `"dom_text_snippet":"Hello"`, `"dom_text_snippet":"Hello qa-member-aaaa1111@example.com `+memberPassword+` `+runPassword+`"`, 1)
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: []byte(result)}}
	tool, _ := newSignInTool(fb, signInSites())
	out, err := call(t, tool, `{"url":"`+signInSite+`/","as":"member"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, memberPassword) || strings.Contains(out, runPassword) || !strings.Contains(out, "<redacted>") || !strings.Contains(out, "qa-member-aaaa1111@example.com") {
		t.Errorf("result = %s", out)
	}

	// In an error's output tail and the logged container output, with or without as.
	for _, args := range []string{`{"url":"` + signInSite + `/","as":"member"}`, `{"url":"` + signInSite + `/"}`} {
		fb = &fakeBrowser{outcome: VerifyBrowserOutcome{ExitCode: 1, Output: "login with qa-member-aaaa1111@example.com / " + memberPassword + " " + adminPassword}}
		tool, logs := newSignInTool(fb, signInSites())
		_, err = call(t, tool, args)
		if err == nil {
			t.Fatal("no error")
		}
		for name, text := range map[string]string{"error": err.Error(), "logs": logs.String()} {
			if strings.Contains(text, memberPassword) || strings.Contains(text, adminPassword) || !strings.Contains(text, "<redacted> <redacted>") || !strings.Contains(text, "qa-member-aaaa1111@example.com") {
				t.Errorf("%s: %s = %q", args, name, text)
			}
		}
	}

	// A run without a set redacts nothing.
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{ExitCode: 1, Output: memberPassword}}
	tool, logs := newRenderedTool(renderedRecord(signInSite+"/"), fb)
	if _, err := call(t, tool, `{"url":"`+signInSite+`/"}`); err == nil || !strings.Contains(err.Error(), memberPassword) || !strings.Contains(logs.String(), memberPassword) {
		t.Errorf("a run without a set redacted: %v", err)
	}
}

// leaksPart reports whether text carries a part of password at least 4 bytes
// long.
func leaksPart(text, password string) bool {
	for i := 0; i+4 <= len(password); i++ {
		if strings.Contains(text, password[i:i+4]) {
			return true
		}
	}
	return false
}

func TestVerifyRenderedSignIn_RedactionBeforeCuts(t *testing.T) {
	// The error's 1000-byte output tail would start inside the password.
	output := "start " + memberPassword + strings.Repeat("x", verifyRenderedOutputTailBytes-20)
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{ExitCode: 1, Output: output}}
	tool, logs := newSignInTool(fb, signInSites())
	_, err := call(t, tool, `{"url":"`+signInSite+`/"}`)
	if err == nil {
		t.Fatal("no error")
	}
	if leaksPart(err.Error(), memberPassword) || leaksPart(logs.String(), memberPassword) {
		t.Errorf("a cut output tail kept part of the password: %q", err.Error())
	}

	// The runner's own output tail cut the password's start off.
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{ExitCode: 1, Output: memberPassword[10:] + " rest", OutputCut: true}}
	tool, logs = newSignInTool(fb, signInSites())
	if _, err = call(t, tool, `{"url":"`+signInSite+`/"}`); err == nil || leaksPart(err.Error(), memberPassword) || leaksPart(logs.String(), memberPassword) || !strings.Contains(err.Error(), "<redacted> rest") {
		t.Errorf("a cut output start kept part of the password: %v", err)
	}
	// An output whose start wasn't cut keeps it.
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{ExitCode: 1, Output: memberPassword[10:] + " rest"}}
	tool, _ = newSignInTool(fb, signInSites())
	if _, err = call(t, tool, `{"url":"`+signInSite+`/"}`); err == nil || !strings.Contains(err.Error(), memberPassword[10:]+" rest") {
		t.Errorf("an uncut output was redacted: %v", err)
	}

	// The runner's cap of dom_text_snippet would cut through the password.
	snippet := strings.Repeat("y", renderedDOMTextChars-10) + memberPassword
	result := strings.Replace(string(passResult()), `"dom_text_snippet":"Hello"`, `"dom_text_snippet":"`+snippet+`"`, 1)
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{Result: []byte(result)}}
	tool, _ = newSignInTool(fb, signInSites())
	out, err := call(t, tool, `{"url":"`+signInSite+`/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if leaksPart(out, memberPassword) {
		t.Errorf("a capped snippet kept part of the password: %s", out)
	}

	// The image capped a string through a password: its start before the mark
	// is redacted; a string's last letters that merely start one are kept.
	result = strings.Replace(string(passResult()), `"dom_text_snippet":"Hello"`, `"dom_text_snippet":"Hello `+runPassword[:12]+`…","console_errors":["Loading Oth…"]`, 1)
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{Result: []byte(result)}}
	tool, _ = newSignInTool(fb, signInSites())
	if out, err = call(t, tool, `{"url":"`+signInSite+`/"}`); err != nil {
		t.Fatal(err)
	}
	if leaksPart(out, runPassword[:12]) || !strings.Contains(out, "Hello <redacted>…") || !strings.Contains(out, "Loading Oth…") {
		t.Errorf("result = %s", out)
	}

	// The image's error result embeds its script's stderr tail, cut through a
	// password and not redacted on a check without as.
	errResult, _ := json.Marshal(map[string]any{
		"schema_version": 1, "status": "error", "blocked_requests": []any{},
		"error": "the browser did not finish within timeout_ms" + stderrTailMark + memberPassword[10:] + " rest",
	})
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{ExitCode: 1, Result: errResult}}
	tool, logs = newSignInTool(fb, signInSites())
	out, err = call(t, tool, `{"url":"`+signInSite+`/"}`)
	if text := out + fmt.Sprint(err) + logs.String(); leaksPart(text, memberPassword) || !strings.Contains(text, "<redacted> rest") {
		t.Errorf("a cut stderr tail kept part of the password: %s %v", out, err)
	}

	// A check without as in a run with a set, with nothing to redact, is as
	// without a set.
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	tool, _ = newSignInTool(fb, signInSites())
	withSet, err := call(t, tool, `{"url":"`+signInSite+`/"}`)
	if err != nil {
		t.Fatal(err)
	}
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	plain, _ := newRenderedTool(renderedRecord(signInSite+"/", "https://d222.cloudfront.net/"), fb)
	if without, err := call(t, plain, `{"url":"`+signInSite+`/"}`); err != nil || without != withSet {
		t.Errorf("with a set %s, without %s (%v)", withSet, without, err)
	}
}

func TestUnescapeRedactedMark(t *testing.T) {
	for in, want := range map[string]string{
		`"a \u003credacted\u003e b"`:   `"a <redacted> b"`,
		`"a \\\u003credacted\u003e b"`: `"a \\<redacted> b"`,
		`"a \\u003credacted\u003e b"`:  `"a \\u003credacted\u003e b"`,
		`"none"`:                       `"none"`,
	} {
		if got := unescapeRedactedMark(in); got != want {
			t.Errorf("unescapeRedactedMark(%s) = %s, want %s", in, got, want)
		}
	}
}

// A backend preview that serves pages is in the set with its own URL and
// itself as its only backend: as signs in on its own host.
func TestVerifyRenderedAs_OnABackendPreview(t *testing.T) {
	const backendPreview = "https://b333.cloudfront.net"
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: signInResult(`{"signed_in":true,"failed_requests":[],"refused_cookies":[]}`)}}
	sites := []BrowserSignInSite{{URL: backendPreview, Backends: []BrowserSignInBackend{
		{Repository: "1-acme/app", SignInPath: "/users/sign_in", Users: []PreviewTestUser{{Role: "editor", Email: "qa-editor-cccc3333@example.com", Password: ownerPassword}}},
	}}}
	tool := newVerifyPreviewRenderedSignIn(renderedRecord(backendPreview+"/"), fb.run, &bytes.Buffer{}, sites, nil)
	if _, err := call(t, tool, `{"url":"`+backendPreview+`/","as":"editor"}`); err != nil {
		t.Fatal(err)
	}
	if in := fb.input(t); in.SignIn == nil || in.SignIn.URL != backendPreview+"/users/sign_in" || in.SignIn.Email != "qa-editor-cccc3333@example.com" {
		t.Errorf("sign_in = %+v", in.SignIn)
	}
	if _, err := call(t, tool, `{"url":"`+backendPreview+`/","as":"admin"}`); err == nil || err.Error() != "no test user for role admin on this preview (its roles: editor)" {
		t.Errorf("as admin: %v", err)
	}
}

func TestVerifyRendered_SchemaAndDescriptionMentionAs(t *testing.T) {
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(verifyPreviewRenderedInputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["as"] == nil || schema.Properties["backend"] == nil {
		t.Error("schema lacks as or backend")
	}
	if as, _ := schema.Properties["as"].(map[string]any); as["description"] != "Optional: check the page signed in as this role — one of the roles listed under this preview to sign in as in the browser." {
		t.Errorf("as = %v", as["description"])
	}
	for _, want := range []string{
		"pass as = one of the roles listed under that preview to sign in as in the browser, and backend too when that role is listed for more than one of its backends.",
		"you never see the email or password.",
		"When url is the site's front page (path /, no query), the check starts on the page the app shows after signing in.",
		"move between pages by clicking.",
		"The result's sign_in says how the sign-in went.",
	} {
		if !strings.Contains(verifyPreviewRenderedDescription, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}
