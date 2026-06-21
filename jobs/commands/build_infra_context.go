package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/jobs/commands/context_sources"
	// Register context sources here (blank imports run their init()), e.g.:
	// _ "github.com/deployment-io/deployment-runner/jobs/commands/context_sources/repo_catalog"
)

const contextPackVersion = 1

// BuildInfraContext is the deterministic context-pack builder. It iterates the registered
// context sources, assembles a Pack (structured-canonical artifacts + a derived markdown
// projection indexed by a manifest), runs the runner-side redaction backstop, and writes the
// result to JobOutput. deployment-server persists it into the per-org context_packs collection.
//
// It's an ordinary command: running_jobs registration + heartbeat are handled by the runner's
// outer execution loop, so the heartbeat-timeout cron covers it like any other job.
type BuildInfraContext struct{}

func (b *BuildInfraContext) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	io.WriteString(logsWriter, "Building infra context pack...\n")

	pack := context_pack.Pack{
		Manifest: context_pack.Manifest{
			PackVersion: contextPackVersion,
			BuiltTs:     time.Now().Unix(),
		},
	}

	sources := context_sources.All()
	io.WriteString(logsWriter, fmt.Sprintf("Running %d context source(s)...\n", len(sources)))
	for _, src := range sources {
		result, err := src.Build(parameters, logsWriter)
		if err != nil {
			// A source failure degrades the pack (recorded as a gap) rather than failing the
			// whole build — partial context beats none.
			pack.Manifest.Gaps = append(pack.Manifest.Gaps, fmt.Sprintf("%s: %v", src.Name(), err))
			io.WriteString(logsWriter, fmt.Sprintf("  source %s failed: %v\n", src.Name(), err))
			continue
		}
		pack.Artifacts = append(pack.Artifacts, result.Artifacts...)
		pack.Markdown = append(pack.Markdown, result.Markdown...)
		pack.Manifest.Files = append(pack.Manifest.Files, result.Entries...)
		pack.Manifest.Gaps = append(pack.Manifest.Gaps, result.Gaps...)
	}

	// Runner-side redaction invariant: nothing reaches JobOutput (or the control plane)
	// without passing through here.
	if err := context_sources.Redact(&pack); err != nil {
		return parameters, fmt.Errorf("redaction failed: %w", err)
	}

	packJSON, err := json.Marshal(pack)
	if err != nil {
		return parameters, fmt.Errorf("failed to encode context pack: %w", err)
	}
	_ = jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(packJSON))
	io.WriteString(logsWriter, fmt.Sprintf("Context pack built: %d artifact(s), %d markdown file(s), %d gap(s)\n",
		len(pack.Artifacts), len(pack.Markdown), len(pack.Manifest.Gaps)))
	return parameters, nil
}
