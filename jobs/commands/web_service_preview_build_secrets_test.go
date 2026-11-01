package commands

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

func TestDockerfileSecretMounts(t *testing.T) {
	for _, tt := range []struct {
		name       string
		dockerfile string
		mounts     bool
		ids        []string
	}{
		{"id and env", "FROM node\nRUN --mount=type=secret,id=NPM_TOKEN,env=NPM_TOKEN npm ci\n", true, []string{"NPM_TOKEN"}},
		{"id before type", "RUN --mount=id=A,type=secret cat /run/secrets/A", true, []string{"A"}},
		{"target's base name, upper-case flag", "RUN --MOUNT=type=secret,target=/run/secrets/B cat /run/secrets/B", true, []string{"B"}},
		{"dst", "RUN --mount=type=secret,dst=/x/C true", true, []string{"C"}},
		{"destination", "RUN --mount=type=secret,destination=/x/D true", true, []string{"D"}},
		{"no id", "RUN --mount=type=secret,required=true true", true, nil},
		{"cache mount", "RUN --mount=type=cache,target=/root/.npm npm ci", false, nil},
		{"several in one RUN", "RUN --mount=type=cache,target=/root/.npm --mount=type=secret,id=Z --mount=type=secret,id=A,required=true --mount=type=secret,id=Z npm ci", true, []string{"A", "Z"}},
		{"none", "FROM scratch\nCOPY . .\n", false, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mounts, ids := dockerfileSecretMounts([]byte(tt.dockerfile))
			if mounts != tt.mounts || !reflect.DeepEqual(ids, tt.ids) {
				t.Errorf("got %v %v, want %v %v", mounts, ids, tt.mounts, tt.ids)
			}
		})
	}
}

// deployWebPreviewWithDockerfile deploys a preview whose Dockerfile is dockerfile
// (written by write, or as a regular file when write is nil) built like a
// Production service whose preview build secrets are secrets, and returns the
// build's parameters and the Step log. The reply also carries a Staging service
// with its own NPM_TOKEN, and Preview configuration and QA's wiring have one too:
// none of them is ever offered.
func deployWebPreviewWithDockerfile(t *testing.T, secrets map[string]string, write func(path string)) (map[string]interface{}, string) {
	t.Helper()
	buildDir, org := webPreviewBuildDir(t)
	if write != nil {
		path := filepath.Join(buildDir, "services/api/docker/Dockerfile.prod")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		write(path)
	}
	fake := &fakeWebPreviewSteps{}
	in := testWebPreviewInput(buildDir)
	in.Service.ProductionEnvironment = true
	in.Service.BuildSecrets = secrets
	staging := in.Service
	staging.DeploymentName, staging.EnvironmentName, staging.ProductionEnvironment = "api-staging", "staging", false
	staging.BuildSecrets = map[string]string{"NPM_TOKEN": "staging-preview", "CORS_ORIGINS": "staging-cors"}
	in.Settings.Services = []task_previews.WebServicePreviewSettingsV1{staging, in.Service}
	// Preview configuration's and QA's wired values (in Settings.Variables) are
	// never offered.
	in.Settings.Variables = map[string]string{"CORS_ORIGINS": "https://wired.example", "NPM_TOKEN": "wired-npm-value", "OK": "config-ok"}
	in.WiredNames = []string{"CORS_ORIGINS", "NPM_TOKEN"}
	in.Service.DockerBuildArgs = nil
	var buildLogs, logs bytes.Buffer
	in.BuildLogs, in.Logs = &buildLogs, &logs
	steps := fake.steps()
	var contextDirs []string
	steps.buildkitContext = func(dir string) error {
		contextDirs = append(contextDirs, dir)
		return nil
	}
	if _, err := testWebPreviewDeployer(org, steps).deploy(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	// A BuildKit build's directory is made readable by BuildKit; a classic one's
	// is left as it is.
	wantDirs := []string(nil)
	if _, err := jobs.GetParameterValue[map[string]string](fake.seen[0], parameters_enums.BuildSecrets); err == nil {
		wantDirs = []string{buildDir}
	}
	if !reflect.DeepEqual(contextDirs, wantDirs) {
		t.Errorf("buildkitContext called with %v, want %v", contextDirs, wantDirs)
	}
	if strings.Contains(logs.String(), "build secret") {
		t.Errorf("build secret lines reached the deploy log: %q", logs.String())
	}
	var values []string
	for _, m := range []map[string]string{in.Settings.Variables, staging.BuildSecrets, secrets} {
		for _, value := range m {
			if value != "" {
				values = append(values, value)
			}
		}
	}
	for _, value := range values {
		if strings.Contains(buildLogs.String(), value) || strings.Contains(logs.String(), value) {
			t.Errorf("a value reached a log: %q / %q", buildLogs.String(), logs.String())
		}
	}
	// Another service's preview build secrets reach nothing the build is given.
	for _, value := range staging.BuildSecrets {
		if strings.Contains(fmt.Sprint(fake.seen[0]), value) {
			t.Errorf("the build parameters carry api-staging's value %q", value)
		}
	}
	return fake.seen[0], buildLogs.String()
}

func writeDockerfile(contents string) func(string) {
	return func(path string) {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			panic(err)
		}
	}
}

