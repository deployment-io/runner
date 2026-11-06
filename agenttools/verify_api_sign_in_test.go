package agenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

const (
	signInOtherURL   = "https://dother.cloudfront.net"
	signInTestCookie = "c00kie-session-value-1234"
	signInTestToken  = "tok3n-bearer-value-5678"
)

var signInTestUser = PreviewTestUser{Role: "admin", Email: "qa-admin-56789012@example.com", Password: "Pw0rdS3cretAbCdEfGhIjKlM"}

// signInTools are a run's verify_api_sign_in and verify_api_request over one
// state, with apiTestURL a preview QA may sign in at as admin with signIn, and
// signInOtherURL a preview without roles.
func signInTools(signIn task_previews.PreviewSignInV1, logs io.Writer) (*verifyAPISignIn, *verifyAPIRequest) {
	record := NewPreviewRecord(nil, io.Discard)
	record.AddURL(apiTestURL)
	record.AddURL(signInOtherURL)
	sessions := NewAPISessions([]string{signInTestUser.Password},
		[]APISignInPreview{{URL: apiTestURL, SignIn: signIn, Users: []PreviewTestUser{signInTestUser}}})
	return &verifyAPISignIn{record: record, sessions: sessions, logsWriter: logs},
		&verifyAPIRequest{record: record, sessions: sessions, logsWriter: logs}
}

var cookieSignIn = task_previews.PreviewSignInV1{Path: "/api/login", BodyFormat: "json", EmailField: "email", PasswordField: "password", Session: "cookie"}

var bearerSignIn = task_previews.PreviewSignInV1{Path: "/api/token", BodyFormat: "json", EmailField: "email", PasswordField: "password",
	Session: "bearer", TokenField: "data.access_token"}

func signInCall(t *testing.T, tool *verifyAPISignIn, args map[string]any) (verifyAPISignInResult, string) {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := tool.handle(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var res verifyAPISignInResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return res, out
}

func signInCallErr(t *testing.T, tool *verifyAPISignIn, args map[string]any) error {
	t.Helper()
	raw, _ := json.Marshal(args)
	_, err := tool.handle(context.Background(), raw)
	if err == nil {
		t.Fatalf("%v: want an error", args)
	}
	return err
}

// noSecrets fails when text has the test user's password, a session's cookie
// value or token. The email is checked when withEmail.
func noSecrets(t *testing.T, what, text string, withEmail bool) {
	t.Helper()
	secrets := []string{signInTestUser.Password, signInTestCookie, signInTestToken}
	if withEmail {
		secrets = append(secrets, signInTestUser.Email)
	}
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s has %q: %s", what, s, text)
		}
	}
}

func TestVerifyAPISignInRegistered(t *testing.T) {
	srv := agentmcp.New("test", "0")
	record := NewPreviewRecord(nil, nil)
	sessions := NewAPISessions(nil, nil)
	RegisterVerifyAPIRequest(srv, record, sessions, nil)
	RegisterVerifyAPISignIn(srv, record, sessions, nil)
	if got := strings.Join(srv.ToolNames(), ","); got != "verify_api_request,verify_api_sign_in" {
		t.Errorf("tools = %s", got)
	}
	if !json.Valid([]byte(verifyAPISignInInputSchema)) || !json.Valid([]byte(verifyAPIRequestInputSchema)) {
		t.Error("an input schema isn't JSON")
	}
}

