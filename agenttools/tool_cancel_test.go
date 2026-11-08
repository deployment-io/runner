package agenttools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

// cancelReturnLimit is how soon after its context is cancelled every tool handler
// returns, so an agent run's teardown never waits on one.
const cancelReturnLimit = 5 * time.Second

// returnsOnCancel calls the tool name registered on srv with args, cancels its
// context once started is closed (the call is in progress), and fails unless the
// call returns within cancelReturnLimit of the cancel.
func returnsOnCancel(t *testing.T, srv *agentmcp.Server, name, args string, started <-chan struct{}) {
	t.Helper()
	tool, ok := srv.Lookup(name)
	if !ok {
		t.Fatalf("%s isn't registered", name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tool.Handler(ctx, json.RawMessage(args))
	}()
	select {
	case <-started:
	case <-done:
		t.Fatalf("%s returned before it was in progress", name)
	case <-time.After(cancelReturnLimit):
		t.Fatalf("%s never got in progress", name)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(cancelReturnLimit):
		t.Fatalf("%s didn't return within %s of its context being cancelled", name, cancelReturnLimit)
	}
}

// signalOnce returns a channel and a func that closes it the first time it's called.
func signalOnce() (chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

// blockingPreviewServer serves the preview URLs; every request signals started and
// is answered only when the client gives up on it.
func blockingPreviewServer(t *testing.T) <-chan struct{} {
	started, signal := signalOnce()
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: only then does the server notice the client going.
		_, _ = io.Copy(io.Discard, r.Body)
		signal()
		<-r.Context().Done()
	})
	return started
}

