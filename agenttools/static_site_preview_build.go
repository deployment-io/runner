package agenttools

// static_site_preview_build.go is deploy_static_site_preview's runner-built path. For a
// repository deployment.io deploys as a static site, the runner builds the preview the
// way the deploy builds it — the deployment's build command, root directory, publish
// directory and is_spa — from a copy of the agent's working tree the agent can't see,
// with every variable and secret file of the org's preview configuration. The agent
// never sees those values: they reach the build and nothing else, and neither the
// tool's result nor the Step log line names one.
//
// For a repository deployment.io doesn't deploy, the runner detects the build settings
// from the repository (DetectSite: the build command from package.json, the publish
// directory from the framework, is_spa from the router) and builds the preview the
// same way, with the same configuration. Only when nothing can be detected does the
// agent build it itself.
//
// A build can outlast an MCP tool call (the agent's CLI gives up on a call after about
// two minutes), so it runs in the background per service and a call waits for it at
// most previewBuildCallWait; the agent calls again to keep waiting.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// previewBuildCallWait is the longest one deploy_static_site_preview call waits for a
// runner-built preview — under the agent CLI's ~2 minute tool-call timeout. A var so
// tests can shorten it.
var previewBuildCallWait = 100 * time.Second

// PreviewBuildSettingsTimeout bounds the build-settings lookup a call makes before it
// starts or attaches to a build, so the lookup and the wait together stay within
// previewBuildCallWait. A var so tests can shorten it.
var PreviewBuildSettingsTimeout = 30 * time.Second

// previewBuildingResult is a call's answer while its service's build is still running.
type previewBuildingResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

const previewBuildingMessage = "The preview is still building. Call deploy_static_site_preview again with the same arguments to wait for it."

// runnerBuiltPreview is everything a runner-built preview needs, decided before the
// build starts.
type runnerBuiltPreview struct {
	repository string // the repository's directory under the work dir, "<idx>-<name>"
	// site is the deployment's build settings or, for a detected build, the detected
	// ones (its DeploymentName then the name below).
	site task_previews.StaticSiteBuildSettingsV1
	// name names the site in messages and the log line: the deployment's name, or
	// "<repository>" / "<repository>/<root>" for a detected build. "" → site's
	// DeploymentName.
	name string
	// detected is the site detected from the repository; nil for a deployed site.
	detected *DetectedSite
	// notDetected is, when planRunnerBuiltPreview returns ok=false, why no settings
	// could be detected ("" when detection isn't available).
	notDetected   string
	rootDirectory string // site.RootDirectory, cleaned; "" for the repository root
	publishDir    string // site.PublishDirectory, cleaned, relative to rootDirectory
	serviceName   string
	configuration task_previews.StaticSiteBuildSettingsReplyV1 // ConfigurationSet, Variables, Files
}

// displayName names the site in messages and the Step log line.
func (p runnerBuiltPreview) displayName() string {
	if p.name != "" {
		return p.name
	}
	return p.site.DeploymentName
}

// planRunnerBuiltPreview resolves repository among the Task's repositories and asks
// deployment-server how it's deployed. When deployment.io doesn't deploy it as a static
// site, the settings are detected from the repository (planDetectedPreview).
// ok=false (no error) when nothing can be detected either — the agent-built path, with
// the reason in the returned plan's notDetected.
func planRunnerBuiltPreview(deps DeployStaticSitePreviewDeps, repository string, rootDirectory *string) (runnerBuiltPreview, bool, error) {
	repoDir, entry, err := resolveTaskRepository(deps.Repositories, repository)
	if err != nil {
		return runnerBuiltPreview{}, false, err
	}
	if deps.BuildSettings == nil {
		return runnerBuiltPreview{}, false, fmt.Errorf("deployment.io can't look up how %s is deployed here", repoDir)
	}
	settings, err := lookupBuildSettings(deps.BuildSettings, entry.CloneURL)
	if err != nil {
		return runnerBuiltPreview{}, false, fmt.Errorf("look up how deployment.io deploys %s: %w", repoDir, err)
	}
	if len(settings.Sites) == 0 {
		return planDetectedPreview(deps, repoDir, rootDirectory, settings)
	}
	site, err := chooseStaticSite(settings.Sites, rootDirectory)
	if err != nil {
		return runnerBuiltPreview{}, false, err
	}
	if strings.TrimSpace(site.BuildCommand) == "" {
		return runnerBuiltPreview{}, false, fmt.Errorf("%s has no build command, so its preview can't be built", site.DeploymentName)
	}
	root, err := cleanRelativeDir(site.RootDirectory)
	if err != nil {
		return runnerBuiltPreview{}, false, fmt.Errorf("%s's root directory: %w", site.DeploymentName, err)
	}
	publish, err := cleanRelativeDir(site.PublishDirectory)
	if err != nil {
		return runnerBuiltPreview{}, false, fmt.Errorf("%s's publish directory: %w", site.DeploymentName, err)
	}
	// As the deploy refuses it: the root directory itself would publish the sources
	// and the secret files written there.
	if publish == "" {
		return runnerBuiltPreview{}, false, fmt.Errorf("%s's publish directory is its root directory, so its preview can't be built", site.DeploymentName)
	}
	// The same key an agent-built deploy of that output gets from its publish_dir, so
	// both are one preview.
	serviceName, ok := deriveServiceName(repoDir + "/" + root + "/" + publish)
	if !ok {
		return runnerBuiltPreview{}, false, fmt.Errorf("could not derive a service name for %s", site.DeploymentName)
	}
	return runnerBuiltPreview{
		repository:    repoDir,
		site:          site,
		name:          site.DeploymentName,
		rootDirectory: root,
		publishDir:    publish,
		serviceName:   serviceName,
		configuration: settings,
	}, true, nil
}

