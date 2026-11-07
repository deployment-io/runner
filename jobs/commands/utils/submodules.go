package utils

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	format "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// submoduleSource is where a submodule is fetched from and whether the
// provider token may be sent there.
type submoduleSource struct {
	// URL is the resolved URL, without userinfo. It is what the submodule's
	// origin is set to and fetched from.
	URL string
	// Host is the URL's host, the only part of it that is logged.
	Host string
	// Trusted is true when URL is https on parentURL's host: only then does the
	// fetch carry the token.
	Trusted bool
}

// resolveSubmoduleURL resolves a submodule's configured URL (rawURL, from
// .gitmodules) against parentURL, the plain clone URL of the repository that
// holds it, and decides whether the token may be sent there.
//
//   - "./x" and "../x" are relative to parentURL: its path joined with rawURL,
//     on parentURL's host, as go-git and git resolve them. Any number of ".."
//     stays on that host.
//   - An scp-style URL (git@host:path) is ssh.
//   - file:// URLs and local paths (absolute, or relative without "./"/"../")
//     are refused.
//   - Userinfo is dropped from the result.
//   - Trusted when, and only when, the result is https on parentURL's host
//     (case-insensitive, ":443" being no port).
func resolveSubmoduleURL(parentURL, submodulePath, rawURL string) (submoduleSource, error) {
	parent, err := url.Parse(parentURL)
	if err != nil {
		return submoduleSource{}, fmt.Errorf("submodule %s: error parsing the parent repository's URL: %s", submodulePath, err)
	}
	localErr := fmt.Errorf("submodule %s: local paths and file:// URLs aren't supported", submodulePath)

	var resolved *url.URL
	switch {
	case strings.HasPrefix(rawURL, "./") || strings.HasPrefix(rawURL, "../"):
		u := *parent
		u.Path = path.Join(parent.Path, rawURL)
		u.RawPath = ""
		u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
		resolved = &u
	case isSCPLikeURL(rawURL):
		host, p, _ := strings.Cut(rawURL[strings.Index(rawURL, "@")+1:], ":")
		resolved = &url.URL{Scheme: "ssh", Host: host, Path: "/" + strings.TrimPrefix(p, "/")}
	default:
		if filepath.IsAbs(rawURL) || path.IsAbs(rawURL) {
			return submoduleSource{}, localErr
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return submoduleSource{}, fmt.Errorf("submodule %s: error parsing its URL: %s", submodulePath, err)
		}
		if u.Scheme == "" || strings.EqualFold(u.Scheme, "file") {
			return submoduleSource{}, localErr
		}
		resolved = u
	}
	resolved.User = nil
	resolved.Scheme = strings.ToLower(resolved.Scheme)

	return submoduleSource{
		URL:     resolved.String(),
		Host:    resolved.Host,
		Trusted: resolved.Scheme == "https" && strings.EqualFold(parent.Scheme, "https") && sameHost(resolved, parent),
	}, nil
}

// isSCPLikeURL reports whether rawURL is git's scp-like syntax,
// [user@]host:path: no "://", and a ":" before the first "/".
func isSCPLikeURL(rawURL string) bool {
	if strings.Contains(rawURL, "://") {
		return false
	}
	colon := strings.Index(rawURL, ":")
	if colon <= 0 {
		return false
	}
	slash := strings.Index(rawURL, "/")
	return slash < 0 || colon < slash
}

// sameHost compares two https URLs' hosts case-insensitively, ":443" being the
// same as no port.
func sameHost(a, b *url.URL) bool {
	return strings.EqualFold(a.Hostname(), b.Hostname()) && httpsPort(a) == httpsPort(b)
}

func httpsPort(u *url.URL) string {
	if p := u.Port(); p != "" && p != "443" {
		return p
	}
	return ""
}

// hostOf is u's host for logs and errors: never the rest of the URL.
func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
}

// UpdateSubmodules initializes and updates, recursively, every submodule of
// repository at the commit the parent records. parentURL is the repository's
// plain clone URL. Each submodule is fetched from its URL resolved by
// resolveSubmoduleURL against its parent's URL, and with the token only when
// that URL is https on its parent's host (and its parent was itself fetched
// with the token); anywhere else it is fetched without credentials.
// Returns an error if it fails to initialize or update any submodule.
func UpdateSubmodules(repository *git.Repository, parentURL, username, repoProviderToken string, logsWriter io.Writer) error {
	return updateSubmodules(context.Background(), repository, parentURL, username, repoProviderToken, logsWriter)
}