// The request the server receives is the setting's: a POST to its path with the
// real email and password at its fields.
func TestVerifyAPISignInSendsTheSettingsRequest(t *testing.T) {
	type seen struct{ method, path, contentType, body string }
	var got seen
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b)}
		w.WriteHeader(http.StatusUnauthorized)
	})
	for _, tc := range []struct {
		name            string
		format          string
		email, password string
		check           func(t *testing.T, body string)
	}{
		{"json, nested", "json", "user.email", "user.password", func(t *testing.T, body string) {
			var v struct {
				User struct{ Email, Password string } `json:"user"`
			}
			if err := json.Unmarshal([]byte(body), &v); err != nil || v.User.Email != signInTestUser.Email || v.User.Password != signInTestUser.Password {
				t.Errorf("body = %s", body)
			}
		}},
		{"json, an empty segment", "json", "user.", "password", func(t *testing.T, body string) {
			var v map[string]string
			if err := json.Unmarshal([]byte(body), &v); err != nil || len(v) != 2 || v["user."] != signInTestUser.Email || v["password"] != signInTestUser.Password {
				t.Errorf("body = %s", body)
			}
		}},
		{"form", "form", "user[email]", "user[password]", func(t *testing.T, body string) {
			v, err := url.ParseQuery(body)
			if err != nil || len(v) != 2 || v.Get("user[email]") != signInTestUser.Email || v.Get("user[password]") != signInTestUser.Password {
				t.Errorf("body = %s", body)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signIn := task_previews.PreviewSignInV1{Path: "/auth/sign_in", BodyFormat: tc.format, EmailField: tc.email, PasswordField: tc.password, Session: "cookie"}
			tool, _ := signInTools(signIn, nil)
			signInCall(t, tool, map[string]any{"role": "admin", "url": apiTestURL})
			wantType := map[string]string{"json": "application/json", "form": "application/x-www-form-urlencoded"}[tc.format]
			if got.method != "POST" || got.path != "/auth/sign_in" || got.contentType != wantType {
				t.Errorf("server saw %+v", got)
			}
			tc.check(t, got.body)
		})
	}
}

// A cookie session: the Set-Cookies of the answer, sent by verify_api_request
// with as; the result and the log carry the cookie names only.
func TestVerifyAPISignInCookieSession(t *testing.T) {
	var cookie string
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			w.Header().Add("Set-Cookie", "session="+signInTestCookie+"; Path=/; HttpOnly")
			w.Header().Add("Set-Cookie", "lang=en")
			w.Header().Add("Set-Cookie", "empty=")
			io.WriteString(w, `{"ok":true}`)
			return
		}
		cookie = r.Header.Get("Cookie")
	})
	var logs bytes.Buffer
	signIn, request := signInTools(cookieSignIn, &logs)
	res, out := signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL + "/"})
	if !res.SignedIn || res.Role != "admin" || res.Status != 200 || res.Session != "cookie" || strings.Join(res.Cookies, ",") != "session,lang" || res.Body != nil {
		t.Errorf("result = %s", out)
	}
	noSecrets(t, "the result", out, true)
	if want := "verify_api_sign_in: admin at https://dapi.cloudfront.net/api/login → signed in (cookie)\n"; logs.String() != want {
		t.Errorf("log = %q, want %q", logs.String(), want)
	}
	apiCall(t, request, map[string]any{"url": apiTestURL + "/me", "as": "admin"})
	if cookie != "session="+signInTestCookie+"; lang=en" {
		t.Errorf("Cookie = %q", cookie)
	}
	// Without as, no session.
	apiCall(t, request, map[string]any{"url": apiTestURL + "/me"})
	if cookie != "" {
		t.Errorf("without as, Cookie = %q", cookie)
	}
	if !strings.HasSuffix(strings.Split(logs.String(), "\n")[1], " ms") || !strings.Contains(logs.String(), "GET https://dapi.cloudfront.net/me as admin → 200 in ") {
		t.Errorf("log = %q", logs.String())
	}
	noSecrets(t, "the log", logs.String(), true)
}