// planDetectedPreview plans the preview of a repository deployment.io doesn't deploy
// from the settings DetectSite finds in <WorkDirHost>/<repoDir>/<root_directory>.
// ok=false when nothing is detected, with the reason in the plan's notDetected.
func planDetectedPreview(deps DeployStaticSitePreviewDeps, repoDir string, rootDirectory *string, settings task_previews.StaticSiteBuildSettingsReplyV1) (runnerBuiltPreview, bool, error) {
	if deps.DetectSite == nil {
		return runnerBuiltPreview{}, false, nil
	}
	root := ""
	if rootDirectory != nil {
		r, err := cleanRelativeDir(*rootDirectory)
		if err != nil {
			return runnerBuiltPreview{}, false, fmt.Errorf("root_directory: %w", err)
		}
		root = r
	}
	repoHostDir := filepath.Join(deps.WorkDirHost, repoDir)
	siteDir := filepath.Join(repoHostDir, root)
	if info, err := os.Stat(siteDir); err != nil || !info.IsDir() {
		return runnerBuiltPreview{}, false, fmt.Errorf("root_directory %q isn't a directory in %s", root, repoDir)
	}
	if err := checkInside(repoHostDir, siteDir); err != nil {
		return runnerBuiltPreview{}, false, fmt.Errorf("root_directory %q resolves outside %s", root, repoDir)
	}
	detected, reason, found := deps.DetectSite(siteDir)
	if !found {
		return runnerBuiltPreview{notDetected: reason}, false, nil
	}
	name := repoDir
	if root != "" {
		name = repoDir + "/" + root
	}
	publish, err := cleanRelativeDir(detected.PublishDirectory)
	if err != nil {
		return runnerBuiltPreview{}, false, fmt.Errorf("%s's detected publish directory: %w", name, err)
	}
	if publish == "" {
		return runnerBuiltPreview{}, false, fmt.Errorf("%s's publish directory is its root directory, so its preview can't be built", name)
	}
	serviceName, ok := deriveServiceName(repoDir + "/" + root + "/" + publish)
	if !ok {
		return runnerBuiltPreview{}, false, fmt.Errorf("could not derive a service name for %s", name)
	}
	return runnerBuiltPreview{
		repository: repoDir,
		site: task_previews.StaticSiteBuildSettingsV1{
			DeploymentName:   name,
			RootDirectory:    root,
			BuildCommand:     detected.BuildCommand,
			PublishDirectory: publish,
			IsSpa:            detected.IsSPA,
		},
		name:          name,
		detected:      &detected,
		rootDirectory: root,
		publishDir:    publish,
		serviceName:   serviceName,
		configuration: settings,
	}, true, nil
}

