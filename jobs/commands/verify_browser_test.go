package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/agenttools"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// fakeVerifyDocker stands in for the Docker daemon behind verifyBrowser's seams.
type fakeVerifyDocker struct {
	mu        sync.Mutex
	present   bool
	pullErr   error
	pulls     int
	prunes    int
	pullGate  chan struct{} // when set, a pull waits for it
	created   []*container.HostConfig
	configs   []*container.Config
	inputs    [][]byte
	removed   []string
	wallClock time.Duration
	// run is what runContainer does; it gets the /io host directory.
	run func(ioDir string) (verifyBrowserExit, error)
}

func newTestVerifyBrowser(t *testing.T, d *fakeVerifyDocker) *verifyBrowser {
	t.Helper()
	v := newVerifyBrowser()
	v.ioRoot = t.TempDir()
	v.slotWait = 50 * time.Millisecond
	v.chown = func(string) error { return nil }
	v.imagePresent = func(context.Context, string) (bool, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.present, nil
	}
	v.pullImage = func(string) error {
		d.mu.Lock()
		d.pulls++
		gate := d.pullGate
		d.mu.Unlock()
		if gate != nil {
			<-gate
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.pullErr == nil {
			d.present = true
		}
		return d.pullErr
	}
	v.pruneSuperseded = func(string) {
		d.mu.Lock()
		d.prunes++
		d.mu.Unlock()
	}
	v.createContainer = func(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig) (string, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.configs = append(d.configs, cfg)
		d.created = append(d.created, hostCfg)
		input, _ := os.ReadFile(filepath.Join(hostCfg.Mounts[0].Source, "input.json"))
		d.inputs = append(d.inputs, input)
		return "container-" + string(rune('a'+len(d.created)-1)), nil
	}
	v.runContainer = func(_ context.Context, id string, wallClock time.Duration) (verifyBrowserExit, error) {
		d.mu.Lock()
		d.wallClock = wallClock
		hostCfg := d.created[len(d.created)-1]
		run := d.run
		d.mu.Unlock()
		if run == nil {
			return verifyBrowserExit{}, nil
		}
		return run(hostCfg.Mounts[0].Source)
	}
	v.removeContainer = func(id string) {
		d.mu.Lock()
		d.removed = append(d.removed, id)
		d.mu.Unlock()
	}
	return v
}

func verifyRequest() agenttools.VerifyBrowserRequest {
	return agenttools.VerifyBrowserRequest{Input: []byte(`{"schema_version":1}`), Timeout: 45 * time.Second}
}

func writeResult(json string) func(string) (verifyBrowserExit, error) {
	return func(ioDir string) (verifyBrowserExit, error) {
		return verifyBrowserExit{output: "browser output"}, os.WriteFile(filepath.Join(ioDir, "result.json"), []byte(json), 0o644)
	}
}

func TestVerifyBrowser_PreparingThenPullErrorThenRetry(t *testing.T) {
	d := &fakeVerifyDocker{pullErr: errors.New("manifest unknown")}
	v := newTestVerifyBrowser(t, d)

	out, err := v.run(context.Background(), verifyRequest(), nil)
	if err != nil || !out.Preparing {
		t.Fatalf("first call: outcome %+v, err %v; want preparing", out, err)
	}
	<-v.pulled

	// The next call reports that failure and starts a retry, which succeeds.
	gate := make(chan struct{})
	d.mu.Lock()
	d.pullGate = gate
	d.mu.Unlock()
	_, err = v.run(context.Background(), verifyRequest(), nil)
	if err == nil || !errors.Is(err, agenttools.ErrVerifyBrowserNotRun) {
		t.Fatalf("second call: err = %v; want the pull error", err)
	}
	for _, want := range []string{verifyBrowserImage, "manifest unknown"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	d.mu.Lock()
	d.pullErr = nil
	d.mu.Unlock()
	close(gate)
	<-v.pulled
	if d.pulls != 2 {
		t.Fatalf("pulls = %d; the failed pull was not retried", d.pulls)
	}
	if d.prunes != 1 {
		t.Errorf("prunes = %d; superseded images not pruned after the pull", d.prunes)
	}
	d.run = writeResult(`{"schema_version":1,"status":"completed","verdict":"pass"}`)
	out, err = v.run(context.Background(), verifyRequest(), nil)
	if err != nil || out.Preparing || len(out.Result) == 0 {
		t.Fatalf("after the pull: %+v, %v", out, err)
	}
}

func TestVerifyBrowser_OnePullAtATime(t *testing.T) {
	d := &fakeVerifyDocker{pullGate: make(chan struct{})}
	v := newTestVerifyBrowser(t, d)
	for i := 0; i < 3; i++ {
		out, err := v.run(context.Background(), verifyRequest(), nil)
		if err != nil || !out.Preparing {
			t.Fatalf("call %d: %+v, %v", i, out, err)
		}
	}
	close(d.pullGate)
	<-v.pulled
	if d.pulls != 1 {
		t.Fatalf("pulls = %d, want 1", d.pulls)
	}
}

func TestVerifyBrowser_ContainerConfig(t *testing.T) {
	d := &fakeVerifyDocker{present: true, run: writeResult(`{"schema_version":1,"status":"completed","verdict":"pass"}`)}
	v := newTestVerifyBrowser(t, d)
	out, err := v.run(context.Background(), verifyRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Result) != `{"schema_version":1,"status":"completed","verdict":"pass"}` || out.Output != "browser output" {
		t.Errorf("outcome = %+v", out)
	}
	cfg, hc := d.configs[0], d.created[0]
	if cfg.Image != verifyBrowserImage || cfg.User != "1000:1000" || len(cfg.Env) != 0 || len(cfg.Cmd) != 0 || len(cfg.Entrypoint) != 0 {
		t.Errorf("config = %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Labels, reclaim.Labels(reclaim.PurposeVerifyBrowser)) || cfg.Labels[reclaim.PurposeLabelKey] != "verify-browser" {
		t.Errorf("labels = %v", cfg.Labels)
	}
	if len(hc.Mounts) != 1 || hc.Mounts[0].Type != mount.TypeBind || hc.Mounts[0].Target != "/io" || hc.Mounts[0].ReadOnly {
		t.Fatalf("mounts = %+v; want only the read-write /io bind", hc.Mounts)
	}
	if !strings.HasPrefix(hc.Mounts[0].Source, v.ioRoot) || string(d.inputs[0]) != `{"schema_version":1}` {
		t.Errorf("/io source %s held input %q", hc.Mounts[0].Source, d.inputs[0])
	}
	if len(hc.Binds) != 0 || len(hc.VolumesFrom) != 0 {
		t.Errorf("extra binds %v / volumes %v", hc.Binds, hc.VolumesFrom)
	}
	tmpfs := "rw,nosuid,nodev,size=256m,uid=1000,gid=1000,mode=0700"
	checks := map[string]bool{
		"CapDrop ALL":       reflect.DeepEqual([]string(hc.CapDrop), []string{"ALL"}),
		"no CapAdd":         len(hc.CapAdd) == 0,
		"no-new-privileges": reflect.DeepEqual(hc.SecurityOpt, []string{"no-new-privileges"}),
		"ReadonlyRootfs":    hc.ReadonlyRootfs,
		"not privileged":    !hc.Privileged,
		"Tmpfs":             reflect.DeepEqual(hc.Tmpfs, map[string]string{"/tmp": tmpfs, "/home/verify": tmpfs}),
		"ShmSize 256 MB":    hc.ShmSize == 256*1024*1024,
		"Memory 1 GiB":      hc.Memory == 1024*1024*1024,
		"NanoCPUs 1 core":   hc.NanoCPUs == 1_000_000_000,
		"PidsLimit 512":     hc.PidsLimit != nil && *hc.PidsLimit == 512,
		"ExtraHosts":        reflect.DeepEqual(hc.ExtraHosts, cloudMetadataExtraHosts()),
		"bridge network":    hc.NetworkMode == "bridge",
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("host config: %s not set (%+v)", name, hc)
		}
	}
	if d.wallClock != 45*time.Second+15*time.Second {
		t.Errorf("wall clock = %s, want timeout + 15s", d.wallClock)
	}
	if !reflect.DeepEqual(d.removed, []string{"container-a"}) {
		t.Errorf("removed = %v", d.removed)
	}
	if _, err := os.Stat(hc.Mounts[0].Source); !os.IsNotExist(err) {
		t.Errorf("/io directory left behind: %v", err)
	}
}

// The runner's env var (development) wins, then the Job's VerifyBrowserImage,
// then the constant.
func TestVerifyBrowser_ImageRefPrecedence(t *testing.T) {
	const jobRef = "docker.io/deploymenthq/verify-browser:0.9.0"
	t.Setenv(verifyBrowserImageEnv, "localhost:5000/verify-browser:dev")
	if got := verifyBrowserImageRef(jobRef); got != "localhost:5000/verify-browser:dev" {
		t.Fatalf("with the env var set: image = %s", got)
	}
	t.Setenv(verifyBrowserImageEnv, "")
	if got := verifyBrowserImageRef(jobRef); got != jobRef {
		t.Fatalf("with a Job image: image = %s", got)
	}
	if got := verifyBrowserImageRef(""); got != verifyBrowserImage {
		t.Fatalf("with neither: image = %s", got)
	}
}

// The ref a check asked for is the one checked, pulled and run.
func TestVerifyBrowser_RunsTheRequestedImage(t *testing.T) {
	t.Setenv(verifyBrowserImageEnv, "")
	const jobRef = "docker.io/deploymenthq/verify-browser:0.9.0"
	d := &fakeVerifyDocker{present: true, run: writeResult(`{"schema_version":1,"status":"completed","verdict":"pass"}`)}
	v := newTestVerifyBrowser(t, d)
	var asked []string
	v.imagePresent = func(_ context.Context, ref string) (bool, error) {
		asked = append(asked, ref)
		return true, nil
	}
	req := verifyRequest()
	req.ImageRef = jobRef
	out, err := v.run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.ImageRef != jobRef {
		t.Errorf("outcome image = %q, want %s", out.ImageRef, jobRef)
	}
	if len(asked) != 1 || asked[0] != jobRef {
		t.Errorf("checked %v, want %s", asked, jobRef)
	}
	if got := d.configs[0].Image; got != jobRef {
		t.Errorf("container image = %s, want %s", got, jobRef)
	}
}

// A failed pull of one ref is reported only to checks that ask for that ref;
// a check for another ref starts its own pull, and prunes after it succeeds.
func TestVerifyBrowser_PullStateIsPerRef(t *testing.T) {
	t.Setenv(verifyBrowserImageEnv, "")
	const badRef, goodRef = "docker.io/deploymenthq/verify-browser:9.9.9", "docker.io/deploymenthq/verify-browser:1.0.0"
	v := newTestVerifyBrowser(t, &fakeVerifyDocker{})
	var mu sync.Mutex
	present := map[string]bool{}
	var pulled, pruned []string
	v.imagePresent = func(_ context.Context, ref string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return present[ref], nil
	}
	v.pullImage = func(ref string) error {
		mu.Lock()
		defer mu.Unlock()
		pulled = append(pulled, ref)
		if ref == badRef {
			return errors.New("manifest unknown")
		}
		present[ref] = true
		return nil
	}
	v.pruneSuperseded = func(ref string) {
		mu.Lock()
		pruned = append(pruned, ref)
		mu.Unlock()
	}
	request := func(ref string) agenttools.VerifyBrowserRequest {
		r := verifyRequest()
		r.ImageRef = ref
		return r
	}

	if out, err := v.run(context.Background(), request(badRef), nil); err != nil || !out.Preparing {
		t.Fatalf("bad ref, first call: %+v, %v", out, err)
	}
	<-v.pulled

	// The other ref is not told about the bad ref's failure.
	if out, err := v.run(context.Background(), request(goodRef), nil); err != nil || !out.Preparing {
		t.Fatalf("good ref after the bad ref's failure: %+v, %v; want preparing, no error", out, err)
	}
	<-v.pulled
	if len(pruned) != 1 || pruned[0] != goodRef {
		t.Errorf("pruned = %v, want only after the good ref's successful pull", pruned)
	}
	if out, err := v.run(context.Background(), request(goodRef), nil); err != nil || out.Preparing {
		t.Fatalf("good ref after its pull: %+v, %v", out, err)
	}

	// The bad ref's own next check is told, and a retry starts.
	_, err := v.run(context.Background(), request(badRef), nil)
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") || !strings.Contains(err.Error(), badRef) {
		t.Fatalf("bad ref, second call: err = %v; want its own pull error", err)
	}
	<-v.pulled
	if want := []string{badRef, goodRef, badRef}; !reflect.DeepEqual(pulled, want) {
		t.Errorf("pulls = %v, want %v", pulled, want)
	}
}

// One pull at a time across refs: a check for another ref while a pull runs
// is "preparing" and starts nothing.
func TestVerifyBrowser_OnePullAtATimeAcrossRefs(t *testing.T) {
	t.Setenv(verifyBrowserImageEnv, "")
	d := &fakeVerifyDocker{pullGate: make(chan struct{})}
	v := newTestVerifyBrowser(t, d)
	for _, ref := range []string{"a/verify-browser:1", "a/verify-browser:2"} {
		r := verifyRequest()
		r.ImageRef = ref
		if out, err := v.run(context.Background(), r, nil); err != nil || !out.Preparing {
			t.Fatalf("%s: %+v, %v", ref, out, err)
		}
	}
	close(d.pullGate)
	<-v.pulled
	if d.pulls != 1 {
		t.Fatalf("pulls = %d, want 1", d.pulls)
	}
}

// buildStaticSitePreviewDeps' wrapper puts the Job's image on every check.
func TestVerifyBrowserForJobCarriesTheJobImage(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.VerifyBrowserImage, "docker.io/deploymenthq/verify-browser:0.9.0")
	var got agenttools.VerifyBrowserRequest
	run := verifyBrowserForJob(func(_ context.Context, req agenttools.VerifyBrowserRequest, _ io.Writer) (agenttools.VerifyBrowserOutcome, error) {
		got = req
		return agenttools.VerifyBrowserOutcome{}, nil
	}, parameters)
	if _, err := run(context.Background(), verifyRequest(), nil); err != nil {
		t.Fatal(err)
	}
	if got.ImageRef != "docker.io/deploymenthq/verify-browser:0.9.0" || len(got.Input) == 0 {
		t.Errorf("request = %+v, want the Job's image and the original input", got)
	}

	run = verifyBrowserForJob(func(_ context.Context, req agenttools.VerifyBrowserRequest, _ io.Writer) (agenttools.VerifyBrowserOutcome, error) {
		got = req
		return agenttools.VerifyBrowserOutcome{}, nil
	}, map[string]interface{}{})
	run(context.Background(), verifyRequest(), nil)
	if got.ImageRef != "" {
		t.Errorf("a Job with no image sent %q", got.ImageRef)
	}
}

