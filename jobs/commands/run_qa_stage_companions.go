package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner/agenttools"
)

// COMPANION BACKENDS (agentbox minCompanionQAAgentboxVersion and later, on the
// stack path).
//
// A Task's site previews may call web services outside the Task in production:
// deployment-server's CompanionBackendsV1 names them, looked up once per stage.
// Each round deploys an unchanged preview of each ("a companion") from its
// production branch, at the commit the stage fetched the first time it needed
// it, as a backend of the round after the Task's own: its links set the site
// previews' variables to its URL and its allowed-origin variables to their
// origins. The QA run is told which backends are companions, and which site
// variables still call whatever API Preview configuration points them at.
//
// A companion's source never comes from the work dir: it is fetched into a
// temporary bare repository of the stage's with an installation token, every
// git command isolated as the merge result's are, and checked out into a new
// build directory per build.

const (
	// qaCompanionSourceTimeout bounds a companion's token lookup, fetch and
	// submodule check together.
	qaCompanionSourceTimeout = 5 * time.Minute
	// qaCompanionSourceRef is the fetched commit's ref in the temporary
	// repository.
	qaCompanionSourceRef = "refs/companion/source"
	// Limits of what QA_PREVIEWS carries of a companion (agentbox's).
	qaPreviewDeploymentMaxRunes     = 200
	qaPreviewNotDeployedAPIMaxRunes = 200
	qaPreviewNotDeployedReasonMax   = 300
	qaPreviewNotDeployedVariableMax = 128
	qaPreviewNotDeployedMaxItems    = 20
)

// qaCompanion is one companion of the stage: a CompanionBackendsV1 companion and
// its source, fetched once.
type qaCompanion struct {
	// name is "companion-<i>-<Repository.Name>": its repository in QA_PREVIEWS
	// and its name in log lines. Never a directory under the work dir.
	name    string
	backend task_previews.CompanionBackendV1
	// fetched is true once the source fetch ran; commit is its full SHA ("" when
	// the fetch failed) and sourceErr why the companion can't be built.
	fetched   bool
	commit    string
	sourceErr error
	// tmp is the stage's temporary directory for it, holding the bare
	// repository; removed when the stage ends.
	tmp string
}

// qaCompanionName is the name of the reply's i-th companion c.
func qaCompanionName(i int, c task_previews.CompanionBackendV1) string {
	return fmt.Sprintf("companion-%d-%s", i, c.Repository.Name)
}