// lookupBuildSettings calls lookup, giving up after PreviewBuildSettingsTimeout so a
// stalled request never holds the tool call past the agent CLI's timeout. (The
// runner's client also puts that deadline on the request itself.)
func lookupBuildSettings(lookup func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error), cloneURL string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
	type answer struct {
		reply task_previews.StaticSiteBuildSettingsReplyV1
		err   error
	}
	ch := make(chan answer, 1)
	go func() {
		reply, err := lookup(cloneURL)
		ch <- answer{reply, err}
	}()
	timer := time.NewTimer(PreviewBuildSettingsTimeout)
	defer timer.Stop()
	select {
	case a := <-ch:
		return a.reply, a.err
	case <-timer.C:
		return task_previews.StaticSiteBuildSettingsReplyV1{}, errors.New("deployment.io didn't answer in time; call deploy_static_site_preview again")
	}
}

// resolveTaskRepository matches repository (its directory under /work, with or
// without the /work prefix) against the Task's repositories, "<idx>-<name>".
func resolveTaskRepository(entries []tasks.RepositoryEntry, repository string) (string, tasks.RepositoryEntry, error) {
	r := strings.TrimSpace(repository)
	r = strings.TrimPrefix(r, containerWorkDir+"/")
	r = strings.Trim(r, "/")
	dirs := make([]string, 0, len(entries))
	for i, e := range entries {
		dir := fmt.Sprintf("%d-%s", i, e.Name)
		if r == dir {
			return dir, e, nil
		}
		dirs = append(dirs, dir)
	}
	if len(dirs) == 0 {
		return "", tasks.RepositoryEntry{}, fmt.Errorf("unknown repository %q: this Task has no repositories", repository)
	}
	return "", tasks.RepositoryEntry{}, fmt.Errorf("unknown repository %q — pass one of the Task's repositories: %s", repository, strings.Join(dirs, ", "))
}

// chooseStaticSite picks the site to build: the first when all sites build the same
// way, else the one (or the identically-built ones) whose root directory is
// rootDirectory — nil when the agent didn't pass it; "" or "." is the repository
// root. Otherwise the error lists each site's root directory. A rootDirectory that
// leaves the repository is an error, never the repository root.
func chooseStaticSite(sites []task_previews.StaticSiteBuildSettingsV1, rootDirectory *string) (task_previews.StaticSiteBuildSettingsV1, error) {
	if rootDirectory != nil {
		if _, err := cleanRelativeDir(*rootDirectory); err != nil {
			return task_previews.StaticSiteBuildSettingsV1{}, fmt.Errorf("root_directory: %w", err)
		}
	}
	if sameBuild(sites) {
		return sites[0], nil
	}
	if rootDirectory != nil {
		want, _ := cleanRelativeDir(*rootDirectory) // validated above
		var matches []task_previews.StaticSiteBuildSettingsV1
		for _, s := range sites {
			if root, _ := cleanRelativeDir(s.RootDirectory); root == want {
				matches = append(matches, s)
			}
		}
		if len(matches) > 0 && sameBuild(matches) {
			return matches[0], nil
		}
	}
	var b strings.Builder
	b.WriteString("deployment.io deploys this repository as several static sites with different settings, so pass root_directory to pick one:")
	for _, s := range sites {
		root, _ := cleanRelativeDir(s.RootDirectory)
		fmt.Fprintf(&b, "\n- %s (environment %s): root_directory %q", s.DeploymentName, s.EnvironmentName, root)
		if root == "" {
			b.WriteString(" (the repository root)")
		}
	}
	return task_previews.StaticSiteBuildSettingsV1{}, errors.New(b.String())
}

// sameBuild reports whether every site builds the same way: same root directory,
// build command, publish directory and is_spa.
func sameBuild(sites []task_previews.StaticSiteBuildSettingsV1) bool {
	key := func(s task_previews.StaticSiteBuildSettingsV1) [4]string {
		root, _ := cleanRelativeDir(s.RootDirectory)
		publish, _ := cleanRelativeDir(s.PublishDirectory)
		return [4]string{root, strings.TrimSpace(s.BuildCommand), publish, fmt.Sprint(s.IsSpa)}
	}
	for _, s := range sites[1:] {
		if key(s) != key(sites[0]) {
			return false
		}
	}
	return true
}

// cleanRelativeDir normalizes a directory setting the way the deploy's checkout does
// (a leading "." and "/" and a trailing "/" are dropped), cleaned, "" for the top. A
// directory that leaves its parent is refused.
func cleanRelativeDir(dir string) (string, error) {
	d := strings.TrimSpace(dir)
	d = strings.TrimPrefix(d, ".")
	d = strings.TrimPrefix(d, "/")
	d = strings.TrimSuffix(d, "/")
	d = path.Clean("./" + d)
	if d == "." {
		return "", nil
	}
	if d == ".." || strings.HasPrefix(d, "../") {
		return "", fmt.Errorf("%q leaves the repository", dir)
	}
	return d, nil
}