// ping never blocks, so it has no moment in progress to cancel it in: it must
// answer even when its context is cancelled before the call.
func TestPingReturnsOnCancel(t *testing.T) {
	srv := agentmcp.New("test", "0")
	agentmcp.RegisterPing(srv)
	tool, ok := srv.Lookup("ping")
	if !ok {
		t.Fatal("ping isn't registered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tool.Handler(ctx, json.RawMessage(`{}`))
	}()
	select {
	case <-done:
	case <-time.After(cancelReturnLimit):
		t.Fatalf("ping didn't return within %s with its context cancelled", cancelReturnLimit)
	}
}

func TestVerifyPreviewReachableReturnsOnCancel(t *testing.T) {
	t.Run("between polls", func(t *testing.T) {
		started, signal := signalOnce()
		probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			signal()
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		srv := agentmcp.New("test", "0")
		record, _ := recordWith([]string{"https://dnew.cloudfront.net"}, nil, nil)
		RegisterVerifyPreviewReachable(srv, record, io.Discard)
		returnsOnCancel(t, srv, "verify_preview_reachable", `{"url":"https://dnew.cloudfront.net/","max_wait_seconds":100}`, started)
	})
	t.Run("during a request", func(t *testing.T) {
		started := blockingPreviewServer(t)
		srv := agentmcp.New("test", "0")
		record, _ := recordWith([]string{"https://dnew.cloudfront.net"}, nil, nil)
		RegisterVerifyPreviewReachable(srv, record, io.Discard)
		returnsOnCancel(t, srv, "verify_preview_reachable", `{"url":"https://dnew.cloudfront.net/","max_wait_seconds":100}`, started)
	})
}

// blockingBrowser is a RunVerifyBrowser whose container runs until ctx is cancelled.
func blockingBrowser() (RunVerifyBrowser, <-chan struct{}) {
	started, signal := signalOnce()
	return func(ctx context.Context, _ VerifyBrowserRequest, _ io.Writer) (VerifyBrowserOutcome, error) {
		signal()
		<-ctx.Done()
		return VerifyBrowserOutcome{}, ctx.Err()
	}, started
}

func TestVerifyPreviewRenderedReturnsOnCancel(t *testing.T) {
	run, started := blockingBrowser()
	srv := agentmcp.New("test", "0")
	RegisterVerifyPreviewRendered(srv, renderedRecord(renderedPreview), run, io.Discard)
	returnsOnCancel(t, srv, "verify_preview_rendered", `{"url":"https://d111.cloudfront.net/"}`, started)

	run, started = blockingBrowser()
	srv = agentmcp.New("test", "0")
	RegisterVerifyPreviewRenderedReadOnly(srv, renderedRecord(renderedPreview), run, io.Discard, nil)
	returnsOnCancel(t, srv, "verify_preview_rendered", `{"url":"https://d111.cloudfront.net/"}`, started)
}

func TestVerifyAPIRequestReturnsOnCancel(t *testing.T) {
	started := blockingPreviewServer(t)
	srv := agentmcp.New("test", "0")
	record := NewPreviewRecord(nil, io.Discard)
	record.AddURL(apiTestURL)
	RegisterVerifyAPIRequest(srv, record, nil, io.Discard)
	returnsOnCancel(t, srv, "verify_api_request", `{"url":"`+apiTestURL+`/slow","timeout_seconds":60}`, started)
}

func TestVerifyAPISignInReturnsOnCancel(t *testing.T) {
	started := blockingPreviewServer(t)
	srv := agentmcp.New("test", "0")
	signIn, _ := signInTools(cookieSignIn, io.Discard)
	srv.Register(agentmcp.Tool{Name: "verify_api_sign_in", Handler: signIn.handle})
	returnsOnCancel(t, srv, "verify_api_sign_in", `{"role":"admin","url":"`+apiTestURL+`"}`, started)
}

func TestDeployStaticSitePreviewReturnsOnCancel(t *testing.T) {
	t.Run("waiting on its build", func(t *testing.T) {
		workDir, _ := newWorkDir(t)
		started, signal := signalOnce()
		deps := DeployStaticSitePreviewDeps{
			WorkDirHost:  workDir,
			LogsWriter:   io.Discard,
			Repositories: testRepositories,
			BuildSettings: func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
				return task_previews.StaticSiteBuildSettingsReplyV1{Sites: []task_previews.StaticSiteBuildSettingsV1{site("web", "", "npm run build", "dist", false)}}, nil
			},
			BuildSite: func(ctx context.Context, _, _ string, _ []string, _ io.Writer) error {
				signal()
				<-ctx.Done() // the tool's stop cancels it
				return ctx.Err()
			},
		}
		srv := agentmcp.New("test", "0")
		stop := RegisterDeployStaticSitePreview(srv, deps)
		defer stop()
		returnsOnCancel(t, srv, "deploy_static_site_preview", `{"repository":"0-acme/web"}`, started)
	})
	t.Run("looking up the settings", func(t *testing.T) {
		started, signal := signalOnce()
		release := make(chan struct{})
		defer close(release)
		deps := planDeps(task_previews.StaticSiteBuildSettingsReplyV1{}, nil)
		deps.BuildSettings = func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
			signal()
			<-release // the RPC takes no context
			return task_previews.StaticSiteBuildSettingsReplyV1{}, nil
		}
		srv := agentmcp.New("test", "0")
		stop := RegisterDeployStaticSitePreview(srv, deps)
		defer stop()
		returnsOnCancel(t, srv, "deploy_static_site_preview", `{"repository":"0-acme/web"}`, started)
	})
}