const npmTokenDockerfile = "FROM node\nRUN --mount=type=secret,id=NPM_TOKEN,env=NPM_TOKEN npm ci\n"

func TestWebPreviewOffersTheChosenServicesPreviewBuildSecrets(t *testing.T) {
	previewSecrets := map[string]string{"NPM_TOKEN": "prod-preview"}
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, previewSecrets, writeDockerfile(npmTokenDockerfile))
	secrets, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if err != nil || !reflect.DeepEqual(secrets, map[string]string{"NPM_TOKEN": "prod-preview"}) {
		t.Errorf("BuildSecrets names %v (%v), want the chosen service's NPM_TOKEN", reflect.ValueOf(secrets).MapKeys(), err)
	}
	want := "Backend preview: the Dockerfile mounts build secrets (NPM_TOKEN), so the preview is built with BuildKit and offered api's preview build secrets\n"
	if !strings.Contains(buildLogs, want) {
		t.Errorf("Step log %q, want it to contain %q", buildLogs, want)
	}
	if strings.Contains(buildLogs, "no preview build secret") {
		t.Errorf("Step log %q names a missing value", buildLogs)
	}
	// A copy: the build's map isn't the service's.
	secrets["NPM_TOKEN"] = "changed"
	if previewSecrets["NPM_TOKEN"] != "prod-preview" {
		t.Error("BuildSecrets shares the service's map")
	}
}

func TestWebPreviewBuildSecretWithNoPreviewValue(t *testing.T) {
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, nil, writeDockerfile(npmTokenDockerfile))
	secrets, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if err != nil || len(secrets) != 0 {
		t.Errorf("BuildSecrets = %v (%v), want present and empty", secrets, err)
	}
	want := "Preview build: no preview build secret NPM_TOKEN; add it on api's Previews tab.\n"
	if !strings.Contains(buildLogs, want) {
		t.Errorf("Step log %q, want it to contain %q", buildLogs, want)
	}
}

func TestWebPreviewBuildSecretsWithoutIds(t *testing.T) {
	_, buildLogs := deployWebPreviewWithDockerfile(t, map[string]string{"A": "a-value"}, writeDockerfile("RUN --mount=type=secret,required=false true\n"))
	want := "Backend preview: the Dockerfile mounts build secrets, so the preview is built with BuildKit"
	if !strings.Contains(buildLogs, want) {
		t.Errorf("Step log %q, want it to contain %q", buildLogs, want)
	}
}

func TestWebPreviewBuildSecretsLeaveOutUnusableNames(t *testing.T) {
	previewSecrets := map[string]string{"A,B": "comma-value", "X=Y": "equals-value", "OK": "ok-value"}
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, previewSecrets, writeDockerfile("RUN --mount=type=secret,id=A,B --mount=type=secret,id=X=Y true\n"))
	secrets, _ := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if !reflect.DeepEqual(secrets, map[string]string{"OK": "ok-value"}) {
		t.Errorf("BuildSecrets = %v, want only OK", secrets)
	}
	// "id=A,B" is the mount id A (B is another option); "id=X=Y" is X=Y.
	for _, id := range []string{"A", "X=Y"} {
		want := "Preview build: no preview build secret " + id + ";"
		if !strings.Contains(buildLogs, want) {
			t.Errorf("Step log %q, want it to contain %q", buildLogs, want)
		}
	}
}