// configurationText says which configuration a runner-built preview was built with,
// without a value.
func configurationText(c task_previews.StaticSiteBuildSettingsReplyV1) string {
	if !c.ConfigurationSet {
		return "none — no preview configuration is set"
	}
	return fmt.Sprintf("preview configuration (%d variables, %d secret files)", len(c.Variables), len(c.Files))
}

// detectedSettingsText describes the settings of a detected build, without a value
// of the configuration.
func detectedSettingsText(p runnerBuiltPreview) string {
	if p.detected == nil {
		return ""
	}
	spa := "no"
	if p.detected.IsSPA {
		spa = "yes"
	}
	return fmt.Sprintf("detected from the repository: %s, build %q, publish directory %q, single-page app: %s",
		p.detected.Framework, p.detected.BuildCommand, p.publishDir, spa)
}

// previewBuildLogLine is the Step log's line for a runner-built preview.
func previewBuildLogLine(p runnerBuiltPreview) string {
	if p.detected != nil {
		return fmt.Sprintf("Preview build: %s built by deployment.io from settings detected in %s (%s) with %s\n",
			p.serviceName, p.displayName(), p.detected.Framework, configurationText(p.configuration))
	}
	return fmt.Sprintf("Preview build: %s built by deployment.io from %s with %s\n", p.serviceName, p.displayName(), configurationText(p.configuration))
}

// previewBuildEnv is the build's environment: the preview configuration's variables
// as "KEY=value", sorted by key.
func previewBuildEnv(c task_previews.StaticSiteBuildSettingsReplyV1) []string {
	if !c.ConfigurationSet {
		return nil
	}
	keys := make([]string, 0, len(c.Variables))
	for k := range c.Variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+c.Variables[k])
	}
	return env
}

// runRunnerBuiltPreview builds p from a copy of the agent's working tree and deploys
// its output, removing the copy in every case. Build output streams to the Step log
// only — it can print the configuration's values, so it isn't returned.
func runRunnerBuiltPreview(ctx context.Context, deps DeployStaticSitePreviewDeps, p runnerBuiltPreview) (string, error) {
	if deps.BuildSite == nil {
		return "", fmt.Errorf("deployment.io can't build previews here")
	}
	copyDir, err := copyRepositoryForBuild(filepath.Join(deps.WorkDirHost, p.repository), deps.WorkDirHost, deps.LogsWriter)
	if copyDir != "" {
		defer os.RemoveAll(copyDir)
	}
	if err != nil {
		return "", fmt.Errorf("copy %s for the preview build: %w", p.repository, err)
	}
	siteDir := filepath.Join(copyDir, p.rootDirectory)
	if info, err := os.Stat(siteDir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s's root directory %q isn't a directory in %s", p.displayName(), p.site.RootDirectory, p.repository)
	}
	if err := checkInside(copyDir, siteDir); err != nil {
		return "", fmt.Errorf("%s's root directory: %w", p.displayName(), err)
	}
	if p.configuration.ConfigurationSet {
		if err := writePreviewFiles(siteDir, p.configuration.Files); err != nil {
			return "", err
		}
	}

	io.WriteString(deps.LogsWriter, previewBuildLogLine(p))
	if err := deps.BuildSite(ctx, siteDir, p.site.BuildCommand, previewBuildEnv(p.configuration), deps.LogsWriter); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("the preview build of %s was cancelled", p.displayName())
		}
		return "", fmt.Errorf("the preview build of %s failed (%s): %w. Its output is in the Step log; it isn't returned here because it can show the preview configuration", p.displayName(), p.site.BuildCommand, err)
	}

	outDir := filepath.Join(siteDir, p.publishDir)
	shown := path.Join(p.repository, p.rootDirectory, p.publishDir)
	if info, err := os.Stat(filepath.Join(outDir, "index.html")); err != nil || info.IsDir() {
		if p.detected != nil {
			return "", fmt.Errorf("the build of %s didn't produce %s/index.html. If the project writes its build somewhere else, build it yourself and call deploy_static_site_preview with publish_dir and without repository", p.displayName(), shown)
		}
		return "", fmt.Errorf("the build of %s didn't produce %s/index.html", p.displayName(), shown)
	}
	if err := checkInside(copyDir, outDir); err != nil {
		return "", fmt.Errorf("%s's publish directory: %w", p.displayName(), err)
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("the preview build of %s was cancelled", p.displayName())
	}
	out, err := deployPreviewDir(ctx, deps, p.serviceName, outDir, p.site.IsSpa)
	if err != nil {
		return "", err
	}
	out.BuiltBy = builtByDeploymentIO
	out.Configuration = configurationText(p.configuration)
	out.Settings = detectedSettingsText(p)
	return marshalResult(out)
}