func TestVerifyBrowser_FailuresStillRemoveTheContainer(t *testing.T) {
	cases := map[string]func(string) (verifyBrowserExit, error){
		"no result": func(string) (verifyBrowserExit, error) {
			return verifyBrowserExit{exitCode: 1, output: "crashed"}, nil
		},
		"timeout": func(string) (verifyBrowserExit, error) {
			return verifyBrowserExit{timedOut: true, output: "still loading"}, nil
		},
		"run error": func(string) (verifyBrowserExit, error) {
			return verifyBrowserExit{}, errors.New("daemon went away")
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			d := &fakeVerifyDocker{present: true, run: run}
			v := newTestVerifyBrowser(t, d)
			out, err := v.run(context.Background(), verifyRequest(), nil)
			if name == "run error" {
				if err == nil {
					t.Fatal("no error")
				}
			} else if err != nil || out.Result != nil || out.Output == "" {
				t.Fatalf("outcome %+v, err %v", out, err)
			}
			if name == "timeout" && !out.TimedOut {
				t.Error("timeout not reported")
			}
			if len(d.removed) != 1 {
				t.Errorf("container removed %d times", len(d.removed))
			}
			entries, _ := os.ReadDir(v.ioRoot)
			if len(entries) != 0 {
				t.Errorf("/io directories left behind: %d", len(entries))
			}
		})
	}
}