func TestWebPreviewWithoutSecretMountsBuildsAsBefore(t *testing.T) {
	previewSecrets := map[string]string{"NPM_TOKEN": "prod-preview"}
	for _, tt := range []struct {
		name  string
		write func(string)
	}{
		{"no secret mount", writeDockerfile("FROM node\nRUN --mount=type=cache,target=/root/.npm npm ci\n")},
		{"symlinked Dockerfile", func(path string) {
			target := filepath.Join(filepath.Dir(path), "Dockerfile.real")
			writeDockerfile(npmTokenDockerfile)(target)
			if err := os.Symlink("Dockerfile.real", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized Dockerfile", writeDockerfile(npmTokenDockerfile + strings.Repeat("#", webPreviewDockerFileMaxBytes))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parameters, buildLogs := deployWebPreviewWithDockerfile(t, previewSecrets, tt.write)
			if _, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets); err == nil {
				t.Error("BuildSecrets is set")
			}
			if strings.Contains(buildLogs, "build secret") {
				t.Errorf("Step log %q has a build secret line", buildLogs)
			}
		})
	}
}

func TestWebPreviewBuildSecretsOfferNoWiredValue(t *testing.T) {
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, map[string]string{"NPM_TOKEN": "prod-preview"}, writeDockerfile(npmTokenDockerfile+"RUN --mount=type=secret,id=CORS_ORIGINS --mount=type=secret,id=OK true\n"))
	secrets, _ := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if !reflect.DeepEqual(secrets, map[string]string{"NPM_TOKEN": "prod-preview"}) {
		t.Errorf("BuildSecrets names %v, want the chosen service's NPM_TOKEN only", reflect.ValueOf(secrets).MapKeys())
	}
	if !strings.Contains(buildLogs, "Preview build: no preview build secret CORS_ORIGINS; add it on api's Previews tab.\n") ||
		!strings.Contains(buildLogs, "Preview build: no preview build secret OK; add it on api's Previews tab.\n") {
		t.Errorf("Step log %q, want CORS_ORIGINS named as missing", buildLogs)
	}
}

// Every mounted id is named in full, however long or many, without a Step log
// line longer than its line scanner takes.
func TestWebPreviewBuildSecretLinesNameEveryID(t *testing.T) {
	long := strings.Repeat("L", 100000)
	var dockerfile strings.Builder
	dockerfile.WriteString("RUN --mount=type=secret,id=" + long + " true\n")
	var ids []string
	for i := 0; i < 50; i++ {
		id := "ID_" + strings.Repeat("x", i)
		ids = append(ids, id)
		dockerfile.WriteString("RUN --mount=type=secret,id=" + id + " true\n")
	}
	_, buildLogs := deployWebPreviewWithDockerfile(t, nil, writeDockerfile(dockerfile.String()))
	for _, line := range strings.Split(buildLogs, "\n") {
		if len(line) > buildLogMaxLine+utf8.UTFMax {
			t.Fatalf("a Step log line has %d bytes", len(line))
		}
	}
	joined := strings.ReplaceAll(buildLogs, "\n", "")
	if !strings.Contains(joined, "build secret "+long+";") {
		t.Error("the long id's missing-value line doesn't name it in full")
	}
	for _, id := range ids {
		if !strings.Contains(joined, ", "+id+",") && !strings.Contains(joined, "("+id+",") {
			t.Errorf("the mounts line doesn't name %s", id)
		}
		if !strings.Contains(joined, "build secret "+id+";") {
			t.Errorf("no missing-value line for %s", id)
		}
	}
}

// A multiline value, such as a PEM key, is offered as it is.
func TestWebPreviewBuildSecretWithALineBreakIsOffered(t *testing.T) {
	previewSecrets := map[string]string{"NPM_TOKEN": "prod-preview", "KEY": "alpha\nbeta", "CR": "gamma\rdelta"}
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, previewSecrets, writeDockerfile(npmTokenDockerfile+"RUN --mount=type=secret,id=KEY true\n"))
	secrets, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if err != nil || !reflect.DeepEqual(secrets, previewSecrets) {
		t.Errorf("BuildSecrets names %v (%v), want NPM_TOKEN, KEY and CR with their values", reflect.ValueOf(secrets).MapKeys(), err)
	}
	if strings.Contains(buildLogs, "Preview build:") {
		t.Errorf("Step log %q reports a missing secret", buildLogs)
	}
	for _, value := range []string{"alpha", "beta", "gamma", "delta", "prod-preview"} {
		if strings.Contains(buildLogs, value) {
			t.Errorf("Step log contains a value")
		}
	}
}