// A bearer session: the token at the nested token field, sent as
// Authorization: Bearer.
func TestVerifyAPISignInBearerSession(t *testing.T) {
	var auth string
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/token" {
			fmt.Fprintf(w, `{"data":{"access_token":%q,"user":{"email":%q}}}`, signInTestToken, signInTestUser.Email)
			return
		}
		auth = r.Header.Get("Authorization")
		fmt.Fprintf(w, `{"token":%q}`, signInTestToken)
	})
	var logs bytes.Buffer
	signIn, request := signInTools(bearerSignIn, &logs)
	res, out := signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL})
	if !res.SignedIn || res.Session != "bearer" || res.Cookies != nil {
		t.Errorf("result = %s", out)
	}
	noSecrets(t, "the result", out, true)
	got, raw := apiCall(t, request, map[string]any{"url": apiTestURL + "/me", "as": "admin"})
	if auth != "Bearer "+signInTestToken {
		t.Errorf("Authorization = %q", auth)
	}
	if got.Body != `{"token":"<redacted>"}` {
		t.Errorf("body = %q", got.Body)
	}
	noSecrets(t, "the result", raw, false)
	if !strings.HasPrefix(logs.String(), "verify_api_sign_in: admin at https://dapi.cloudfront.net/api/token → signed in (bearer)\n") {
		t.Errorf("log = %q", logs.String())
	}
	noSecrets(t, "the log", logs.String(), true)
}

// A sign-in that doesn't give a session says what was missing, with the start of
// the body, redacted; an earlier session is kept.
func TestVerifyAPISignInFailures(t *testing.T) {
	var answer func(w http.ResponseWriter)
	var cookie string
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			answer(w)
			return
		}
		cookie = r.Header.Get("Cookie")
	})
	for _, tc := range []struct {
		name    string
		signIn  task_previews.PreviewSignInV1
		answer  func(w http.ResponseWriter)
		reason  string
		status  int
		logLine string
	}{
		{"401", cookieSignIn, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":"wrong password %s for %s"}`, signInTestUser.Password, signInTestUser.Email)
		}, "status 401", 401, "not signed in (status 401)"},
		{"2xx without Set-Cookie", cookieSignIn, func(w http.ResponseWriter) { io.WriteString(w, "welcome") },
			"no Set-Cookie in the response", 200, "not signed in (status 200)"},
		{"the token field missing", bearerSignIn, func(w http.ResponseWriter) { io.WriteString(w, `{"data":{}}`) },
			"the token field data.access_token is not a non-empty string", 200, "not signed in (status 200)"},
		{"the token field not a string", bearerSignIn, func(w http.ResponseWriter) { io.WriteString(w, `{"data":{"access_token":42}}`) },
			"the token field data.access_token is not a non-empty string", 200, "not signed in (status 200)"},
		{"not JSON", bearerSignIn, func(w http.ResponseWriter) { io.WriteString(w, `<html>`) },
			"the token field data.access_token is not a non-empty string", 200, "not signed in (status 200)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			signIn, request := signInTools(tc.signIn, &logs)
			// An earlier cookie session, kept after the failure.
			signIn.sessions.keep("dapi.cloudfront.net", "admin", &apiSession{kind: apiSessionCookie, cookies: []apiSessionCookieValue{{name: "session", value: signInTestCookie}}})
			answer = tc.answer
			res, out := signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL})
			if res.SignedIn || res.Status != tc.status || res.Reason != tc.reason || res.Body == nil || res.Session != "" {
				t.Errorf("result = %s", out)
			}
			noSecrets(t, "the result", out, false)
			if tc.status == 401 && !strings.Contains(*res.Body, "wrong password <redacted> for "+signInTestUser.Email) {
				t.Errorf("body = %q", *res.Body)
			}
			if want := "verify_api_sign_in: admin at https://dapi.cloudfront.net" + tc.signIn.Path + " → " + tc.logLine + "\n"; logs.String() != want {
				t.Errorf("log = %q, want %q", logs.String(), want)
			}
			apiCall(t, request, map[string]any{"url": apiTestURL + "/me", "as": "admin"})
			if cookie != "session="+signInTestCookie {
				t.Errorf("the earlier session wasn't kept: Cookie = %q", cookie)
			}
		})
	}
}

// A failed sign-in's body is at most 2 KiB.
func TestVerifyAPISignInBodyCut(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, strings.Repeat("x", 5000))
	})
	tool, _ := signInTools(cookieSignIn, nil)
	res, _ := signInCall(t, tool, map[string]any{"role": "admin", "url": apiTestURL})
	if res.Body == nil || len(*res.Body) != verifyAPISignInMaxBodyBytes {
		t.Errorf("body = %v", res.Body)
	}
}

