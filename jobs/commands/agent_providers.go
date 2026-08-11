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
	// model is the LOGICAL id the Task asked for ("claude-sonnet-4-6"), taken
	// from the job parameter — not read back out of env. Rendering it is this
	// package's job, so taking it as input keeps the function from depending on
	// whether some earlier step happened to have written env["MODEL"] yet.
	model string
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

// providerSetups is the registry of providers needing RUNNER-SIDE preparation,
// which in practice means the cloud-role ones: their credentials are assumed
// here rather than arriving in the bundle, and their model ids must be
// discovered rather than declared.
//
// The API-key and subscription providers deliberately have no entry. There is
// nothing for a setup to do — their credentials are already in the injected
// bundle and their model ids are declared — so an entry would be an empty type
// whose only content is "nothing to do", and would break the invariant that a
// registered setup means discovery is needed.
//
// A new provider is one entry here plus its own file; no call site changes.
var providerSetups = []providerSetup{bedrockSetup{}}

// prepareProvider runs the setup for the provider serving this Job and returns
// its resolver, or nil when it has none.
//
// ONE resolver, not a map keyed by provider. A Job is served by exactly one
// provider — the map could only ever hold that one entry, under a key the
// caller already had. Provider PRIORITY does not change this: choosing which
// provider wins happens upstream, so by the time a model is being rendered the
// choice is already made.
//
// nil is the availability signal, so no boolean is threaded through the call
// chain and a caller cannot mistake "credentials failed" for "this provider
// needs no discovery" — ResolvesModelIDAtRuntime answers the second.
func prepareProvider(env map[string]string, p llm_provider_enums.Provider, logsWriter io.Writer) modelResolver {
	for _, setup := range providerSetups {
		if setup.provider() == p {
			return setup.prepare(env, logsWriter)
		}
	}
	return nil
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
// The one thing the provider does NOT decide is whether a Bedrock id keeps its
// cross-region geography prefix. That is a property of the model's VENDOR, so
// the vendor is read off the catalogue here and handed to the renderer rather
// than inferred from the id's own text.
//
// Adding Vertex later means setting that property and supplying its discovery;
// nothing here changes.
func applyAgentModelEnv(env map[string]string, spawn agentSpawn, resolve modelResolver, logsWriter io.Writer) error {
	provider := spawn.provider
	logical := spawn.model
	if logical == "" {
		return nil
	}
	if !provider.IsValid() {
		// No recognisable credential. Leave the model untouched so the agent
		// fails on the missing credential rather than on a mangled id.
		return nil
	}

	// The vendor travels with the id because opencode's rendering depends on
	// WHO MAKES the model, not on how it is reached — models.dev lists some
	// vendors' Bedrock ids under their cross-region geography prefix and others
	// only bare. VendorUnknown is the honest answer for a model outside the
	// catalogue, and means the id is passed through untouched.
	vendor := llm_provider_enums.VendorUnknown

	id := logical
	if model, err := llm_provider_enums.GetModel(logical); err == nil {
		id = model.IDFor(provider)
		vendor = model.Vendor()
		if provider.ResolvesModelIDAtRuntime() {
			// Two different questions, deliberately kept apart: the provider
			// says whether an id must be DISCOVERED, and a nil resolver says
			// whether we can discover right now. Collapsing them would make a
			// credential failure look like "this provider needs no discovery".
			if resolve == nil {
				// CREDENTIALS, not model access — and the two must not be
				// collapsed, which is what the paragraph above is warning
				// about. Degrades to the logical id so the agent reports a
				// credential problem in its own words. Telling the user to
				// enable model access here would name the wrong cause.
				io.WriteString(logsWriter, fmt.Sprintf("%s: cannot resolve %q — credentials unavailable; leaving the model unchanged.\n", provider, logical))
			} else if resolved := resolve(context.TODO(), model); resolved != "" {
				id = resolved
			} else if model.BedrockProfilePrefix() != "" {
				// FAIL FAST: discovery RAN and found nothing.
				//
				// A profile prefix means the concrete id can ONLY come from
				// discovery — the catalogue guarantees a model has a prefix or
				// a declared id, never both. So an empty result leaves the
				// LOGICAL id ("claude-opus-5"), which no provider accepts. The
				// run is already lost; the only question is whether it is lost
				// here or several minutes later.
				//
				// It used to be later: we logged the reason, spawned the
				// container, cloned the repo, installed the CLI, and let the
				// agent fail on an error that never mentioned model access. The
				// explanation sat in the job logs while the Task error said
				// nothing useful — a live opencode run showed exactly that
				// shape. Returning here makes the actionable message the error
				// itself.
				//
				// Guarded on the PREFIX, not the provider. The Bedrock-only
				// lineup (Qwen, DeepSeek, GLM, MiniMax, Grok) has no profile
				// and declares its id, so discovery finding nothing is CORRECT
				// for them — guarding on provider would break every one.
				return fmt.Errorf(
					"model %q could not be resolved at %s: no matching inference profile in this account/region. "+
						"Enable model access for it in the AWS console, or pick a different model",
					logical, provider)
			}
		}
	} else {
		io.WriteString(logsWriter, fmt.Sprintf("Model %q is not in the catalogue; passing it through unchanged.\n", logical))
	}

	if spawn.agentType == llm_provider_enums.Opencode {
		rendered := llm_provider_enums.OpencodeModelID(id, provider, vendor)
		if rendered == "" {
			// Unroutable — opencode has no name for this provider. Returns
			// WITHOUT writing the env, so MODEL keeps what the Task asked for
			// and the resulting error names that rather than a half-resolved
			// id. Deliberately not an error like the discovery case above:
			// this is a catalogue contradiction, not an account problem, so
			// there is nothing the user could act on.
			return nil
		}
		id = rendered
	}
	// WHERE the model goes is the catalogue's call, not ours: it is the same
	// fact as claude-code's Bedrock switch, and the two must agree — an agent in
	// Bedrock mode reading the wrong variable is broken either way. This file
	// wrote that rule out itself and got it wrong twice: once keying on the
	// provider alone, so codex on Bedrock was pointed at claude-code's
	// variable, and once writing ANTHROPIC_MODEL *instead of* MODEL, which left
	// agentbox passing the unresolved logical id as --model.
	llm_provider_enums.ApplyModelEnv(env, id, provider, spawn.agentType)
	return nil
}