// An agent-built deploy isn't cancelled when the agent run ends: it runs to its end
// (the teardown waits for it) and the call answers with the deployed preview.
func TestDeployStaticSitePreviewAgentBuiltDeployRunsToItsEnd(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	writeTree(t, repoDir, map[string]string{"dist/index.html": "<html>"})
	started, signal := signalOnce()
	release := make(chan struct{})
	var deployCtxErr error
	saved := deployPreviewDir
	deployPreviewDir = func(ctx context.Context, _ DeployStaticSitePreviewDeps, _, _ string, _ bool) (deployStaticSitePreviewResult, error) {
		signal()
		<-release
		deployCtxErr = ctx.Err()
		return deployStaticSitePreviewResult{URL: "https://dnew.cloudfront.net", Status: "deployed"}, nil
	}
	defer func() { deployPreviewDir = saved }()
	deps := planDeps(task_previews.StaticSiteBuildSettingsReplyV1{}, nil)
	deps.WorkDirHost = workDir
	srv := agentmcp.New("test", "0")
	stop := RegisterDeployStaticSitePreview(srv, deps)
	defer stop()
	tool, _ := srv.Lookup("deploy_static_site_preview")

	ctx, cancel := context.WithCancel(context.Background())
	type answer struct {
		out string
		err error
	}
	done := make(chan answer, 1)
	go func() {
		out, err := tool.Handler(ctx, json.RawMessage(`{"repository":"0-acme/web","publish_dir":"0-acme/web/dist"}`))
		done <- answer{out, err}
	}()
	select {
	case <-started:
	case <-time.After(cancelReturnLimit):
		t.Fatal("the deploy never started")
	}
	cancel()
	select {
	case a := <-done:
		t.Fatalf("the call returned before its deploy finished: %q, %v", a.out, a.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	a := <-done
	if a.err != nil || !strings.Contains(a.out, `"status":"deployed"`) {
		t.Fatalf("got %q, %v; want the deployed preview", a.out, a.err)
	}
	if deployCtxErr != nil {
		t.Errorf("the deploy's context was cancelled with the run: %v", deployCtxErr)
	}
}

func TestDeployWebServicePreviewReturnsOnCancel(t *testing.T) {
	t.Run("waiting on its deploy", func(t *testing.T) {
		started, signal := signalOnce()
		release := make(chan struct{})
		deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
			signal()
			<-release // the deploy's steps can't be cancelled
			return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
		})
		srv := agentmcp.New("test", "0")
		stop := RegisterDeployWebServicePreview(srv, deps)
		returnsOnCancel(t, srv, "deploy_web_service_preview", `{"repository":"1-acme/api"}`, started)
		close(release)
		stop()
	})
	t.Run("looking up the settings", func(t *testing.T) {
		started, signal := signalOnce()
		release := make(chan struct{})
		defer close(release)
		deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, nil)
		deps.Deploy = func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
			return WebServicePreviewDeployOutput{}, nil
		}
		deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
			signal()
			<-release
			return task_previews.WebServicePreviewSettingsReplyV1{}, nil
		}
		srv := agentmcp.New("test", "0")
		stop := RegisterDeployWebServicePreview(srv, deps)
		defer stop()
		returnsOnCancel(t, srv, "deploy_web_service_preview", `{"repository":"1-acme/api"}`, started)
	})
}

// TestPreviewListingReturnsOnCancel: a URL whose host isn't recorded makes the
// handler list the Task's previews; a lookup that never answers doesn't hold it
// past its context being cancelled.
func TestPreviewListingReturnsOnCancel(t *testing.T) {
	blockingRecord := func() (*PreviewRecord, <-chan struct{}, func()) {
		started, signal := signalOnce()
		release := make(chan struct{})
		record := NewPreviewRecord(func() ([]string, error) {
			signal()
			<-release
			return nil, nil
		}, io.Discard)
		return record, started, func() { close(release) }
	}
	t.Run("verify_preview_reachable", func(t *testing.T) {
		record, started, release := blockingRecord()
		defer release()
		srv := agentmcp.New("test", "0")
		RegisterVerifyPreviewReachable(srv, record, io.Discard)
		returnsOnCancel(t, srv, "verify_preview_reachable", `{"url":"https://dunlisted.cloudfront.net/"}`, started)
	})
	t.Run("verify_preview_rendered", func(t *testing.T) {
		record, started, release := blockingRecord()
		defer release()
		run, _ := blockingBrowser()
		srv := agentmcp.New("test", "0")
		RegisterVerifyPreviewRendered(srv, record, run, io.Discard)
		returnsOnCancel(t, srv, "verify_preview_rendered", `{"url":"https://dunlisted.cloudfront.net/"}`, started)
	})
	t.Run("verify_api_request", func(t *testing.T) {
		record, started, release := blockingRecord()
		defer release()
		srv := agentmcp.New("test", "0")
		RegisterVerifyAPIRequest(srv, record, nil, io.Discard)
		returnsOnCancel(t, srv, "verify_api_request", `{"url":"https://dunlisted.cloudfront.net/x"}`, started)
	})
}