func TestVerifyBrowser_TwoContainersAtOnce(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	d := &fakeVerifyDocker{present: true, run: func(string) (verifyBrowserExit, error) {
		started <- struct{}{}
		<-release
		return verifyBrowserExit{}, nil
	}}
	v := newTestVerifyBrowser(t, d)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = v.run(context.Background(), verifyRequest(), nil)
		}()
	}
	<-started
	<-started

	_, err := v.run(context.Background(), verifyRequest(), nil)
	if err == nil || !errors.Is(err, agenttools.ErrVerifyBrowserNotRun) || !strings.Contains(err.Error(), "busy with other Tasks on this runner") {
		t.Fatalf("third concurrent check: err = %v", err)
	}

	// A check that finds the slots busy gets one that frees up within the wait.
	v.slotWait = 5 * time.Second
	done := make(chan error, 1)
	go func() {
		_, err := v.run(context.Background(), verifyRequest(), nil)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	release <- struct{}{}
	<-started
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("waiting check: %v", err)
	}
	wg.Wait()
	if len(d.created) != 3 {
		t.Fatalf("created %d containers, want 3", len(d.created))
	}
}

// The slot wait shrinks with the check's timeout so a whole call stays under the
// agent's tool-call timeout.
func TestVerifyBrowser_SlotWaitKeepsTheCallUnderBudget(t *testing.T) {
	v := newVerifyBrowser()
	cases := map[time.Duration]time.Duration{
		45 * time.Second:  verifyBrowserSlotWait,
		90 * time.Second:  5 * time.Second,
		100 * time.Second: 0,
	}
	for timeout, want := range cases {
		got := v.slotWaitFor(timeout)
		if got != want {
			t.Errorf("slotWaitFor(%s) = %s, want %s", timeout, got, want)
		}
		if got+timeout+verifyBrowserWallClockExtra > verifyBrowserCallBudget && got != 0 {
			t.Errorf("a %s check can take %s, over the %s budget", timeout, got+timeout+verifyBrowserWallClockExtra, verifyBrowserCallBudget)
		}
	}
}

func TestCheckPullStream(t *testing.T) {
	ok := `{"status":"Pulling from deploymenthq/verify-browser"}
{"status":"Download complete","id":"abc"}
{"status":"Status: Downloaded newer image"}
`
	if err := checkPullStream(strings.NewReader(ok)); err != nil {
		t.Fatalf("clean stream: %v", err)
	}
	failed := `{"status":"Pulling fs layer","id":"abc"}
{"errorDetail":{"message":"failed to register layer: no space left on device"},"error":"failed to register layer: no space left on device"}
`
	err := checkPullStream(strings.NewReader(failed))
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("stream with an error: got %v", err)
	}
	if err := checkPullStream(strings.NewReader(`{"status":"Pulling`)); err == nil {
		t.Fatal("truncated stream: want an error")
	}
}