func TestVerifyAPISignInRefusals(t *testing.T) {
	probeServer(t, func(http.ResponseWriter, *http.Request) { t.Error("a refused sign-in was sent") })
	var logs bytes.Buffer
	tool, _ := signInTools(cookieSignIn, &logs)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"role": "editor", "url": apiTestURL}, "no test user for role editor on this preview"},
		{map[string]any{"role": "admin", "url": signInOtherURL}, "no test user for role admin on this preview"},
		{map[string]any{"role": "admin", "url": "https://evil.example.com"}, "url must be a backend preview URL this Task deployed"},
		{map[string]any{"role": "admin", "url": "http://dapi.cloudfront.net"}, "url must be a backend preview URL this Task deployed"},
		{map[string]any{"role": "admin", "url": apiTestURL + "/api/login"}, "without a path"},
		{map[string]any{"role": "admin", "url": apiTestURL + "/?x=1"}, "without a path"},
		{map[string]any{"url": apiTestURL}, "role is required"},
		{map[string]any{"role": "admin"}, "url is required"},
	} {
		if err := signInCallErr(t, tool, tc.args); !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	noSecrets(t, "the log", logs.String(), true)
	if !strings.Contains(logs.String(), "verify_api_sign_in: editor at https://dapi.cloudfront.net → refused (no test user for the role)\n") {
		t.Errorf("log = %q", logs.String())
	}
}

// Every call counts toward the 10-call cap, errors included.
func TestVerifyAPISignInCallCap(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	tool, _ := signInTools(cookieSignIn, nil)
	for i := 0; i < verifyAPISignInMaxCalls; i++ {
		args := map[string]any{"role": "admin", "url": apiTestURL}
		if i%2 == 1 {
			args["role"] = "nobody"
		}
		raw, _ := json.Marshal(args)
		tool.handle(context.Background(), raw)
	}
	err := signInCallErr(t, tool, map[string]any{"role": "admin", "url": apiTestURL})
	if !strings.Contains(err.Error(), "already been called 10 times") {
		t.Errorf("err = %v", err)
	}
}

// With as, the url's host must have the role's session, and the caller can't
// pass Cookie or Authorization.
func TestVerifyAPIRequestAsRefusals(t *testing.T) {
	sent := 0
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			w.Header().Add("Set-Cookie", "session="+signInTestCookie)
			return
		}
		sent++
	})
	var logs bytes.Buffer
	signIn, request := signInTools(cookieSignIn, &logs)
	if err := apiCallErr(t, request, map[string]any{"url": apiTestURL + "/me", "as": "admin"}); err.Error() != "not signed in as admin on this preview — call verify_api_sign_in first" {
		t.Errorf("err = %v", err)
	}
	signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL})
	if err := apiCallErr(t, request, map[string]any{"url": signInOtherURL + "/me", "as": "admin"}); !strings.Contains(err.Error(), "not signed in as admin on this preview") {
		t.Errorf("another host: %v", err)
	}
	if err := apiCallErr(t, request, map[string]any{"url": apiTestURL + "/me", "as": "viewer"}); !strings.Contains(err.Error(), "not signed in as viewer") {
		t.Errorf("another role: %v", err)
	}
	for _, name := range []string{"Cookie", "authorization"} {
		err := apiCallErr(t, request, map[string]any{"url": apiTestURL + "/me", "as": "admin", "headers": map[string]string{name: "x"}})
		if !strings.Contains(err.Error(), "is not allowed with as") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if sent != 0 {
		t.Errorf("%d refused requests were sent", sent)
	}
	// Without as, both headers go as given.
	apiCall(t, request, map[string]any{"url": apiTestURL + "/me", "headers": map[string]string{"Cookie": "a=b", "Authorization": "Basic x"}})
	if sent != 1 {
		t.Errorf("sent = %d", sent)
	}
	noSecrets(t, "the log", logs.String(), true)
}

