package commands

import (
	"sort"
	"strconv"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

// THE STACK'S WIRING.
//
// deployment-server's StackWiringV1 says, per production static-site and
// web-service deployment, which of the site's variables holds the API's address
// (and the URL's path) and which of the API's variables lists the site's origin
// — names only. A stack-path QA round uses it to build each site preview to call
// the backend preview of the same change, and to let that backend preview
// accept the site preview's origin.

// qaStackPair is one (site, backend) pair of a stack-path round, in site-major
// work-dir order.
type qaStackPair struct {
	site, backend *qaStackPreview
	// link is the first link of the reply that applies to the pair, or nil.
	link *task_previews.StackLinkV1
	// reason is why the pair isn't connected, "" while it can be.
	reason string
	// singleOrigin is, for reason "single origin", the backend's variable the
	// pair lost to an earlier site.
	singleOrigin string
	// originVariables are the backend's variables the site's origin was set in.
	originVariables []string
}

// linked reports whether the pair's link names a site variable holding the
// backend's address.
func (p *qaStackPair) linked() bool {
	return p.link != nil && len(p.link.APIURLVariables) > 0
}

// connected reports whether the site's preview is built to call the backend's.
func (p *qaStackPair) connected() bool {
	return p.linked() && p.reason == ""
}

// pairStack makes every (site, backend) pair of the round and its applying
// link. lookupFailed marks every pair "lookup failed".
func pairStack(sites, backends []*qaStackPreview, links []task_previews.StackLinkV1, lookupFailed bool) []*qaStackPair {
	var pairs []*qaStackPair
	for _, site := range sites {
		for _, backend := range backends {
			p := &qaStackPair{site: site, backend: backend}
			switch {
			case lookupFailed:
				p.reason = qaWiringLookupFailed
			case backend.planErr != "":
				p.reason = qaWiringBackendFailed
			default:
				p.link = applyingLink(links, site, backend)
				if !p.linked() {
					p.reason = qaWiringNoLink
				}
			}
			pairs = append(pairs, p)
		}
	}
	return pairs
}

// applyingLink is the first link for site's and backend's repositories and the
// deployments their previews are built from. A detected site, or a preview that
// couldn't be planned, has no deployment and so no link.
func applyingLink(links []task_previews.StackLinkV1, site, backend *qaStackPreview) *task_previews.StackLinkV1 {
	if site.deploymentName == "" || backend.deploymentName == "" {
		return nil
	}
	for i := range links {
		l := &links[i]
		if l.FrontendRepository == site.repository && l.APIRepository == backend.repository &&
			l.Frontend == site.deploymentName && l.API == backend.deploymentName {
			return l
		}
	}
	return nil
}

// stackOrigins is the value of each of backend's allowed-origin variables: the
// origins of every site linked to it whose link names the variable, in
// work-dir order, joined by the variable's separator. A variable whose
// separator is "" (production lists a single origin) gets the first site's
// origin only; each later site is not connected to backend ("single origin").
// A site without an origin (its distribution couldn't be reserved) sets none.
func stackOrigins(pairs []*qaStackPair, backend *qaStackPreview) map[string]string {
	type variable struct {
		separator string
		origins   []string
	}
	vars := map[string]*variable{}
	for _, p := range pairs {
		if p.backend != backend || !p.connected() || p.site.origin == "" {
			continue
		}
		taken := ""
		for _, v := range p.link.AllowedOriginVariables {
			if got := vars[v.Name]; got != nil && got.separator == "" {
				taken = v.Name
				break
			}
		}
		if taken != "" {
			p.reason, p.singleOrigin = qaWiringSingleOrigin, taken
			continue
		}
		for _, v := range p.link.AllowedOriginVariables {
			got := vars[v.Name]
			if got == nil {
				got = &variable{separator: v.Separator}
				vars[v.Name] = got
			}
			got.origins = append(got.origins, p.site.origin)
			p.originVariables = append(p.originVariables, v.Name)
		}
	}
	out := map[string]string{}
	for name, v := range vars {
		out[name] = strings.Join(v.origins, v.separator)
	}
	return out
}

// stackAPIAddresses is the value of each of site's API-address variables: for
// every backend it is connected to, https://<the backend preview's domain>
// followed by the production URL's path. The first backend in work-dir order
// sets a variable two backends would.
func stackAPIAddresses(pairs []*qaStackPair, site *qaStackPreview) map[string]string {
	out := map[string]string{}
	for _, p := range pairs {
		if p.site != site || !p.connected() {
			continue
		}
		for _, v := range p.link.APIURLVariables {
			if _, set := out[v.Name]; !set {
				out[v.Name] = strings.TrimRight(p.backend.url, "/") + urlPath(v.Path)
			}
		}
	}
	return out
}

// urlPath is path with a leading "/" when it has one at all.
func urlPath(path string) string {
	if path == "" || strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

// wiringOutput is the pair as the round records it: names only, its
// repositories by entry name (name maps a work-dir name to it).
func (p *qaStackPair) wiringOutput(name func(dir string) string) qaWiringOutput {
	out := qaWiringOutput{Frontend: name(p.site.repository), API: name(p.backend.repository), Connected: p.connected(), Reason: p.reason}
	switch {
	case out.Connected:
		for _, v := range p.link.APIURLVariables {
			out.FrontendVariables = append(out.FrontendVariables, v.Name)
		}
		out.APIVariables = append(out.APIVariables, p.originVariables...)
	case p.reason == qaWiringSingleOrigin:
		out.APIVariables = []string{p.singleOrigin}
	}
	out.FrontendVariables = sortedUnique(out.FrontendVariables)
	out.APIVariables = sortedUnique(out.APIVariables)
	return out
}

func sortedUnique(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := names[:1]
	for _, n := range names[1:] {
		if n != out[len(out)-1] {
			out = append(out, n)
		}
	}
	return out
}

// qaStackRepositories is every repository of static and backends once, in
// work-dir order.
func qaStackRepositories(static, backends []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, dir := range append(append([]string{}, static...), backends...) {
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return workDirIndex(out[i]) < workDirIndex(out[j]) })
	return out
}

// workDirIndex is the index a work-dir name "<idx>-<name>" starts with.
func workDirIndex(dir string) int {
	prefix, _, _ := strings.Cut(dir, "-")
	i, err := strconv.Atoi(prefix)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return i
}

// sortedKeys is m's keys, sorted: the variable names of a wired set.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
