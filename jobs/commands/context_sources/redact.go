package context_sources

import (
	"encoding/json"
	"fmt"
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
		// Redact runs before the pack is JSON-marshaled, so Data is still the source's native
		// Go value — often a typed struct or []struct, which scrub's map/slice switch would
		// skip. Normalize it to its generic wire shape first so scrub can actually walk it,
		// whatever the source's type.
		generic, err := toGeneric(pack.Artifacts[i].Data)
		if err != nil {
			return fmt.Errorf("redact: normalize artifact %q: %w", pack.Artifacts[i].Name, err)
		}
		pack.Artifacts[i].Data = scrub(generic)
	}
	return nil
}

// toGeneric round-trips a (possibly typed) value through JSON into the generic
// map[string]interface{} / []interface{} / scalar shape scrub understands — the same form the
// value takes on the wire and after the server unmarshals it.
func toGeneric(v interface{}) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var g interface{}
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, err
	}
	return g, nil
}

// scrub recursively walks the generic map/slice/scalar shape (produced by toGeneric), replacing
// the value under any secret-ish key with a placeholder.
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