// Each Set-Cookie on an as response updates the session: a new or changed value
// replaces the cookie's; an empty value, Max-Age <= 0 or a past Expires removes it.
func TestVerifyAPIRequestAsUpdatesCookies(t *testing.T) {
	var cookies []string
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		switch r.URL.Path {
		case "/api/login":
			for _, c := range []string{"session=" + signInTestCookie, "a=1", "b=2", "c=3", "d=4"} {
				h.Add("Set-Cookie", c)
			}
		case "/rotate":
			for _, c := range []string{"session=rotated-value-9999; Path=/", "a=", "b=2; Max-Age=0", "c=3; Expires=Thu, 01 Jan 1970 00:00:00 GMT", "e=5"} {
				h.Add("Set-Cookie", c)
			}
		}
		cookies = append(cookies, r.Header.Get("Cookie"))
	})
	signIn, request := signInTools(cookieSignIn, nil)
	signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL})
	res, _ := apiCall(t, request, map[string]any{"url": apiTestURL + "/rotate", "as": "admin"})
	apiCall(t, request, map[string]any{"url": apiTestURL + "/me", "as": "admin"})
	// Without as, the answer's Set-Cookies change nothing.
	apiCall(t, request, map[string]any{"url": apiTestURL + "/api/login"})
	apiCall(t, request, map[string]any{"url": apiTestURL + "/me", "as": "admin"})
	want := "session=rotated-value-9999; d=4; e=5"
	if len(cookies) != 5 || cookies[2] != want || cookies[4] != want {
		t.Errorf("Cookie headers = %q", cookies)
	}
	if got := res.Headers["Set-Cookie"]; got != "session=<redacted>; Path=/, a=<redacted>, b=<redacted>; Max-Age=0, c=<redacted>; Expires=Thu, 01 Jan 1970 00:00:00 GMT, e=<redacted>" {
		t.Errorf("Set-Cookie = %q", got)
	}
}

// Every result is redacted of the passwords, the tokens and the session cookie
// values of 8 characters or more — also where a limit cuts a secret — and every
// Set-Cookie value; emails stay; body_bytes is the response's own size.
func TestVerifyAPIRequestRedaction(t *testing.T) {
	const shortCookie = "abc12"
	bodyCut := strings.Repeat("-", verifyAPIRequestMaxResultBodyBytes-5) + signInTestUser.Password
	headerCut := strings.Repeat("-", verifyAPIRequestMaxHeaderValueChars-5) + signInTestCookie
	echo := fmt.Sprintf("pw=%s token=%s cookie=%s short=%s email=%s", signInTestUser.Password, signInTestToken, signInTestCookie, shortCookie, signInTestUser.Email)
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		switch r.URL.Path {
		case "/api/login":
			h.Add("Set-Cookie", "session="+signInTestCookie)
			h.Add("Set-Cookie", "short="+shortCookie)
		case "/echo":
			h.Set("X-Echo", echo)
			h.Add("Set-Cookie", "one=first-value; Path=/")
			h.Add("Set-Cookie", "two=second-value")
			io.WriteString(w, echo)
		case "/cut":
			h.Set("X-Cut", headerCut)
			io.WriteString(w, bodyCut)
		}
	})
	signIn, request := signInTools(cookieSignIn, nil)
	signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL})
	// A token from another sign-in of the run.
	signIn.sessions.keep("dother.cloudfront.net", "viewer", &apiSession{kind: apiSessionBearer, token: signInTestToken})

	res, raw := apiCall(t, request, map[string]any{"url": apiTestURL + "/echo"})
	want := "pw=<redacted> token=<redacted> cookie=<redacted> short=" + shortCookie + " email=" + signInTestUser.Email
	if res.Body != want || res.Headers["X-Echo"] != want {
		t.Errorf("body = %q, X-Echo = %q, want %q", res.Body, res.Headers["X-Echo"], want)
	}
	if res.BodyBytes != len(echo) {
		t.Errorf("body_bytes = %d, want %d", res.BodyBytes, len(echo))
	}
	if got := res.Headers["Set-Cookie"]; got != "one=<redacted>; Path=/, two=<redacted>" {
		t.Errorf("Set-Cookie = %q", got)
	}
	noSecrets(t, "the result", raw, false)

	res, raw = apiCall(t, request, map[string]any{"url": apiTestURL + "/cut"})
	if strings.Contains(res.Body, signInTestUser.Password[:5]) || strings.Contains(res.Headers["X-Cut"], signInTestCookie[:5]) {
		t.Errorf("a cut secret shows: body ends %q, X-Cut ends %q", res.Body[len(res.Body)-20:], res.Headers["X-Cut"][len(res.Headers["X-Cut"])-20:])
	}
	if res.BodyBytes != len(bodyCut) || !res.BodyTruncated {
		t.Errorf("body_bytes = %d, truncated %v", res.BodyBytes, res.BodyTruncated)
	}
	noSecrets(t, "the result", raw, false)
}

