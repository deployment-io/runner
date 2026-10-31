package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
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
// (written by write, or as a regular file when write is nil) with variables as its
// Preview configuration, and returns the build's parameters and the Step log.
func deployWebPreviewWithDockerfile(t *testing.T, variables map[string]string, write func(path string)) (map[string]interface{}, string) {
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
	in.ConfigurationVariables = variables
	// QA's wired values (in Settings.Variables) are never offered.
	in.Settings.Variables = map[string]string{"CORS_ORIGINS": "https://wired.example"}
	for k, v := range variables {
		in.Settings.Variables[k] = v
	}
	if _, ok := variables["NPM_TOKEN"]; ok {
		in.Settings.Variables["NPM_TOKEN"] = "wired-npm-value"
	}
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
	for _, value := range in.Settings.Variables {
		if value != "" && strings.Contains(buildLogs.String(), value) {
			t.Errorf("a value reached the Step log: %q", buildLogs.String())
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

func TestWebPreviewOffersPreviewVariablesAsBuildSecrets(t *testing.T) {
	variables := map[string]string{"NPM_TOKEN": "npm-preview-value", "API_URL": "https://preview.example"}
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, variables, writeDockerfile(npmTokenDockerfile))
	secrets, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if err != nil || !reflect.DeepEqual(secrets, variables) {
		t.Errorf("BuildSecrets = %v (%v), want %v", secrets, err, variables)
	}
	want := "Backend preview: the Dockerfile mounts build secrets (NPM_TOKEN), so the preview is built with BuildKit and offered its Preview configuration variables as build secrets\n"
	if !strings.Contains(buildLogs, want) {
		t.Errorf("Step log %q, want it to contain %q", buildLogs, want)
	}
	if strings.Contains(buildLogs, "no preview value") {
		t.Errorf("Step log %q names a missing value", buildLogs)
	}
	// A copy: the build's map isn't the Preview configuration's.
	secrets["NPM_TOKEN"] = "changed"
	if variables["NPM_TOKEN"] != "npm-preview-value" {
		t.Error("BuildSecrets shares the Preview configuration's map")
	}
}

func TestWebPreviewBuildSecretWithNoPreviewValue(t *testing.T) {
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, nil, writeDockerfile(npmTokenDockerfile))
	secrets, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if err != nil || len(secrets) != 0 {
		t.Errorf("BuildSecrets = %v (%v), want present and empty", secrets, err)
	}
	want := "Preview build: no preview value for build secret NPM_TOKEN; add a read-only one in Preview configuration.\n"
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
	variables := map[string]string{"A,B": "comma-value", "X=Y": "equals-value", "OK": "ok-value"}
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, variables, writeDockerfile("RUN --mount=type=secret,id=A,B --mount=type=secret,id=X=Y true\n"))
	secrets, _ := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if !reflect.DeepEqual(secrets, map[string]string{"OK": "ok-value"}) {
		t.Errorf("BuildSecrets = %v, want only OK", secrets)
	}
	// "id=A,B" is the mount id A (B is another option); "id=X=Y" is X=Y.
	for _, id := range []string{"A", "X=Y"} {
		want := "Preview build: no preview value for build secret " + id + ";"
		if !strings.Contains(buildLogs, want) {
			t.Errorf("Step log %q, want it to contain %q", buildLogs, want)
		}
	}
}

func TestWebPreviewWithoutSecretMountsBuildsAsBefore(t *testing.T) {
	variables := map[string]string{"NPM_TOKEN": "npm-preview-value"}
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
			parameters, buildLogs := deployWebPreviewWithDockerfile(t, variables, tt.write)
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
	parameters, buildLogs := deployWebPreviewWithDockerfile(t, map[string]string{"NPM_TOKEN": "npm-preview-value"}, writeDockerfile(npmTokenDockerfile+"RUN --mount=type=secret,id=CORS_ORIGINS true\n"))
	secrets, _ := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if !reflect.DeepEqual(secrets, map[string]string{"NPM_TOKEN": "npm-preview-value"}) {
		t.Errorf("BuildSecrets = %v, want the configuration's NPM_TOKEN only", secrets)
	}
	if !strings.Contains(buildLogs, "no preview value for build secret CORS_ORIGINS;") {
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
