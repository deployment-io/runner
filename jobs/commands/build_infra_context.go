package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner-kit/enums/context_pack_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/jobs/commands/context_sources"
	// Register context sources here (blank imports run their init()).
	_ "github.com/deployment-io/deployment-runner/jobs/commands/context_sources/aws_ecs"
)

const contextPackVersion = 1

// infraBuildTimeout is a generous wall-clock bound on the whole source-build pass, passed into every
// source's Build. The heartbeat is a separate ticker that keeps beating even if a source blocks, so
// it can't reclaim a hung command; this deadline is what bounds a pathological cloud-API hang.
const infraBuildTimeout = 15 * time.Minute

// BuildInfraContext is the deterministic context-pack builder. It iterates the registered
// context sources, groups their output by scope (Org / Cluster), runs the
// runner-side redaction backstop per scope, and writes the resulting []ScopedPack to
// JobOutput. deployment-server persists one context_packs record per (org, scope), so
// org-wide content is stored once rather than duplicated per environment.
//
// Failure posture (last-good wins, error != gap): a connector error is logged and skipped,
// never folded into the pack. Only scopes rebuilt cleanly this run are emitted, so a failed
// connector leaves its scope's last-good record untouched rather than overwriting it with a
// degraded snapshot — the cron/user retry refreshes it. (Manifest gaps are blind spots a
// *successful* run determined, e.g. an `auth can-i` denial — not connector errors.)
//
// running_jobs registration + heartbeat are handled by the runner's outer execution loop, so a dead
// runner is reclaimed by the heartbeat-timeout cron like any other job. The heartbeat is a separate
// ticker, though, so it does NOT bound a hung command — the source builds run under a wall-clock
// deadline (infraBuildTimeout) so a pathological cloud-API hang can't tie up a job slot.
type BuildInfraContext struct{}

func (b *BuildInfraContext) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	io.WriteString(logsWriter, "Building infra context pack...\n")

	builtTs := time.Now().Unix()
	byScope := map[context_pack.Scope]*context_pack.Pack{}
	var order []context_pack.Scope // deterministic emit order

	// Org-wide content is keyed by the org id (its owning entity); finer scopes carry their own
	// env/cluster id. Read the org id once and normalize Org-level scopes to it, so all org
	// content groups into a single record.
	orgID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	normalize := func(s context_pack.Scope) context_pack.Scope {
		if s.Level == context_pack_enums.Org {
			s.ID = orgID
		}
		return s
	}

	ensure := func(scope context_pack.Scope) *context_pack.Pack {
		pack := byScope[scope]
		if pack == nil {
			pack = &context_pack.Pack{Manifest: context_pack.Manifest{PackVersion: contextPackVersion, BuiltTs: builtTs}}
			byScope[scope] = pack
			order = append(order, scope)
		}
		return pack
	}

	ctx, cancel := context.WithTimeout(context.Background(), infraBuildTimeout)
	defer cancel()
	sources := context_sources.All()
	io.WriteString(logsWriter, fmt.Sprintf("Running %d context source(s)...\n", len(sources)))
	for _, src := range sources {
		results, err := src.Build(ctx, parameters, logsWriter)
		if err != nil {
			// A connector error is transient plumbing failure, not knowledge about the infra:
			// log it and skip. We deliberately do NOT fold it into the pack as a gap (gaps are
			// blind spots a *successful* run determined), and we do NOT emit a degraded pack for
			// its scope — the scope is left un-rebuilt this run, so its last-good record stands
			// (deployment-server's per-scope upsert only touches scopes we emit). The cron/user
			// retry refreshes it; a persistent failure shows up in logs, not by overwriting good
			// context with a worse snapshot.
			io.WriteString(logsWriter, fmt.Sprintf("  source %s failed (skipping; last-good context retained): %v\n", src.Name(), err))
			continue
		}
		// Success path: each Result is one scope's contribution — artifacts + any gaps the connector
		// *determined* (real can-i-style blind spots, never errors) join that scope's pack. A
		// connector spanning several scopes (e.g. one Result per ECS cluster) groups into one pack
		// per scope here.
		for _, result := range results {
			pack := ensure(normalize(result.Scope))
			pack.Artifacts = append(pack.Artifacts, result.Artifacts...)
			pack.Manifest.Files = append(pack.Manifest.Files, result.Entries...)
			pack.Manifest.Gaps = append(pack.Manifest.Gaps, result.Gaps...)
		}
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
