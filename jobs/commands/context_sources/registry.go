// Package context_sources defines the ContextSource registry. Each connector implements
// Source and registers itself (from its package init); the BuildInfraContext command iterates
// the registered sources in a fixed order to assemble a context pack.
//
// Sources emit metadata/structure only — never secret values. Redaction is enforced
// runner-side (see Redact) before the pack is written to JobOutput and persisted.
package context_sources

import (
	"io"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
)

// Result is one source's contribution to a context pack.
type Result struct {
	Artifacts []context_pack.Artifact     // structured-canonical (queryable once persisted)
	Markdown  []context_pack.MarkdownFile // derived projection the agent reads
	Entries   []context_pack.ManifestEntry
	Gaps      []string // what the source could not see (auth/permission honesty)
}

// Source is a context connector. Build runs against the job parameters and returns the
// source's contribution; it must return metadata/structure only, never secret values.
type Source interface {
	// Name is the connector identifier recorded in manifest entries (e.g. "repo-catalog").
	Name() string
	// Build runs the connector against the job parameters and returns its contribution.
	Build(parameters map[string]interface{}, logsWriter io.Writer) (Result, error)
}

var registry []Source

// Register adds a source to the fixed build sequence. Call from a source package's init().
func Register(s Source) {
	registry = append(registry, s)
}

// All returns the registered sources in registration order.
func All() []Source {
	return registry
}