// updateSubmodules is UpdateSubmodules bounded by ctx, for a clone with a
// deadline: every submodule fetch aborts when ctx is done.
func updateSubmodules(ctx context.Context, repository *git.Repository, parentURL, username, repoProviderToken string, logsWriter io.Writer) error {
	u := submoduleUpdater{ctx: ctx, username: username, token: repoProviderToken, logsWriter: logsWriter}
	return u.updateAll(repository, submoduleParent{url: parentURL, trusted: true}, 1)
}

// submoduleUpdater holds what every submodule update of one checkout shares.
type submoduleUpdater struct {
	ctx        context.Context
	username   string
	token      string
	logsWriter io.Writer
}

// submoduleParent is the repository holding a submodule: its resolved URL,
// and whether it was fetched with the token. Below a submodule fetched
// without it, none is.
type submoduleParent struct {
	url     string
	trusted bool
}

// updateAll updates repository's submodules, depth levels below the top
// repository, and theirs down to git.DefaultSubmoduleRecursionDepth levels.
func (u submoduleUpdater) updateAll(repository *git.Repository, parent submoduleParent, depth int) error {
	wt, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("error updating submodules: %s", err)
	}

	submodules, err := wt.Submodules()
	if err != nil {
		return fmt.Errorf("error updating submodules: %s", err)
	}

	for _, submodule := range submodules {
		r, self, err := u.update(submodule, parent)
		if err != nil {
			return err
		}
		if depth < int(git.DefaultSubmoduleRecursionDepth) {
			if err := u.updateAll(r, self, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// update fetches and checks out one submodule of parent. It returns the
// submodule's repository and the submodule as the parent of its own.
func (u submoduleUpdater) update(submodule *git.Submodule, parent submoduleParent) (*git.Repository, submoduleParent, error) {
	subPath := submodule.Config().Path
	source, err := resolveSubmoduleURL(parent.url, subPath, submodule.Config().URL)
	if err != nil {
		return nil, submoduleParent{}, err
	}
	self := submoduleParent{url: source.URL, trusted: parent.trusted && source.Trusted}

	// ignore err if submodule is already initialized
	if err := submodule.Init(); err != nil && !errors.Is(err, git.ErrSubmoduleAlreadyInitialized) {
		return nil, self, fmt.Errorf("error updating submodules: %s", err)
	}
	r, err := submodule.Repository()
	if err != nil {
		return nil, self, fmt.Errorf("error updating submodule: %s: %s", subPath, err)
	}
	// Fetch from the resolved URL and nothing else: replace whatever go-git
	// computed, or an earlier Step or an agent left, in
	// .git/modules/<name>/config — including url.<x>.insteadOf rules, which
	// go-git would otherwise apply to origin's URL.
	if err := setOriginURL(r, source.URL, true); err != nil {
		return nil, self, fmt.Errorf("error updating submodule: %s: %s", subPath, err)
	}

	var auth transport.AuthMethod
	if self.trusted {
		auth = &http.BasicAuth{Username: u.username, Password: u.token}
	}
	err = submodule.UpdateContext(u.ctx, &git.SubmoduleUpdateOptions{RecurseSubmodules: git.NoRecurseSubmodules, Auth: auth})
	if err != nil {
		// %w keeps transport.ErrAuthenticationRequired visible to the
		// clone retry wrappers' token refresh.
		if self.trusted {
			return nil, self, fmt.Errorf("error updating submodule: %s: %w", subPath, err)
		}
		return nil, self, fmt.Errorf("error updating submodule %s: %s, so it was fetched without credentials: %w",
			subPath, untrustedReason(source, parent), err)
	}
	if self.trusted {
		io.WriteString(u.logsWriter, fmt.Sprintf("Submodule %s updated successfully\n", subPath))
	} else {
		reason := fmt.Sprintf("it isn't on %s over https", hostOf(parent.url))
		if source.Trusted {
			reason = "its parent submodule was"
		}
		io.WriteString(u.logsWriter, fmt.Sprintf("Submodule %s updated successfully (from %s, fetched without credentials because %s)\n",
			subPath, source.Host, reason))
	}
	return r, self, nil
}

// untrustedReason says, naming hosts only, why a submodule from source under
// parent was fetched without the token.
func untrustedReason(source submoduleSource, parent submoduleParent) string {
	if !source.Trusted {
		return fmt.Sprintf("it's on %s, not %s over https", source.Host, hostOf(parent.url))
	}
	return fmt.Sprintf("it's on %s, under a submodule fetched without credentials", source.Host)
}

// setOriginURL makes origin's URLs exactly [originURL], keeping its fetch
// refspecs. The remote is replaced rather than edited: go-git writes back a
// remote's pre-insteadOf URLs when an insteadOf rule matched them, so an
// in-place edit would not stick. dropURLRules also removes every
// url.<x>.insteadOf rule, for a repository whose fetch can't name its URL.
func setOriginURL(repository *git.Repository, originURL string, dropURLRules bool) error {
	cfg, err := repository.Config()
	if err != nil {
		return err
	}
	remote := &config.RemoteConfig{
		Name:  git.DefaultRemoteName,
		URLs:  []string{originURL},
		Fetch: []config.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
	}
	if old, ok := cfg.Remotes[git.DefaultRemoteName]; ok && len(old.Fetch) > 0 {
		remote.Fetch = old.Fetch
	}
	cfg.Remotes[git.DefaultRemoteName] = remote
	if dropURLRules {
		cfg.URLs = map[string]*config.URL{}
	}
	return repository.Storer.SetConfig(cfg)
}

// ScrubRemoteToken resets origin's URL to the plain clone URL, and drops the
// userinfo from every remote URL under .git/modules, so neither .git/config
// nor any submodule's config holds a credential. Clones no longer put the
// token in a URL; this cleans a repository directory reused from a runner
// that did. Call it at the end of a checkout, after the runner's own
// clone/fetch. Pushes and fetches name their URL and authenticate through
// Auth, independent of the remote URL. A missing origin is not an error.
func ScrubRemoteToken(repository *git.Repository, tokenlessURL string) error {
	cfg, err := repository.Config()
	if err != nil {
		return fmt.Errorf("error reading repo config to scrub token: %s", err)
	}
	if _, ok := cfg.Remotes[git.DefaultRemoteName]; ok {
		if err := setOriginURL(repository, tokenlessURL, false); err != nil {
			return fmt.Errorf("error setting scrubbed remote config: %s", err)
		}
	}
	storage, ok := repository.Storer.(*filesystem.Storage)
	if !ok {
		return nil
	}
	if err := scrubModuleConfigs(storage.Filesystem()); err != nil {
		return fmt.Errorf("error scrubbing submodule configs: %s", err)
	}
	return nil
}

// scrubModuleConfigs drops the userinfo from the remote URLs of every
// modules/**/config under the git directory dotGit.
func scrubModuleConfigs(dotGit billy.Filesystem) error {
	if _, err := dotGit.Lstat("modules"); err != nil {
		return nil
	}
	return util.Walk(dotGit, "modules", func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Name() != "config" || !info.Mode().IsRegular() {
			return nil
		}
		return scrubConfigFile(dotGit, p)
	})
}

// scrubConfigFile drops the userinfo from every remote's url and pushurl in
// the git config file p. It edits the file's raw options, not go-git's parsed
// config: parsing applies url.<x>.insteadOf rules, which can move a
// credential out of the parsed URL's userinfo while the file keeps it.
func scrubConfigFile(fs billy.Filesystem, p string) error {
	data, err := util.ReadFile(fs, p)
	if err != nil {
		return err
	}
	raw := format.New()
	if err := format.NewDecoder(bytes.NewReader(data)).Decode(raw); err != nil {
		// Not a git config (a file named "config" in a worktree under
		// modules): nothing to scrub.
		return nil
	}
	changed := false
	for _, section := range raw.Sections {
		if !section.IsName("remote") {
			continue
		}
		for _, sub := range section.Subsections {
			for _, opt := range sub.Options {
				if !opt.IsKey("url") && !opt.IsKey("pushurl") {
					continue
				}
				if scrubbed := stripUserinfo(opt.Value); scrubbed != opt.Value {
					opt.Value = scrubbed
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil
	}
	var out bytes.Buffer
	if err := format.NewEncoder(&out).Encode(raw); err != nil {
		return err
	}
	return util.WriteFile(fs, p, out.Bytes(), 0o644)
}

// stripUserinfo returns rawURL without its userinfo, or rawURL unchanged when
// it isn't a URL with one.
func stripUserinfo(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL
	}
	u.User = nil
	return u.String()
}