// Without a sign-in state, verify_api_request still redacts every Set-Cookie
// value, and nothing else.
func TestVerifyAPIRequestRedactsSetCookieWithoutSessions(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "sid=abcdefghijklmnop; HttpOnly")
		io.WriteString(w, "abcdefghijklmnop")
	})
	res, _ := apiCall(t, apiTestTool(nil), map[string]any{"url": apiTestURL + "/"})
	if res.Headers["Set-Cookie"] != "sid=<redacted>; HttpOnly" || res.Body != "abcdefghijklmnop" {
		t.Errorf("Set-Cookie = %q, body = %q", res.Headers["Set-Cookie"], res.Body)
	}
}

func TestSetJSONField(t *testing.T) {
	for _, tc := range []struct{ email, password, want string }{
		{"email", "password", `{"email":"e","password":"p"}`},
		{"user.email", "user.password", `{"user":{"email":"e","password":"p"}}`},
		{"a.b.c", "pw", `{"a":{"b":{"c":"e"}},"pw":"p"}`},
		{"user.", "password", `{"password":"p","user.":"e"}`},
		{"a..b", ".pw", `{".pw":"p","a..b":"e"}`},
	} {
		object := map[string]any{}
		setJSONField(object, tc.email, "e")
		setJSONField(object, tc.password, "p")
		if got, _ := json.Marshal(object); string(got) != tc.want {
			t.Errorf("%s, %s: %s, want %s", tc.email, tc.password, got, tc.want)
		}
	}
}

// A secret the 1 MiB read limit cuts doesn't show its start, even when the
// redacted body is short enough to be returned whole.
func TestVerifyAPIRequestRedactsASecretCutAtTheReadLimit(t *testing.T) {
	token := strings.Repeat("T", 2048)
	copies := strings.Repeat(token, verifyAPIRequestReadLimitBytes/len(token)-1)
	cut := len(signInTestUser.Password) - 1
	body := copies + strings.Repeat("-", verifyAPIRequestReadLimitBytes-len(copies)-cut) + signInTestUser.Password
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	})
	signIn, request := signInTools(cookieSignIn, nil)
	signIn.sessions.keep("dother.cloudfront.net", "viewer", &apiSession{kind: apiSessionBearer, token: token})
	res, raw := apiCall(t, request, map[string]any{"url": apiTestURL + "/"})
	if !res.BodyBytesCapped || res.BodyTruncated || !strings.HasSuffix(res.Body, "-"+apiRedacted) {
		t.Errorf("capped %v, truncated %v, body ends %q", res.BodyBytesCapped, res.BodyTruncated, res.Body[len(res.Body)-40:])
	}
	if strings.Contains(raw, signInTestUser.Password[:cut]) {
		t.Errorf("the cut password shows: %s", raw[len(raw)-200:])
	}
}

