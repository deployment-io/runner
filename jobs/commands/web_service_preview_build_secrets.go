package commands

// web_service_preview_build_secrets.go: a backend preview whose Dockerfile mounts
// build secrets (RUN --mount=type=secret,…) is built with BuildKit and offered its
// Preview configuration variables as build secrets — never production's: the agent
// writes the Dockerfile, so anything a preview build is given is agent-visible.

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// webPreviewDockerFileMaxBytes bounds the Dockerfile read for secret mounts.
const webPreviewDockerFileMaxBytes = 1 << 20

// dockerMountFlag is a --mount=<options> flag; <options> runs to the next
// whitespace.
var dockerMountFlag = regexp.MustCompile(`(?i)--mount=(\S*)`)

// buildSecretName is the build-secret name rule: a name buildctl's
// --secret id=…,env=… argument can carry.
var buildSecretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// dockerfileSecretMounts reports whether the Dockerfile mounts any build secret,
// and the ids of those mounts (sorted, unique). A mount's id is its id=, or else
// the base name of its target (BuildKit's default); a mount with neither has none.
// Detection is textual: comments, quoting and line continuations aren't handled.
func dockerfileSecretMounts(dockerfile []byte) (mounts bool, ids []string) {
	seen := map[string]bool{}
	for _, m := range dockerMountFlag.FindAllSubmatch(dockerfile, -1) {
		secret := false
		var id, target string
		for _, option := range strings.Split(string(m[1]), ",") {
			key, value, _ := strings.Cut(option, "=")
			switch strings.ToLower(key) {
			case "type":
				secret = strings.EqualFold(value, "secret")
			case "id":
				id = value
			case "target", "dst", "destination":
				target = value
			}
		}
		if !secret {
			continue
		}
		mounts = true
		if id == "" && target != "" {
			if base := path.Base(target); base != "." && base != "/" {
				id = base
			}
		}
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return mounts, ids
}

// webPreviewBuildSecrets is the build secrets a preview is offered: a copy of its
// Preview configuration variables, only those whose names follow the build-secret
// name rule.
func webPreviewBuildSecrets(variables map[string]string) map[string]string {
	secrets := map[string]string{}
	for name, value := range variables {
		if buildSecretName.MatchString(name) {
			secrets[name] = value
		}
	}
	return secrets
}

// setWebPreviewBuildSecrets reads the preview's Dockerfile and, when it mounts
// build secrets, sets the BuildSecrets parameter (so BuildDockerImage builds with
// BuildKit) to the Preview configuration variables, and writes names only to
// buildLogs (when not nil). A Dockerfile that can't be read as a bounded regular
// file counts as mounting none: the preview builds as before.
func setWebPreviewBuildSecrets(parameters map[string]interface{}, dockerFilePath string, variables map[string]string, buildLogs io.Writer) {
	data, err := readBoundedRegularFile(dockerFilePath, webPreviewDockerFileMaxBytes)
	if err != nil {
		return
	}
	mounts, ids := dockerfileSecretMounts(data)
	if !mounts {
		return
	}
	secrets := webPreviewBuildSecrets(variables)
	jobs.SetParameterValue(parameters, parameters_enums.BuildSecrets, secrets)
	if buildLogs == nil {
		return
	}
	// Every id in full; buildLogWriter breaks a line longer than the Step log's
	// line scanner takes.
	logs := &buildLogWriter{w: buildLogs}
	named := ""
	if len(ids) > 0 {
		named = fmt.Sprintf(" (%s)", strings.Join(ids, ", "))
	}
	io.WriteString(logs, fmt.Sprintf("Backend preview: the Dockerfile mounts build secrets%s, so the preview is built with BuildKit and offered its Preview configuration variables as build secrets\n", named))
	for _, id := range ids {
		if _, ok := secrets[id]; !ok {
			io.WriteString(logs, fmt.Sprintf("Preview build: no preview value for build secret %s; add a read-only one in Preview configuration.\n", id))
		}
	}
}
