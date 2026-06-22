package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner-kit/enums/context_pack_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/jobs/commands/context_sources"
	// Register context sources here (blank imports run their init()), e.g.:
	// _ "github.com/deployment-io/deployment-runner/jobs/commands/context_sources/repo_catalog"
)

const contextPackVersion = 1

// BuildInfraContext is the deterministic context-pack builder. It iterates the registered
// context sources, groups their output by scope (Org / Environment / Cluster), runs the
// runner-side redaction backstop per scope, and writes the resulting []ScopedPack to
// JobOutput. deployment-server persists one context_packs record per (org, scope), so
// org-wide content is stored once rather than duplicated per environment.
//
// It's an ordinary command: running_jobs registration + heartbeat are handled by the runner's
// outer execution loop, so the heartbeat-timeout cron covers it like any other job.
type BuildInfraContext struct{}

func (b *BuildInfraContext) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	io.WriteString(logsWriter, "Building infra context pack...\n")

	builtTs := time.Now().Unix()
	byScope := map[context_pack.Scope]*context_pack.Pack{}
	var order []context_pack.Scope // deterministic emit order

	ensure := func(scope context_pack.Scope) *context_pack.Pack {
		pack := byScope[scope]
		if pack == nil {
			pack = &context_pack.Pack{Manifest: context_pack.Manifest{PackVersion: contextPackVersion, BuiltTs: builtTs}}
			byScope[scope] = pack
			order = append(order, scope)
		}
		return pack
	}

	sources := context_sources.All()
	io.WriteString(logsWriter, fmt.Sprintf("Running %d context source(s)...\n", len(sources)))
	for _, src := range sources {
		result, err := src.Build(parameters, logsWriter)
		if err != nil {
			// A source failure degrades the pack (recorded as an org-scoped gap) rather than
			// failing the whole build — partial context beats none.
			orgPack := ensure(context_pack.Scope{Level: context_pack_enums.Org})
			orgPack.Manifest.Gaps = append(orgPack.Manifest.Gaps, fmt.Sprintf("%s: %v", src.Name(), err))
			io.WriteString(logsWriter, fmt.Sprintf("  source %s failed: %v\n", src.Name(), err))
			continue
		}
		pack := ensure(result.Scope)
		pack.Artifacts = append(pack.Artifacts, result.Artifacts...)
		pack.Markdown = append(pack.Markdown, result.Markdown...)
		pack.Manifest.Files = append(pack.Manifest.Files, result.Entries...)
		pack.Manifest.Gaps = append(pack.Manifest.Gaps, result.Gaps...)
	}

	// Runner-side redaction invariant: every scope's pack passes through here before it's
	// written to JobOutput (or reaches the control plane).
	scopedPacks := make([]context_pack.ScopedPack, 0, len(order))
	for _, scope := range order {
		pack := byScope[scope]
		if err := context_sources.Redact(pack); err != nil {
			return parameters, fmt.Errorf("redaction failed: %w", err)
		}
		scopedPacks = append(scopedPacks, context_pack.ScopedPack{Scope: scope, Pack: *pack})
	}

	out, err := json.Marshal(scopedPacks)
	if err != nil {
		return parameters, fmt.Errorf("failed to encode context packs: %w", err)
	}
	_ = jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(out))
	io.WriteString(logsWriter, fmt.Sprintf("Context pack built: %d scope(s)\n", len(scopedPacks)))
	return parameters, nil
}