// The read limit's cut is redacted from the body as sent: a session cookie
// equal to the password's first 8 characters doesn't hide where the cut
// password starts.
func TestVerifyAPIRequestRedactsASecretCutAtTheReadLimitBehindAShorterSecret(t *testing.T) {
	token := strings.Repeat("T", 2048)
	copies := strings.Repeat(token, verifyAPIRequestReadLimitBytes/len(token)-1)
	cut := len(signInTestUser.Password) - 1
	body := copies + strings.Repeat("-", verifyAPIRequestReadLimitBytes-len(copies)-cut) + signInTestUser.Password
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	})
	signIn, request := signInTools(cookieSignIn, nil)
	signIn.sessions.keep("dother.cloudfront.net", "viewer", &apiSession{kind: apiSessionBearer, token: token})
	signIn.sessions.keep("dother.cloudfront.net", "member", &apiSession{kind: apiSessionCookie, cookies: []apiSessionCookieValue{{name: "s", value: signInTestUser.Password[:8]}}})
	res, raw := apiCall(t, request, map[string]any{"url": apiTestURL + "/"})
	if !res.BodyBytesCapped || res.BodyTruncated || !strings.HasSuffix(res.Body, "-"+apiRedacted) {
		t.Errorf("capped %v, truncated %v, body ends %q", res.BodyBytesCapped, res.BodyTruncated, res.Body[len(res.Body)-40:])
	}
	if strings.Contains(raw, signInTestUser.Password[8:cut]) {
		t.Errorf("the cut password shows: %s", raw[len(raw)-200:])
	}
}

// A response header named after a secret doesn't show it, whatever case Go's
// HTTP parser gives the name.
func TestVerifyAPIRequestRedactsHeaderNames(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(signInTestToken, "1")
		w.Header().Set("X-"+signInTestUser.Password, "2")
		w.Header().Set("X-Plain", "3")
	})
	signIn, request := signInTools(cookieSignIn, nil)
	signIn.sessions.keep("dother.cloudfront.net", "viewer", &apiSession{kind: apiSessionBearer, token: signInTestToken})
	res, raw := apiCall(t, request, map[string]any{"url": apiTestURL + "/"})
	if res.Headers[apiRedacted] != "1" || res.Headers["X-"+apiRedacted] != "2" || res.Headers["X-Plain"] != "3" {
		t.Errorf("headers = %v", res.Headers)
	}
	for _, secret := range []string{signInTestToken, signInTestUser.Password} {
		if strings.Contains(strings.ToLower(raw), strings.ToLower(secret)) {
			t.Errorf("%q shows in any case: %s", secret, raw)
		}
	}
}

// A cookie named after the password doesn't show it in the sign-in's result.
func TestVerifyAPISignInRedactsCookieNames(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", signInTestUser.Password+"=x")
		w.Header().Add("Set-Cookie", "session="+signInTestCookie)
	})
	signIn, _ := signInTools(cookieSignIn, nil)
	res, out := signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL})
	if !res.SignedIn || strings.Join(res.Cookies, ",") != apiRedacted+",session" {
		t.Errorf("result = %s", out)
	}
	noSecrets(t, "the result", out, true)
}

// A quoted cookie value is sent back quoted.
func TestVerifyAPIRequestAsSendsQuotedCookiesQuoted(t *testing.T) {
	var cookie string
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			w.Header().Add("Set-Cookie", `session="abc defghijk"; Path=/`)
			w.Header().Add("Set-Cookie", "lang=en")
			return
		}
		cookie = r.Header.Get("Cookie")
	})
	signIn, request := signInTools(cookieSignIn, nil)
	if res, out := signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL}); !res.SignedIn {
		t.Fatalf("result = %s", out)
	}
	apiCall(t, request, map[string]any{"url": apiTestURL + "/me", "as": "admin"})
	if want := `session="abc defghijk"; lang=en`; cookie != want {
		t.Errorf("Cookie = %q, want %q", cookie, want)
	}
}

