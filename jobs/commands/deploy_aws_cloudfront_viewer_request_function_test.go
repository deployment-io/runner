package commands

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
)

func nonSpaViewerRequestFunctionCode(t *testing.T) string {
	t.Helper()
	key, err := parameters_enums.IsSpa.Key()
	if err != nil {
		t.Fatalf("IsSpa key: %v", err)
	}
	code, err := getViewerRequestFunctionCode(map[string]interface{}{key: false})
	if err != nil {
		t.Fatalf("getViewerRequestFunctionCode: %v", err)
	}
	return code
}

func TestViewerRequestFunctionKeepsRepeatedQueryValues(t *testing.T) {
	code := nonSpaViewerRequestFunctionCode(t)
	if !strings.Contains(code, "entry.multiValue.forEach(function(mv) {") {
		t.Errorf("function code lacks multiValue-aware buildQS:\n%s", code)
	}
}

// TestViewerRequestFunctionRedirectBehaviour runs the generated function under
// node (when installed) to check the trailing-slash redirect keeps every value
// of a repeated query key.
func TestViewerRequestFunctionRedirectBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := nonSpaViewerRequestFunctionCode(t) + `
function req(uri, qs) { return { request: { uri: uri, querystring: qs, headers: { host: { value: 'www.example.com' } } } }; }
var out = {
	repeated: handler(req('/docs/x', {
		tag: { value: 'a', multiValue: [{ value: 'a' }, { value: 'b' }] }
	})).headers.location.value,
	single: handler(req('/docs/x', { q: { value: 'x' } })).headers.location.value,
	bare: handler(req('/docs/x', {})).headers.location.value,
	dir: handler(req('/docs/x/', {})).uri
};
console.log(JSON.stringify(out));`
	got, err := exec.Command(node, "-e", script).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	want := `{"repeated":"https://www.example.com/docs/x/?tag=a&tag=b","single":"https://www.example.com/docs/x/?q=x","bare":"https://www.example.com/docs/x/","dir":"/docs/x/index.html"}`
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}
