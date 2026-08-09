package commands

// AWS Bedrock's providerSetup: assume the scoped role, inject short-lived
// credentials, and discover the concrete inference-profile id for a model.
//
// Everything Bedrock-specific is HERE, behind the providerSetup interface — the
// role ARN, the role-chaining session limit, the region-to-geography prefix,
// the profile search. Nothing outside this file names Bedrock, so a second
// provider is a sibling file rather than another branch in shared code.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner/utils"
)

// bedrockRoleArnEnvVar names the env var the CloudFormation runner task def sets
// to the dr-bedrock-role ARN (see cloud-formation-one-click). Empty means the
// runner's stack predates Bedrock support.
const bedrockRoleArnEnvVar = "BedrockRoleArn"

// bedrockMaxSessionSeconds is the ROLE CHAINING limit — 1 hour, hard.
//
// ⚠️ This is NOT dr-bedrock-role's MaxSessionDuration, and raising that will
// not raise this. The runner already runs as an assumed role (its ECS task
// role), so using those credentials to assume dr-bedrock-role is *role
// chaining*, which AWS caps at 1 hour regardless of what either role permits.
// Asking for more fails the whole call:
//
//	ValidationError: The requested DurationSeconds exceeds the 1 hour session
//	limit for roles assumed by role chaining.
//
// after which the guard below logs and the task runs credential-free —
// agentbox then fail-fasts on the missing AWS_* vars. Observed on the first
// live Bedrock run (2026-07-28), which had requested 4h.
//
// The template's MaxSessionDuration: 14400 is therefore moot for this path.
// It is left in place because it is harmless and would matter if the assume
// ever stopped being chained, but it is NOT what governs here.
//
// CONSEQUENCE — creds expire 1h into a task that may run 4h, and env-injected
// credentials do not auto-refresh. A Bedrock task still running past the hour
// loses access mid-run. Raising this constant cannot fix that; the fix is
// refresh over the existing agentbox<->runner RPC channel (a local
// credential_process), which PLAN_opencode_completion_and_bedrock.md scopes
// and defers. Ship v1 at 1h and revisit if long-task expiry is actually hit.
const bedrockMaxSessionSeconds = 3600

// bedrockRegionPrefix returns the cross-region inference prefix for an AWS
// region. Bedrock groups regions into geographies, so this is a coarse mapping
// of the region's first segment, not a per-region lookup.
func bedrockRegionPrefix(region string) string {
	switch {
	case strings.HasPrefix(region, "eu-"):
		return "eu."
	case strings.HasPrefix(region, "ap-"):
		return "apac."
	default:
		return "us."
	}
}