// qaCompanionOutput is a round's record of one companion. Names, branches and
// SHAs only.
type qaCompanionOutput struct {
	API        string `json:"api"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	// Commit is the full SHA built, "" when the fetch failed.
	Commit string `json:"commit"`
	// ServiceName is its preview's service key, "" when it couldn't be planned.
	ServiceName string `json:"service_name"`
	Deployed    bool   `json:"deployed"`
	Error       string `json:"error,omitempty"`
}

// qaBackendNotDeployedOutput is a site variable that links to a web service
// outside the Task whose preview wasn't deployed.
type qaBackendNotDeployedOutput struct {
	Frontend string `json:"frontend"`
	Variable string `json:"variable"`
	API      string `json:"api,omitempty"`
	Reason   string `json:"reason"`
}

// qaPreviewNotDeployed is a backends_not_deployed item of a site's QA_PREVIEWS
// entry.
type qaPreviewNotDeployed struct {
	Variable string `json:"variable"`
	API      string `json:"api,omitempty"`
	Reason   string `json:"reason"`
}

// stackRepositoriesArg is every Task repository as StackWiringV1 and
// CompanionBackendsV1 take them.
func (s *qaStage) stackRepositoriesArg() []task_previews.StackRepositoryV1 {
	repositories := make([]task_previews.StackRepositoryV1, 0, len(s.ctx.Entries))
	for i, e := range s.ctx.Entries {
		repositories = append(repositories, task_previews.StackRepositoryV1{Repository: fmt.Sprintf("%d-%s", i, e.Name), CloneURL: e.CloneURL})
	}
	return repositories
}

// lookupCompanions asks deployment-server, once per stage (in its first
// stack-path round), for the companions of the round's planned sites. A failed
// lookup leaves the stage with none, and isn't retried.
func (s *qaStage) lookupCompanions(r *qaStackRound) {
	if s.companionsLooked {
		return
	}
	s.companionsLooked = true
	var frontends []task_previews.CompanionFrontendV1
	for _, site := range r.sites {
		if site.planErr != "" || site.site.DeploymentName == "" {
			continue
		}
		frontends = append(frontends, task_previews.CompanionFrontendV1{Repository: site.repository, Frontend: site.site.DeploymentName, Environment: site.site.EnvironmentName})
	}
	if len(frontends) == 0 {
		return
	}
	reply, err := r.ops.companions(s.stackRepositoriesArg(), frontends, r.webDeps.Region)
	if err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: couldn't look up the backends outside this Task that the site previews call: %s\n", err))
		return
	}
	for i, c := range reply.Companions {
		if i >= task_previews.MaxCompanionBackends {
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: a Task deploys at most %d companion backends: no preview of %s (%s)\n", task_previews.MaxCompanionBackends, c.API, c.Repository.Name))
			continue
		}
		s.companions = append(s.companions, &qaCompanion{name: qaCompanionName(i, c), backend: c})
	}
	s.companionsNotDeployed = reply.NotDeployed
	for _, c := range s.companions {
		for _, l := range c.backend.Links {
			names := make([]string, 0, len(l.APIURLVariables))
			for _, v := range l.APIURLVariables {
				names = append(names, v.Name)
			}
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: %s calls %s (%s) through %s — deploying an unchanged preview of it from %s\n",
				l.FrontendRepository, c.backend.API, c.backend.Repository.Name, strings.Join(names, ", "), c.backend.Repository.BaseBranch))
		}
	}
	for _, n := range s.companionsNotDeployed {
		line := fmt.Sprintf("QA stage: %s's %s has no backend preview: %s", n.FrontendRepository, n.Variable, n.Reason)
		if n.API != "" {
			line += fmt.Sprintf(" (%s)", n.API)
		}
		io.WriteString(s.logsWriter, line+"\n")
	}
}

// companionByName is the stage's companion named name, or nil.
func (s *qaStage) companionByName(name string) *qaCompanion {
	for _, c := range s.companions {
		if c.name == name {
			return c
		}
	}
	return nil
}

// planCompanions plans the backend preview of every companion of the stage and
// adds each to the round's backends, after the Task's.
func (r *qaStackRound) planCompanions() {
	for _, c := range r.stage.companions {
		p := &qaStackPreview{repository: c.name, companion: c}
		plan, err := r.ops.planCompanion(r.webDeps, agenttools.CompanionWebService{
			Repository: c.name,
			Entry:      c.backend.Repository,
			API:        c.backend.API,
			Checkout:   r.stage.companionCheckout(c),
		})
		if err != nil {
			r.fail(p, "backend", err)
			p.planErr = p.err
		} else {
			p.backend, p.deploymentName = plan, plan.DeploymentName
		}
		r.companions = append(r.companions, p)
		r.backends = append(r.backends, p)
	}
}

// withCompanionPairs is pairs (pairStack's, site-major) with every (site,
// companion) pair added after each site's own, its link coming from the
// companion's Links alone: StackWiringV1's lookup has no say in it.
func withCompanionPairs(pairs []*qaStackPair, sites, companions []*qaStackPreview) []*qaStackPair {
	if len(companions) == 0 {
		return pairs
	}
	var out []*qaStackPair
	for _, site := range sites {
		for _, p := range pairs {
			if p.site == site {
				out = append(out, p)
			}
		}
		for _, companion := range companions {
			p := &qaStackPair{site: site, backend: companion}
			switch {
			case companion.planErr != "":
				p.reason = qaWiringBackendFailed
			case site.planErr != "":
				p.reason = qaWiringSiteFailed
			default:
				p.link = applyingLink(companionLinks(companion), site, companion)
				if !p.linked() {
					p.reason = qaWiringNoLink
				}
			}
			out = append(out, p)
		}
	}
	return out
}

// companionLinks are the companion's links as StackWiringV1 would give them.
func companionLinks(p *qaStackPreview) []task_previews.StackLinkV1 {
	c := p.companion
	links := make([]task_previews.StackLinkV1, 0, len(c.backend.Links))
	for _, l := range c.backend.Links {
		links = append(links, task_previews.StackLinkV1{
			FrontendRepository:     l.FrontendRepository,
			Frontend:               l.Frontend,
			APIRepository:          c.name,
			API:                    c.backend.API,
			APIURLVariables:        l.APIURLVariables,
			AllowedOriginVariables: l.AllowedOriginVariables,
		})
	}
	return links
}

// companionSource fetches c's source the first time a round needs it, and
// returns why it can't be built (the same in every round), or nil.
func (s *qaStage) companionSource(ctx context.Context, c *qaCompanion) error {
	if c.fetched {
		return c.sourceErr
	}
	c.fetched = true
	timeout := s.companionTimeout
	if timeout <= 0 {
		timeout = qaCompanionSourceTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	commit, err := s.fetchCompanionSource(ctx, c)
	c.commit = commit
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		c.sourceErr = err
		io.WriteString(s.logsWriter, fmt.Sprintf("QA: the preview of %s can't be built: %s\n", c.backend.API, err))
		return err
	}
	io.WriteString(s.logsWriter, fmt.Sprintf("QA: the preview of %s builds from %s at %s\n", c.backend.API, c.backend.Repository.BaseBranch, shortCommit(commit)))
	return nil
}

// fetchCompanionSource fetches the head of c's production branch into a new
// bare repository of the stage's and returns its SHA ("" when the fetch
// failed). A tree with a submodule fails.
func (s *qaStage) fetchCompanionSource(ctx context.Context, c *qaCompanion) (string, error) {
	entry := c.backend.Repository
	branch := entry.BaseBranch
	if !validBranchName(branch) {
		return "", fmt.Errorf("the branch %q isn't a branch name", branch)
	}
	tmp, err := os.MkdirTemp(s.fixTreesTmpRoot, "qa-companion-")
	if err != nil {
		return "", fmt.Errorf("could not create a temporary directory: %s", err)
	}
	c.tmp = tmp
	g := s.companionGit(c)
	if _, err := g.run(ctx, isolatedGit{}, "init", "--bare", "--quiet", "--template=", g.gitDir); err != nil {
		return "", err
	}
	source, secret, err := s.newQAMergeResults().lookupFetchSource(ctx, entry)
	if err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("QA: the installation token for %s couldn't be looked up: %s\n", entry.Name, scrubSecret(err.Error(), secret, false)))
		return "", errors.New("the repository's installation token couldn't be looked up")
	}
	if source == "" || strings.HasPrefix(source, "-") {
		return "", errors.New("the repository has no URL to fetch from")
	}
	o := isolatedGit{secret: secret}
	_, err = g.run(ctx, o, "fetch", "--quiet", "--depth", "1", "--no-tags", "--no-recurse-submodules", source, "+refs/heads/"+branch+":"+qaCompanionSourceRef)
	// FETCH_HEAD notes where the fetch came from; it is no use here.
	_ = os.Remove(filepath.Join(g.gitDir, "FETCH_HEAD"))
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		// The reason is the branch alone; the scrubbed git error is logged.
		io.WriteString(s.logsWriter, fmt.Sprintf("QA: fetching %s of %s failed: %s\n", branch, entry.Name, err))
		return "", fmt.Errorf("%s couldn't be fetched", branch)
	}
	out, err := g.run(ctx, o, "rev-parse", "--verify", "--quiet", qaCompanionSourceRef+"^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if !objectIDPattern.MatchString(commit) {
		return "", errors.New("the fetch gave no commit")
	}
	out, err = g.run(ctx, o, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return commit, err
	}
	for _, e := range strings.Split(out, "\x00") {
		if strings.HasPrefix(e, "160000 ") {
			return commit, errors.New("the repository has submodules")
		}
	}
	return commit, nil
}

// companionCheckout writes c's fetched commit into dir with an index of its
// own. It fails when the source couldn't be fetched: the build never falls back
// to anything else.
func (s *qaStage) companionCheckout(c *qaCompanion) func(ctx context.Context, dir string) error {
	return func(ctx context.Context, dir string) error {
		if !c.fetched {
			return errors.New("its source wasn't fetched")
		}
		if c.sourceErr != nil {
			return c.sourceErr
		}
		f, err := os.CreateTemp(c.tmp, "build-index-")
		if err != nil {
			return err
		}
		index := f.Name()
		f.Close()
		// git writes the index itself: an empty file is not one.
		_ = os.Remove(index)
		defer os.Remove(index)
		g := s.companionGit(c)
		o := isolatedGit{workTree: dir, index: index}
		if _, err := g.run(ctx, o, "read-tree", c.commit); err != nil {
			return err
		}
		_, err = g.run(ctx, o, "checkout-index", "--all", "--force")
		return err
	}
}

// removeCompanionSources removes every companion's temporary directory.
func (s *qaStage) removeCompanionSources() {
	for _, c := range s.companions {
		if c.tmp != "" {
			_ = os.RemoveAll(c.tmp)
		}
	}
}

// qaCompanionGit runs git for a companion's temporary bare repository alone:
// GIT_DIR with an empty config, no repository's config or alternates, no
// inherited GIT_* variable, its own process group killed on timeout.
type qaCompanionGit struct {
	git, tmp, gitDir string
}

func (s *qaStage) companionGit(c *qaCompanion) qaCompanionGit {
	git := s.fixTreesGit
	if git == "" {
		git = "git"
	}
	return qaCompanionGit{git: git, tmp: c.tmp, gitDir: filepath.Join(c.tmp, "source.git")}
}

// run runs one git command bounded by parent and fixTreesGitTimeout, in o's
// work tree with o's index when given; o.secret is scrubbed from its error.
func (g qaCompanionGit) run(parent context.Context, o isolatedGit, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, fixTreesGitTimeout)
	defer cancel()
	dir := g.tmp
	if o.workTree != "" {
		dir = o.workTree
	}
	cmd := exec.CommandContext(ctx, g.git, append([]string{"-c", "safe.directory=*", "-C", dir}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = fixTreesGitWaitDelay
	cmd.Env = append(withoutGitEnv(os.Environ()),
		"GIT_DIR="+g.gitDir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)
	if o.workTree != "" {
		cmd.Env = append(cmd.Env, "GIT_WORK_TREE="+o.workTree)
	}
	if o.index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+o.index)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr := &limitedBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", isolatedGitError(parent, ctx, args[0], err, scrubSecret(stderr.buf.String(), o.secret, stderr.overflow))
	}
	return stdout.String(), nil
}

// companionPreview is a companion's QA_PREVIEWS entry: a backend's, marked.
func (r *qaStackRound) companionPreview(p *qaStackPreview) qaPreview {
	entry := r.backendPreview(p)
	entry.Companion = true
	entry.Deployment = qaPreviewText(p.companion.backend.API, qaPreviewDeploymentMaxRunes)
	if entry.Deployment == "" {
		entry.Deployment = p.companion.name
	}
	return entry
}

// backendsNotDeployed is a deployed site's backends_not_deployed: the reply's
// items for its repository and deployment, each as agentbox accepts it — an
// item whose variable it would refuse, or that repeats one, is left out — and
// at most qaPreviewNotDeployedMaxItems.
func (r *qaStackRound) backendsNotDeployed(site *qaStackPreview) []qaPreviewNotDeployed {
	if site.deploymentName == "" {
		return nil
	}
	var out []qaPreviewNotDeployed
	seen := map[string]bool{}
	for _, n := range r.stage.companionsNotDeployed {
		if n.FrontendRepository != site.repository || n.Frontend != site.deploymentName {
			continue
		}
		if !qaPreviewVariableName(n.Variable) || seen[n.Variable] {
			continue
		}
		reason := qaPreviewText(n.Reason, qaPreviewNotDeployedReasonMax)
		if reason == "" {
			continue
		}
		seen[n.Variable] = true
		out = append(out, qaPreviewNotDeployed{Variable: n.Variable, API: qaPreviewText(n.API, qaPreviewNotDeployedAPIMaxRunes), Reason: reason})
		if len(out) == qaPreviewNotDeployedMaxItems {
			break
		}
	}
	return out
}

// qaPreviewVariableName reports whether agentbox accepts name as a variable:
// non-empty, at most qaPreviewNotDeployedVariableMax characters, no whitespace
// or control character.
func qaPreviewVariableName(name string) bool {
	if name == "" || len([]rune(name)) > qaPreviewNotDeployedVariableMax {
		return false
	}
	for _, c := range name {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// qaPreviewText is s as agentbox accepts it: no control character, trimmed and
// at most max characters.
func qaPreviewText(s string, max int) string {
	s = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max]))
	}
	return s
}

// qaAddressPatterns match what names where a request went in an error: a URL,
// an IP address (v4, or v6 in brackets) or a host name with a port, the host
// names of a certificate that didn't match, the host name of a failed DNS
// lookup, and any other dotted name.
var qaAddressPatterns = []*regexp.Regexp{
	regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]*`),
	regexp.MustCompile(`\[[0-9A-Fa-f:.%]*:[0-9A-Fa-f:.%]*\](:[0-9]+)?`),
	regexp.MustCompile(`\b[0-9]{1,3}(\.[0-9]{1,3}){3}(:[0-9]+)?\b`),
	regexp.MustCompile(`\b(localhost|[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+):[0-9]+\b`),
	regexp.MustCompile(`certificate is valid for .*?, not [^\s:;,]+`),
	regexp.MustCompile(`certificate is not valid for any names, but wanted to match [^\s:;,]+`),
	regexp.MustCompile(`\blookup [^\s:]+`),
	regexp.MustCompile(`\b[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z][A-Za-z0-9-]*\b`),
}

