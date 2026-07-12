package agenttools

import "testing"

// TestResolvePublishDir covers the path mapping + the traversal guard: whatever
// the agent passes for publish_dir, the result must stay under workDirHost.
func TestResolvePublishDir(t *testing.T) {
	const work = "/srv/work-host"
	cases := []struct {
		name, in, want string
	}{
		{"plain", "dist", "/srv/work-host/dist"},
		{"nested", "web/build", "/srv/work-host/web/build"},
		{"dot slash", "./dist", "/srv/work-host/dist"},
		{"trim spaces", "  dist  ", "/srv/work-host/dist"},
		{"container abs", "/work/dist", "/srv/work-host/dist"},
		{"container abs nested", "/work/web/build", "/srv/work-host/web/build"},
		{"traversal neutralized", "../../etc/passwd", "/srv/work-host/etc/passwd"},
		{"container abs traversal", "/work/../../secret", "/srv/work-host/secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvePublishDir(work, c.in); got != c.want {
				t.Fatalf("resolvePublishDir(%q, %q) = %q, want %q", work, c.in, got, c.want)
			}
		})
	}
}

func TestTailString(t *testing.T) {
	if got := tailString([]byte("hello"), 10); got != "hello" {
		t.Fatalf("short (no truncation): got %q, want %q", got, "hello")
	}
	if got := tailString([]byte("hello world"), 5); got != "…world" {
		t.Fatalf("truncated: got %q, want %q", got, "…world")
	}
	// max lands mid multibyte rune → advance to the next rune boundary.
	if got := tailString([]byte("aéb"), 2); got != "…b" {
		t.Fatalf("split-rune boundary: got %q, want %q", got, "…b")
	}
	// max lands exactly on a rune boundary → keep it whole.
	if got := tailString([]byte("aé"), 2); got != "…é" {
		t.Fatalf("clean boundary: got %q, want %q", got, "…é")
	}
}

// TestDeriveServiceName covers the repo-aware key: strip the numeric idx prefix,
// keep org/repo/subdir, sanitize to dashes; same-type services get distinct keys.
func TestDeriveServiceName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0-deployment-io/dashboard/dist", "deployment-io-dashboard-dist"},
		{"/work/0-deployment-io/dashboard/dist", "deployment-io-dashboard-dist"},
		{"0-org/app/build", "org-app-build"},   // monorepo service A
		{"0-org/admin/dist", "org-admin-dist"}, // monorepo service B — distinct key
		{"dist", "dist"},                       // no idx/repo prefix
		{"", "static-site"},                    // nothing usable → fallback
	}
	for _, c := range cases {
		if got := deriveServiceName(c.in); got != c.want {
			t.Errorf("deriveServiceName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