// resolveBedrockModelID discovers the Bedrock inference-profile id for a model,
// or returns "" when it cannot.
//
// Takes the MODEL, not a prefix. The catalogue lookup belongs to Bedrock's own
// code — it is Bedrock's discovery input, and nothing outside should have to
// know that "the thing you search profiles by" is what to pass. Passing a
// string also let the caller supply anything at all, including an already-dotted
// profile id, which is why this used to carry a pass-through branch for input it
// can no longer receive: a hand-pinned id is not in the catalogue, so
// applyAgentModelEnv passes it through without ever calling a resolver.
//
// "" ON EVERY FAILURE, never a guess. The caller has already computed
// model.IDFor(provider) and keeps it when this returns "" — so returning the
// profile PREFIX here (a family token like "claude-sonnet-4-6", not an id)
// would overwrite a correct id with a worse one. That is invisible while the
// two coincide, and silently wrong the moment IDFor gains an override.
//
// Discovery failure is never fatal: the reason is logged and the caller's id
// stands. A wrong model then produces a legible Bedrock error, whereas failing
// the Step here would hide the cause one layer up.
func resolveBedrockModelID(ctx context.Context, cfg aws.Config, model llm_provider_enums.Model, region string, logsWriter io.Writer) string {
	// The model -> profile prefix mapping lives in the shared catalogue, not
	// here: deployment-runner-kit is the one module both the control plane and
	// the runner import, so a second copy in this file was exactly the drift the
	// catalogue exists to prevent. It also carries the guard that a prefix pins
	// the model VERSION — a loose one Contains-matches a neighbouring version's
	// profile and silently runs the wrong model.
	profilePrefix := model.BedrockProfilePrefix()
	if profilePrefix == "" {
		io.WriteString(logsWriter, fmt.Sprintf("Bedrock: %s has no Bedrock profile prefix; leaving the model unchanged.\n", model))
		return ""
	}
	// There is no prefix query: ListInferenceProfiles filters only by TYPE, and
	// GetInferenceProfile wants the exact id — which is the thing being
	// discovered. So the match is client-side, over every page.
	//
	// SYSTEM_DEFINED is a correctness filter, not just a smaller response.
	// APPLICATION profiles are ones the CUSTOMER created; one of those could
	// Contains-match our prefix and win the sort below, silently routing the
	// task through a user-defined profile with its own regions and cost
	// behaviour. We want Amazon's cross-region profiles only.
	paginator := bedrock.NewListInferenceProfilesPaginator(bedrock.NewFromConfig(cfg), &bedrock.ListInferenceProfilesInput{
		MaxResults: aws.Int32(100),
		TypeEquals: bedrocktypes.InferenceProfileTypeSystemDefined,
	})
	prefix := bedrockRegionPrefix(region)
	var matches []string
	// PAGINATED deliberately. A single 100-result page silently truncates: an
	// account whose profile list is longer would look like "no profile for this
	// model", and the task would then run against an id Bedrock does not know.
	// A miss must mean absent, not unread.
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Bedrock: could not list inference profiles (%s); leaving %s unchanged.\n", err, model))
			return ""
		}
		for _, p := range out.InferenceProfileSummaries {
			id := aws.ToString(p.InferenceProfileId)
			if strings.HasPrefix(id, prefix) && strings.Contains(id, profilePrefix) {
				matches = append(matches, id)
			}
		}
	}
	if len(matches) == 0 {
		io.WriteString(logsWriter, fmt.Sprintf("Bedrock: no %s inference profile for %s in %s — check model access for this account/region. Leaving it unchanged.\n", prefix, model, region))
		return ""
	}
	// Descending so the newest revision wins. Because the prefix pins the model
	// version, this only ever chooses between DATES/revisions of the same
	// model — a safe auto-upgrade, not a silent model swap. Logged because a
	// silent pick between several revisions is hard to reconstruct later.
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	io.WriteString(logsWriter, fmt.Sprintf("Bedrock: resolved %s -> %s.\n", model, matches[0]))
	return matches[0]
}

// bedrockSetup assumes dr-bedrock-role and injects its scoped short-lived
// credentials, then resolves model ids through Bedrock's inference profiles.
type bedrockSetup struct{}