// copyRepositoryForBuild copies src into a new temporary directory beside workDir —
// never inside it, so the agent can't see it — leaving out .git and node_modules at
// any depth, and every symlink whose target leaves the repository (see
// symlinkLeavesRepository and symlinkResolvesOutside), each with a line to logs naming the link's path inside the
// repository. So nothing reading the copy on the host follows a link out of it.
// Returns the copy's path (also on a copy error, so the caller removes it).
func copyRepositoryForBuild(src, workDir string, logs io.Writer) (string, error) {
	if logs == nil {
		logs = io.Discard
	}
	work := filepath.Clean(workDir)
	dst, err := os.MkdirTemp(filepath.Dir(work), filepath.Base(work)+"-preview-build-")
	if err != nil {
		return "", err
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" || d.Name() == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Mkdir(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if symlinkLeavesRepository(rel, link) || symlinkResolvesOutside(src, rel) {
				fmt.Fprintf(logs, "Preview build: left out the symlink %s, whose target is outside the repository\n", filepath.ToSlash(rel))
				return nil
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		default:
			return nil // sockets, devices, pipes: not part of a build
		}
	})
	return dst, err
}

// symlinkLeavesRepository reports whether the symlink at rel (relative to the
// repository root) with target link leaves the repository: an absolute target, or a
// relative one resolving — lexically, from the link's own directory — outside the root.
func symlinkLeavesRepository(rel, link string) bool {
	if filepath.IsAbs(link) || strings.HasPrefix(link, "/") {
		return true
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(rel), link))
	return resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator))
}

// symlinkResolvesOutside reports whether the symlink at rel (relative to the source
// repository src) leaves the repository once the links on its way are followed in src
// — which the lexical check misses for a chain of links that each stay inside, such
// as a -> . with b -> a/../outside. It walks the target one component at a time and
// never needs the target to exist: a ".." above the root counts as leaving whether or
// not anything is there now, since the copy sits in a different directory. Past a
// component missing from src (or left out of the copy: .git, node_modules), what the
// copy will hold is unknown, so any later ".." counts as leaving. An absolute target
// anywhere on the way, or too many links (a loop), counts as leaving too.
func symlinkResolvesOutside(src, rel string) bool {
	const maxLinks = 255
	var cur []string // resolved components below src
	pending := splitPath(filepath.ToSlash(rel))
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				return true
			}
			cur = cur[:len(cur)-1]
			continue
		}
		cur = append(cur, part)
		full := filepath.Join(append([]string{src}, cur...)...)
		info, err := os.Lstat(full)
		if err != nil || part == ".git" || part == "node_modules" {
			for _, rest := range pending {
				if rest == ".." {
					return true
				}
			}
			return false
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		links++
		if links > maxLinks {
			return true
		}
		link, err := os.Readlink(full)
		if err != nil || filepath.IsAbs(link) || strings.HasPrefix(link, "/") {
			return true
		}
		cur = cur[:len(cur)-1]
		pending = append(splitPath(filepath.ToSlash(link)), pending...)
	}
	return false
}

func splitPath(p string) []string {
	return strings.Split(p, "/")
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// writePreviewFiles writes each secret file at <siteDir>/<name>, as the deploy's
// checkout does, creating directories. A name that would resolve outside siteDir —
// by "..", or through a symlinked directory of the copied tree — is refused.
func writePreviewFiles(siteDir string, files []task_previews.PreviewFileV1) error {
	for _, f := range files {
		target, err := previewFilePath(siteDir, f.Name)
		if err != nil {
			return err
		}
		if err := mkdirInside(siteDir, filepath.Dir(target)); err != nil {
			return fmt.Errorf("secret file %q: %w", f.Name, err)
		}
		// Remove first, as the checkout does: never write through a symlink.
		_ = os.Remove(target)
		if err := os.WriteFile(target, []byte(f.Contents), 0o644); err != nil {
			return fmt.Errorf("write secret file %q: %w", f.Name, err)
		}
	}
	return nil
}

// mkdirInside creates dir (inside base) one directory at a time, checking each that
// already exists resolves inside base before creating anything under it — so a
// symlinked directory of the copied tree never gets directories created at its
// target.
func mkdirInside(base, dir string) error {
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return err
	}
	cur := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if _, err := os.Lstat(cur); errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		if err := checkInside(base, cur); err != nil {
			return errors.New("would be written outside the site's root directory")
		}
		if info, err := os.Stat(cur); err != nil || !info.IsDir() {
			return fmt.Errorf("%s isn't a directory", part)
		}
	}
	return nil
}