// qaWithoutAddresses is msg with every URL, address and host name in it
// replaced by "<address>", so a companion's Job output record names no
// endpoint. "deployment.io" and each of keep (the companion's own names) are
// left as they are: only a dotted one could be taken for a host name.
func qaWithoutAddresses(msg string, keep ...string) string {
	keep = append([]string{"deployment.io"}, keep...)
	sort.SliceStable(keep, func(i, j int) bool { return len(keep[i]) > len(keep[j]) })
	var kept []string
	for _, k := range keep {
		if !strings.Contains(k, ".") || !strings.Contains(msg, k) {
			continue
		}
		msg = strings.ReplaceAll(msg, k, fmt.Sprintf("\x00%d\x00", len(kept)))
		kept = append(kept, k)
	}
	for _, p := range qaAddressPatterns {
		msg = p.ReplaceAllStringFunc(msg, func(m string) string {
			switch {
			case strings.HasPrefix(m, "lookup "):
				return "lookup <address>"
			case strings.HasPrefix(m, "certificate is valid for "):
				return "certificate is valid for <address>, not <address>"
			case strings.HasPrefix(m, "certificate is not valid for any names"):
				return "certificate is not valid for any names, but wanted to match <address>"
			}
			return "<address>"
		})
	}
	for i := len(kept) - 1; i >= 0; i-- {
		msg = strings.ReplaceAll(msg, fmt.Sprintf("\x00%d\x00", i), kept[i])
	}
	return msg
}