// applyBedrockCreds switches a task to AWS Bedrock. Called only for an org
// whose provider IS AWSBedrock — prepareProvider dispatches, so there is no
// provider check here. The runner assumes the minimal dr-bedrock-role and injects the short-lived, Bedrock-only
// credentials into the agent container. No long-lived secret is stored, and the
// agent receives creds that can invoke Bedrock and nothing else. The Bedrock API
// host is added to the egress allowlist. On any failure the task proceeds without
// creds (and fails auth in the container) rather than crashing the runner.
func applyBedrockCreds(env map[string]string, logsWriter io.Writer) (aws.Config, string, bool) {
	roleArn := strings.TrimSpace(os.Getenv(bedrockRoleArnEnvVar))
	if roleArn == "" {
		io.WriteString(logsWriter, "Bedrock: BedrockRoleArn is not set on this runner — update the CloudFormation stack to add the Bedrock role.\n")
		return aws.Config{}, "", false
	}
	region := utils.RunnerData.Get().RunnerRegion
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Bedrock: could not load AWS config: %s\n", err))
		return aws.Config{}, "", false
	}
	out, err := sts.NewFromConfig(cfg).AssumeRole(context.TODO(), &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleArn),
		RoleSessionName: aws.String("agentbox-bedrock"),
		// Match the per-task wall-clock cap. These creds are injected as env
		// vars and do NOT auto-refresh, so a session shorter than the task
		// would expire mid-run. AssumeRole defaults to 1h when this is unset,
		// regardless of the role's MaxSessionDuration - so both this and the
		// role's ceiling (set in the CloudFormation template) are required.
		//
		// STS does NOT clamp this to the role's MaxSessionDuration — asking for
		// more than the role allows FAILS the call outright, after which the
		// guard below logs and runs the task credential-free. bedrockSessionSeconds
		// clamps to what the role permits so that cannot happen; see its comment.
		//
		// The clamp does NOT remove the deploy-order requirement: a stack whose
		// dr-bedrock-role still has the 1h default will reject the 4h request
		// regardless. Update the CloudFormation stack BEFORE deploying a runner
		// that requests 4h. Runners auto-upgrade while customer stacks do not,
		// so before Bedrock is customer-reachable this should also retry at a
		// shorter duration on ValidationError rather than rely on that order.
		DurationSeconds: aws.Int32(bedrockSessionSeconds()),
	})
	if err != nil || out.Credentials == nil {
		io.WriteString(logsWriter, fmt.Sprintf("Bedrock: AssumeRole %s failed: %s\n", roleArn, err))
		return aws.Config{}, "", false
	}
	c := out.Credentials
	env["AWS_ACCESS_KEY_ID"] = aws.ToString(c.AccessKeyId)
	env["AWS_SECRET_ACCESS_KEY"] = aws.ToString(c.SecretAccessKey)
	env["AWS_SESSION_TOKEN"] = aws.ToString(c.SessionToken)
	env["AWS_REGION"] = region
	// Allowlist BOTH Bedrock hosts — agentbox's proxy gates egress, and
	// claude-code uses both planes:
	//
	//   bedrock-runtime.<region> — data plane (InvokeModel*), the obvious one
	//   bedrock.<region>         — control plane (model/inference-profile
	//                              lookups) which claude-code calls at startup
	//
	// The control-plane host was initially judged unnecessary "for pure
	// inference". That was wrong: the first live run logged
	// `denied:bedrock.eu-west-1.amazonaws.com` before the agent had issued a
	// single completion. Both are required.
	//
	// The proxy denies rather than fails closed, so omitting a host degrades
	// oddly instead of erroring clearly — worth keeping both in step with the
	// dr-bedrock-role policy, which already grants ListInferenceProfiles and
	// GetInferenceProfile (control-plane calls).
	bedrockHosts := "bedrock-runtime." + region + ".amazonaws.com" +
		",bedrock." + region + ".amazonaws.com"
	if existing := env["ADDITIONAL_ALLOWED_HOSTS"]; existing != "" {
		env["ADDITIONAL_ALLOWED_HOSTS"] = existing + "," + bedrockHosts
	} else {
		env["ADDITIONAL_ALLOWED_HOSTS"] = bedrockHosts
	}
	io.WriteString(logsWriter, fmt.Sprintf("Bedrock: assumed %s; agent will use Bedrock in %s.\n", roleArn, region))
	// Hand back a config built from the ASSUMED credentials, not the default
	// one used to make the AssumeRole call.
	//
	// cfg here is the runner's own task role, which has no Bedrock permissions
	// — it exists only to call STS. Returning it meant profile discovery ran as
	// dr-task-role and got a 403 on ListInferenceProfiles, while
	// dr-bedrock-role (which grants exactly that) was used for nothing but the
	// container's env vars. The failure was legible but pointed at IAM policy
	// rather than at the caller's identity:
	//
	//   AccessDeniedException: User: .../dr-task-role-... is not authorized to
	//   perform: bedrock:ListInferenceProfiles
	//
	// Same credentials the agent gets, so discovery and inference cannot
	// disagree about who they are — or about when they expire.
	assumed := cfg.Copy()
	assumed.Credentials = credentials.NewStaticCredentialsProvider(
		aws.ToString(c.AccessKeyId),
		aws.ToString(c.SecretAccessKey),
		aws.ToString(c.SessionToken),
	)
	return assumed, region, true
}

// bedrockSessionSeconds is how long a vended Bedrock session should last: the
// per-task wall-clock cap, clamped to the role-chaining limit above. These
// creds are injected as env vars and do NOT auto-refresh, so a session shorter
// than the task expires mid-run — hence asking for as much as is allowed.
// STS also rejects anything below 900s, so guard that end too.
func bedrockSessionSeconds() int32 {
	const stsMinSeconds = 900
	d := int32(defaultWallClockTimeout.Seconds())
	if d > bedrockMaxSessionSeconds {
		return bedrockMaxSessionSeconds
	}
	if d < stsMinSeconds {
		return stsMinSeconds
	}
	return d
}

func (bedrockSetup) provider() llm_provider_enums.Provider { return llm_provider_enums.AWSBedrock }

func (bedrockSetup) prepare(env map[string]string, logsWriter io.Writer) modelResolver {
	cfg, region, ok := applyBedrockCreds(env, logsWriter)
	if !ok {
		// No credentials, so no discovery. Returning a resolver anyway would
		// hand the SDK a zero aws.Config, which PANICS rather than erroring.
		return nil
	}
	return func(ctx context.Context, m llm_provider_enums.Model) string {
		return resolveBedrockModelID(ctx, cfg, m, region, logsWriter)
	}
}