// previewFilePath is <siteDir>/<name>, refused when it isn't strictly inside siteDir.
func previewFilePath(siteDir, name string) (string, error) {
	target := filepath.Join(siteDir, name)
	rel, err := filepath.Rel(siteDir, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("secret file %q would be written outside the site's root directory", name)
	}
	return target, nil
}

// checkInside refuses p when, symlinks resolved, it isn't base or inside it.
func checkInside(base, p string) error {
	rb, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rb, rp)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s resolves outside %s", p, base)
	}
	return nil
}

// previewBuilds runs the agent run's runner-built previews in the background, one per
// service. A call starts its service's build, or attaches to the one running, and
// waits up to previewBuildCallWait. A finished build's result (or error) goes to the
// next call for its service and is then forgotten, so a later call builds again.
type previewBuilds struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	byService map[string]*previewBuild
	// byRequest maps a call's arguments to the service its build runs as, so a call
	// repeating them attaches to that build without looking up the settings again.
	byRequest map[string]string
}

type previewBuild struct {
	done   chan struct{}
	result string
	err    error
}

func newPreviewBuilds() *previewBuilds {
	ctx, cancel := context.WithCancel(context.Background())
	return &previewBuilds{ctx: ctx, cancel: cancel, byService: map[string]*previewBuild{}, byRequest: map[string]string{}}
}

// call starts or attaches to service's build (run, given a context cancelled when the
// agent run ends) and waits for it at most previewBuildCallWait.
func (b *previewBuilds) call(ctx context.Context, service string, run func(context.Context) (string, error)) (string, error) {
	return b.callFor(ctx, "", service, run, previewBuildCallWait)
}

// callFor is call waiting at most wait, remembering that request (when not "") builds
// as service.
func (b *previewBuilds) callFor(ctx context.Context, request, service string, run func(context.Context) (string, error), wait time.Duration) (string, error) {
	b.mu.Lock()
	if b.ctx.Err() != nil {
		b.mu.Unlock()
		return "", errors.New("the agent run has ended, so no preview can be built")
	}
	pb := b.byService[service]
	if pb == nil {
		pb = &previewBuild{done: make(chan struct{})}
		b.byService[service] = pb
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer close(pb.done)
			defer func() {
				if r := recover(); r != nil {
					pb.result, pb.err = "", fmt.Errorf("the preview build failed: %v", r)
				}
			}()
			pb.result, pb.err = run(b.ctx)
		}()
	}
	if request != "" {
		b.byRequest[request] = service
	}
	b.mu.Unlock()
	return b.wait(ctx, service, pb, wait)
}

// attach waits, at most wait, for the build a previous call with the same request
// started, when it is still there (running, or finished and not yet returned).
// ok=false when there is none: the caller looks up the settings and calls callFor.
func (b *previewBuilds) attach(ctx context.Context, request string, wait time.Duration) (result string, err error, ok bool) {
	b.mu.Lock()
	service, known := b.byRequest[request]
	pb := b.byService[service]
	if !known || pb == nil {
		delete(b.byRequest, request)
		b.mu.Unlock()
		return "", nil, false
	}
	b.mu.Unlock()
	result, err = b.wait(ctx, service, pb, wait)
	return result, err, true
}

// wait returns pb's result once it finishes within wait, forgetting it, or the
// "building" result.
func (b *previewBuilds) wait(ctx context.Context, service string, pb *previewBuild, wait time.Duration) (string, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-pb.done:
		b.mu.Lock()
		if b.byService[service] == pb {
			delete(b.byService, service)
		}
		b.mu.Unlock()
		return pb.result, pb.err
	case <-timer.C:
		return marshalResult(previewBuildingResult{Status: "building", Message: previewBuildingMessage})
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// stop cancels the builds still running and waits for them to finish (and remove
// their copies).
func (b *previewBuilds) stop() {
	b.mu.Lock()
	b.cancel()
	b.mu.Unlock()
	b.wg.Wait()
}
