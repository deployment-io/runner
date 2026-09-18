package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/sessions"
)

// suggestionSink records what the forwarder sent and can be made to fail.
type suggestionSink struct {
	sent    []sessions.SetRepoSuggestionDtoV1
	orgs    []string
	failNow bool
}

func (s *suggestionSink) send(dto sessions.SetRepoSuggestionDtoV1, orgID string) error {
	if s.failNow {
		return fmt.Errorf("rpc down")
	}
	s.sent = append(s.sent, dto)
	s.orgs = append(s.orgs, orgID)
	return nil
}

// newSuggestionForwarder wires a forwarder at a temp path with an injected sink.
func newSuggestionForwarder(t *testing.T) (*repoSuggestionForwarder, *suggestionSink, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "repo-suggestion.json")
	sink := &suggestionSink{}
	return &repoSuggestionForwarder{
		path: path, orgID: "org-1", jobID: "job-1", logsWriter: io.Discard, send: sink.send,
	}, sink, path
}

func writeSuggestionFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

const oneRepoSuggestion = `{"repositories":[{"name":"deployment-io/kit","reason":"the model lives here","confidence":"high"}]}`

// An agentbox that never writes the file leaves the forwarder idle: no RPC, no
// log noise. This is what keeps an old image paired with the new runner silent.
func TestRepoSuggestionForwarder_NoFileIsSilent(t *testing.T) {
	rf, sink, _ := newSuggestionForwarder(t)
	rf.tick()
	rf.tick()
	if len(sink.sent) != 0 {
		t.Errorf("forwarded without a file: %+v", sink.sent)
	}
}

func TestRepoSuggestionForwarder_ForwardsOnChangeOnly(t *testing.T) {
	rf, sink, path := newSuggestionForwarder(t)

	writeSuggestionFile(t, path, oneRepoSuggestion)
	rf.tick()
	if len(sink.sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sink.sent))
	}
	got := sink.sent[0]
	if got.JobID != "job-1" || sink.orgs[0] != "org-1" {
		t.Errorf("not bound to the session job/org: %+v (org %q)", got, sink.orgs[0])
	}
	if len(got.Repositories) != 1 {
		t.Fatalf("repositories = %d, want 1", len(got.Repositories))
	}
	r := got.Repositories[0]
	if r.Name != "deployment-io/kit" || r.Reason != "the model lives here" || r.Confidence != "high" {
		t.Errorf("payload = %+v", r)
	}

	// Unchanged content is not re-forwarded, however many ticks run.
	rf.tick()
	rf.tick()
	if len(sink.sent) != 1 {
		t.Fatalf("unchanged content was re-forwarded: %d sends", len(sink.sent))
	}

	// A new suggestion is forwarded wholesale.
	writeSuggestionFile(t, path, `{"repositories":[{"name":"deployment-io/runner","confidence":"low"}]}`)
	rf.tick()
	if len(sink.sent) != 2 || sink.sent[1].Repositories[0].Name != "deployment-io/runner" {
		t.Fatalf("changed content not forwarded: %+v", sink.sent)
	}
}

// A failed RPC must leave lastContent alone so the next tick retries — the
// suggestion is only stored server-side, so a dropped forward loses it.
func TestRepoSuggestionForwarder_RetriesAfterAFailedRPC(t *testing.T) {
	rf, sink, path := newSuggestionForwarder(t)
	writeSuggestionFile(t, path, oneRepoSuggestion)

	sink.failNow = true
	rf.tick()
	if len(sink.sent) != 0 {
		t.Fatalf("a failing send should record nothing: %+v", sink.sent)
	}

	sink.failNow = false
	rf.tick() // same content, but the last attempt failed → retry
	if len(sink.sent) != 1 {
		t.Fatalf("the next tick did not retry: %d sends", len(sink.sent))
	}
	rf.tick() // now it has landed, so it stops
	if len(sink.sent) != 1 {
		t.Errorf("a landed forward was repeated: %d sends", len(sink.sent))
	}
}

// Unparseable content is skipped without sending and without wedging: a later
// well-formed write still forwards.
func TestRepoSuggestionForwarder_SkipsUnparseableContent(t *testing.T) {
	rf, sink, path := newSuggestionForwarder(t)

	writeSuggestionFile(t, path, `{"repositories":`)
	rf.tick()
	if len(sink.sent) != 0 {
		t.Fatalf("unparseable content was forwarded: %+v", sink.sent)
	}

	writeSuggestionFile(t, path, oneRepoSuggestion)
	rf.tick()
	if len(sink.sent) != 1 {
		t.Errorf("a good write after a bad one did not forward: %d sends", len(sink.sent))
	}
}

// The drain tick after the container exits catches a suggestion written between
// the last ticker pass and the exit.
func TestRepoSuggestionForwarder_DrainsOnExit(t *testing.T) {
	rf, sink, path := newSuggestionForwarder(t)

	rf.tick() // ticker pass before the agent wrote anything
	writeSuggestionFile(t, path, oneRepoSuggestion)

	rf.tick() // the post-exit drain
	if len(sink.sent) != 1 {
		t.Fatalf("the drain tick did not forward the final suggestion: %d sends", len(sink.sent))
	}
}

// An empty repositories list still forwards; the server decides what to do with
// it (it rejects it rather than blanking a good suggestion). The runner stays a
// dumb pipe so the policy lives in one place.
func TestRepoSuggestionForwarder_ForwardsEmptyListVerbatim(t *testing.T) {
	rf, sink, path := newSuggestionForwarder(t)
	writeSuggestionFile(t, path, `{"repositories":[]}`)
	rf.tick()
	if len(sink.sent) != 1 || len(sink.sent[0].Repositories) != 0 {
		t.Errorf("sent = %+v, want one empty payload", sink.sent)
	}
}

// The prompt the runner ships has to describe the block agentbox extracts, and
// must not point at a context path that doesn't exist.
func TestPlanModePromptDescribesTheRepoSuggestionBlock(t *testing.T) {
	for _, want := range []string{
		"<repo-suggestion>",
		"</repo-suggestion>",
		`{"repositories":[{"name":"org/repo"`,
		"at most ONE <repo-suggestion> block per message",
		"/work/context",
		"index.md",
	} {
		if !strings.Contains(planModePrompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	// repos/_catalog.md does not exist — the context pack materializes index.md
	// plus per-pack artifacts, so naming it would send the agent at nothing.
	if strings.Contains(planModePrompt, "_catalog.md") {
		t.Error("the prompt must not name a context file that is never materialized")
	}
}
