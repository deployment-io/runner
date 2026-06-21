package context_sources

import (
	"strings"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
)

// secretKeySubstrings are field-name fragments whose VALUES must never enter a pack. The
// connectors already emit metadata/structure only; this backstop nulls any value that slips
// through under a secret-ish key, enforcing the "never secret values" invariant runner-side.
var secretKeySubstrings = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key", "accesskey", "access_key",
	"privatekey", "private_key", "credential", "connectionstring", "connection_string",
	"authorization",
}

const redactedPlaceholder = "[redacted]"

// Redact is the runner-side chokepoint every pack passes through before it is written to
// JobOutput and persisted to the control plane. Sources are the first line of redaction (they
// emit names/structure, never values; secret-name lists summarized; identifiers marked for
// field-encryption); this is the backstop and the single place to add cross-source rules.
//
// The four-bucket classification (exclude / field-encrypt / redact-down / plain) is applied at
// the source; field-encryption of the identifier tier is opt-in hardening layered in as
// connectors land. For now this pass scrubs any value that appears under a secret-ish key in
// the structured Artifacts.
func Redact(pack *context_pack.Pack) error {
	for i := range pack.Artifacts {
		pack.Artifacts[i].Data = scrub(pack.Artifacts[i].Data)
	}
	return nil
}

// scrub recursively walks structured data, replacing the value under any secret-ish key with a
// placeholder. It operates on the generic map/slice shapes that JSON round-trips into.
func scrub(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if isSecretKey(k) {
				t[k] = redactedPlaceholder
				continue
			}
			t[k] = scrub(val)
		}
		return t
	case []interface{}:
		for i := range t {
			t[i] = scrub(t[i])
		}
		return t
	default:
		return v
	}
}

func isSecretKey(key string) bool {
	lk := strings.ToLower(key)
	for _, s := range secretKeySubstrings {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}