// companionOutputs is the round's record of each companion, in the reply's
// order; nil when the stage has none.
func (r *qaStackRound) companionOutputs() []qaCompanionOutput {
	var out []qaCompanionOutput
	for _, p := range r.companions {
		c := p.companion
		o := qaCompanionOutput{
			API:        c.backend.API,
			Repository: c.backend.Repository.Name,
			Branch:     c.backend.Repository.BaseBranch,
			Commit:     c.commit,
			Deployed:   p.err == "" && p.url != "",
			Error:      qaWithoutAddresses(p.err, c.backend.API, c.backend.Repository.Name, c.backend.Repository.BaseBranch),
		}
		if p.planErr == "" {
			o.ServiceName = p.backend.ServiceName
		}
		out = append(out, o)
	}
	return out
}

// backendsNotDeployedOutputs is every NotDeployed item of the stage's lookup,
// its frontend by entry name; nil when there are none.
func (s *qaStage) backendsNotDeployedOutputs() []qaBackendNotDeployedOutput {
	var out []qaBackendNotDeployedOutput
	for _, n := range s.companionsNotDeployed {
		out = append(out, qaBackendNotDeployedOutput{Frontend: s.entryName(n.FrontendRepository), Variable: n.Variable, API: n.API, Reason: n.Reason})
	}
	return out
}
