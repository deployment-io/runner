package commands

import (
	"context"
	"fmt"
	"io"

	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// How a Job's PROVIDER is chosen and how its MODEL is rendered for that
// provider — split out of run_agent_step.go, which is about spawning a
// container, not about who serves the model.
//
// The seam is providerSetup: a provider's own credentials, discovery and
// quirks live in ITS OWN FILE (bedrock_setup.go), and this file only ever holds
// a llm_provider_enums.Provider and a map of resolvers. Adding a provider is a
// new file plus one registry entry — no signature grows, nothing here learns
// its name, and none of this needs reading to write it.

// resolveJobProvider returns the provider serving this Job, or 0 when the
// parameter is missing or names a provider this build does not know.
//
// deployment-server decided this when it resolved credentials, so the
// parameter is the only source — the provider is never inferred from which
// secrets happen to be present. An unknown slug means this runner is older
// than the control plane; 0 is invalid, so the model is left unrendered and no
// provider setup runs, and the agent fails on the credential rather than on a
// guess.
func resolveJobProvider(parameters map[string]interface{}, logsWriter io.Writer) llm_provider_enums.Provider {
	slug, err := jobs.GetParameterValue[string](parameters, parameters_enums.AgentProvider)
	if err != nil || slug == "" {
		io.WriteString(logsWriter, "No agent provider on this Job — deployment-server should have set it at pickup.\n")
		return 0
	}
	provider, err := llm_provider_enums.ProviderFromKey(slug)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Unknown agent provider %q — this runner may be older than the control plane.\n", slug))
		return 0
	}
	return provider
}

// agentSpawn is the pair every decision here turns on: WHO serves the model and
// WHAT runs it. Grouped because they always travel together and because
// rendering needs BOTH — an id is not correct in isolation, only correct for a
// given agent talking to a given provider.
type agentSpawn struct {
	provider  llm_provider_enums.Provider
	agentType llm_provider_enums.AgentType
}

// modelResolver returns the concrete provider-side id for a model, or "" when
// it cannot produce one.
//
// Exists so the call site never names a provider. Each implementation reaches
// for its OWN discovery input — Bedrock uses Model.BedrockProfilePrefix, Vertex
// would use whatever it needs — instead of the caller branching to pick one.
type modelResolver func(ctx context.Context, m llm_provider_enums.Model) string

// providerSetup prepares one provider for an agent spawn.
//
// Exists so nothing provider-specific crosses the spawn path. Bedrock needs an
// aws.Config, a region and a did-it-work flag; Vertex will need a project, a
// location and a service account. Threading those through as parameters means
// the generic call site names every provider and grows by three arguments per
// provider added. Here they are a setup's own state, captured in the closure it
// returns, and never seen by anyone else.
type providerSetup interface {
	provider() llm_provider_enums.Provider
	// prepare injects whatever the agent needs to reach this provider, and
	// returns a resolver for its model ids — or nil when the provider declares
	// its ids (no discovery needed) or the setup failed.
	prepare(env map[string]string, logsWriter io.Writer) modelResolver
}

// providerSetups is the registry. A new provider is one entry here plus its own
// type; no call site changes.
var providerSetups = []providerSetup{bedrockSetup{}}

// prepareProvider runs the setup for the provider serving this Job and returns
// the resolvers that are ACTUALLY usable.
//
// Absence is the availability signal: a provider whose setup failed simply has
// no entry, so no boolean is threaded through the call chain and a caller
// cannot mistake "credentials failed" for "this provider needs no discovery".
func prepareProvider(env map[string]string, p llm_provider_enums.Provider, logsWriter io.Writer) map[llm_provider_enums.Provider]modelResolver {
	resolvers := map[llm_provider_enums.Provider]modelResolver{}
	for _, setup := range providerSetups {
		if setup.provider() != p {
			continue
		}
		if resolve := setup.prepare(env, logsWriter); resolve != nil {
			resolvers[p] = resolve
		}
	}
	return resolvers
}

// applyAgentModelEnv renders the Task's LOGICAL model id into whatever the
// selected agent actually reads.
//
// This runs for every spawn, not just Bedrock ones. Model ids are logical now
// ("claude-sonnet-4-6"), and opencode needs a "provider/model" string, so an
// opencode task on Anthropic would otherwise receive a bare id with no way to
// know where to route it.
//
// The provider drives everything and no branch names one:
//
//	IDFor                      — the provider's own name for the model
//	ResolvesModelIDAtRuntime   — whether the concrete id must be discovered
//	OpencodeModelID            — the "provider/model" rendering
//
// Adding Vertex later means setting that property and supplying its discovery;
// nothing here changes.
func applyAgentModelEnv(env map[string]string, spawn agentSpawn, resolvers map[llm_provider_enums.Provider]modelResolver, logsWriter io.Writer) {
	provider := spawn.provider
	logical := env["MODEL"]
	if logical == "" {
		return
	}
	if !provider.IsValid() {
		// No recognisable credential. Leave the model untouched so the agent
		// fails on the missing credential rather than on a mangled id.
		return
	}

	id := logical
	if model, err := llm_provider_enums.GetModel(logical); err == nil {
		id = model.IDFor(provider)
		if provider.ResolvesModelIDAtRuntime() {
			// The provider says a concrete id must be DISCOVERED; the map says
			// whether we can do that right now. Splitting the two keeps a
			// credential failure from looking like "this provider does not need
			// discovery", and means a second such provider adds a map entry
			// rather than another boolean.
			resolve, available := resolvers[provider]
			if !available {
				io.WriteString(logsWriter, fmt.Sprintf("%s: cannot resolve %q — credentials unavailable; leaving the model unchanged.\n", provider, logical))
			} else if resolved := resolve(context.TODO(), model); resolved != "" {
				id = resolved
			}
		}
	} else {
		io.WriteString(logsWriter, fmt.Sprintf("Model %q is not in the catalogue; passing it through unchanged.\n", logical))
	}

	if spawn.agentType == llm_provider_enums.Opencode {
		if rendered := llm_provider_enums.OpencodeModelID(id, provider); rendered != "" {
			env["MODEL"] = rendered
		}
		return
	}
	// claude-code in Bedrock mode takes the model from ANTHROPIC_MODEL rather
	// than --model. The condition MIRRORS ApplyClaudeCodeUseBedrock: whichever
	// agents get that switch must be exactly the ones told where to read the
	// model, or an agent runs in Bedrock mode looking at the wrong variable.
	// Testing the provider alone would have set ANTHROPIC_MODEL for codex too.
	if provider == llm_provider_enums.AWSBedrock && spawn.agentType == llm_provider_enums.ClaudeCode {
		env["ANTHROPIC_MODEL"] = id
		return
	}
	env["MODEL"] = id
}