// Overlapping secrets are redacted as one: a session cookie that runs into the
// password's start doesn't leave the password's end showing, and a secret that
// overlaps itself is redacted whole.
func TestAPIRedactorMergesOverlappingSecrets(t *testing.T) {
	password := signInTestUser.Password
	cookie := "prefix-" + password[:8]
	r := newAPIRedactor([]string{password, cookie, "abab"})
	for in, want := range map[string]string{
		"x prefix-" + password + " y": "x " + apiRedacted + " y",
		password + " and " + password: apiRedacted + " and " + apiRedacted,
		cookie + password:             apiRedacted,
		"[ababab]":                    "[" + apiRedacted + "]",
		"no secret here":              "no secret here",
	} {
		if got := r.text(in); got != want {
			t.Errorf("text(%q) = %q, want %q", in, got, want)
		}
		if got := string(r.bytes([]byte(in))); got != want {
			t.Errorf("bytes(%q) = %q, want %q", in, got, want)
		}
	}
	if got := r.name("X-" + strings.ToUpper(password) + "-Id"); got != "X-"+apiRedacted+"-Id" {
		t.Errorf("name = %q", got)
	}
	// The cut: the start of a secret at the end is redacted; without a cut it stays.
	if got := string(r.cutBytes([]byte("ok " + password[:5]))); got != "ok "+apiRedacted {
		t.Errorf("cutBytes = %q", got)
	}
	if got := r.text("ok " + password[:5]); got != "ok "+password[:5] {
		t.Errorf("text without a cut = %q", got)
	}
}

// The same response through the tool: a cookie value overlapping the password
// doesn't expose the password's end.
func TestVerifyAPIRequestRedactsOverlappingSecrets(t *testing.T) {
	cookie := "prefix-" + signInTestUser.Password[:8]
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "prefix-"+signInTestUser.Password)
	})
	signIn, request := signInTools(cookieSignIn, nil)
	signIn.sessions.keep("dother.cloudfront.net", "member", &apiSession{kind: apiSessionCookie, cookies: []apiSessionCookieValue{{name: "s", value: cookie}}})
	res, raw := apiCall(t, request, map[string]any{"url": apiTestURL + "/"})
	if res.Body != apiRedacted {
		t.Errorf("body = %q", res.Body)
	}
	if strings.Contains(raw, signInTestUser.Password[8:]) {
		t.Errorf("the password's end shows: %s", raw)
	}
}

// A session token equal to "Set-Cookie" redacts that header's name, but its
// values are still redacted as Set-Cookie values.
func TestVerifyAPIRequestRedactsSetCookieWhenItsNameIsASecret(t *testing.T) {
	const other = "another-cookie-secret"
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "sid="+other+"; Path=/")
	})
	signIn, request := signInTools(cookieSignIn, nil)
	signIn.sessions.keep("dother.cloudfront.net", "viewer", &apiSession{kind: apiSessionBearer, token: "Set-Cookie"})
	res, raw := apiCall(t, request, map[string]any{"url": apiTestURL + "/"})
	if strings.Contains(raw, other) {
		t.Errorf("the cookie's value shows: %s", raw)
	}
	if got := res.Headers[apiRedacted]; got != "sid="+apiRedacted+"; Path=/" {
		t.Errorf("headers = %v", res.Headers)
	}
}

// A secret that repeats, in a long response made of it, is redacted in linear
// time: this would take minutes with a search that restarts at every byte.
func TestAPIRedactorIsLinearOnARepeatingSecret(t *testing.T) {
	secret := strings.Repeat("T", 64*1024)
	body := []byte(strings.Repeat("T", verifyAPIRequestReadLimitBytes))
	r := newAPIRedactor([]string{secret})
	if got := string(r.cutBytes(body)); got != apiRedacted {
		t.Errorf("got %d bytes, want just %q", len(got), apiRedacted)
	}
}

// Sign-ins while "as" requests rotate the same role's cookies: run with -race.
func TestVerifyAPISignInWhileCookiesRotate(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "session="+signInTestCookie)
		w.Header().Add("Set-Cookie", fmt.Sprintf("rot=%s-rotating-value", r.URL.Path))
	})
	signIn, _ := signInTools(cookieSignIn, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			signIn.sessions.updateCookies("dapi.cloudfront.net", "admin",
				[]*http.Cookie{{Name: fmt.Sprintf("c%d", i%5), Value: "rotated-value-1234"}}, time.Now())
		}
	}()
	for i := 0; i < 5; i++ {
		if res, _ := signInCall(t, signIn, map[string]any{"role": "admin", "url": apiTestURL}); !res.SignedIn {
			t.Fatalf("not signed in: %+v", res)
		}
	}
	<-done
}
