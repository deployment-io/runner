package commands

import (
	"testing"

	"github.com/deployment-io/deployment-runner-kit/sessions"
)

// TestRotateOutputBatch covers the chunk/final grouping that drives the
// at-least-once message forwarder: consecutive chunks share one MessageID and
// "final" closes it.
func TestRotateOutputBatch(t *testing.T) {
	const job = "job1"
	batch, end := rotateOutputBatch([]outputRec{
		{Type: "chunk", Text: "Hel"}, {Type: "chunk", Text: "lo"}, {Type: "final"},
	}, job, "")

	if len(batch) != 3 {
		t.Fatalf("want 3 deltas, got %d", len(batch))
	}
	id := batch[0].MessageID
	if id == "" {
		t.Fatal("first chunk should mint a MessageID")
	}
	if batch[1].MessageID != id {
		t.Error("chunks of one message must share the MessageID")
	}
	if batch[0].Content != "Hel" || batch[1].Content != "lo" {
		t.Errorf("chunk content wrong: %q %q", batch[0].Content, batch[1].Content)
	}
	if !batch[2].IsDone || batch[2].MessageID != id || batch[2].Content != "" {
		t.Error("final should close the same message with no content")
	}
	if end != "" {
		t.Errorf("endMsgID should reset after a final, got %q", end)
	}
	if batch[0].JobID != job {
		t.Errorf("jobID = %q, want %q", batch[0].JobID, job)
	}
}

// TestRotateOutputBatch_CarryOver covers an in-flight message split across ticks
// (no final yet): the id carries over and the next tick continues it.
func TestRotateOutputBatch_CarryOver(t *testing.T) {
	batch, end := rotateOutputBatch([]outputRec{{Type: "chunk", Text: "a"}, {Type: "chunk", Text: "b"}}, "j", "")
	if end == "" {
		t.Fatal("an in-flight message id should carry over when there's no final")
	}
	if batch[0].MessageID != end || batch[1].MessageID != end {
		t.Error("trailing chunks should share the carried id")
	}

	batch2, end2 := rotateOutputBatch([]outputRec{{Type: "chunk", Text: "c"}, {Type: "final"}}, "j", end)
	if batch2[0].MessageID != end {
		t.Error("continuation should reuse the carried MessageID")
	}
	if !batch2[1].IsDone || batch2[1].MessageID != end {
		t.Error("final should close the carried message")
	}
	if end2 != "" {
		t.Errorf("id should reset after the final, got %q", end2)
	}
}

func TestRotateOutputBatch_TwoMessages(t *testing.T) {
	batch, _ := rotateOutputBatch([]outputRec{
		{Type: "chunk", Text: "one"}, {Type: "final"},
		{Type: "chunk", Text: "two"}, {Type: "final"},
	}, "j", "")
	if len(batch) != 4 {
		t.Fatalf("want 4 deltas, got %d", len(batch))
	}
	if batch[0].MessageID == batch[2].MessageID {
		t.Error("separate messages must have distinct MessageIDs")
	}
}

func TestRotateOutputBatch_LoneFinalIgnored(t *testing.T) {
	batch, end := rotateOutputBatch([]outputRec{{Type: "final"}}, "j", "")
	if len(batch) != 0 {
		t.Errorf("a final with no in-flight message should produce nothing, got %d", len(batch))
	}
	if end != "" {
		t.Errorf("no in-flight id expected, got %q", end)
	}
}

// TestFilterUndelivered covers the inputPump dedupe that makes the server's
// inclusive ($gte) input query safe to re-return same-second turns.
func TestFilterUndelivered(t *testing.T) {
	seen := map[string]bool{"b": true}
	out := filterUndelivered([]sessions.UserMessageDtoV1{{ID: "a"}, {ID: "b"}, {ID: "c"}}, seen)
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "c" {
		t.Errorf("want [a c], got %v", out)
	}
	if got := filterUndelivered([]sessions.UserMessageDtoV1{{ID: "b"}}, seen); len(got) != 0 {
		t.Errorf("already-seen message should be filtered, got %v", got)
	}
	if got := filterUndelivered(nil, seen); len(got) != 0 {
		t.Errorf("nil input should yield empty, got %v", got)
	}
}
