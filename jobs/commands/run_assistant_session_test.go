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

// TestRotateOutputBatch_TurnEnd covers the turn boundary: it forwards as its
// own control update (fresh MessageID, IsDone, TurnEnd, no content) after the
// turn's messages.
func TestRotateOutputBatch_TurnEnd(t *testing.T) {
	batch, end := rotateOutputBatch([]outputRec{
		{Type: "chunk", Text: "answer"}, {Type: "final"}, {Type: "turn_end"},
	}, "j", "")
	if len(batch) != 3 {
		t.Fatalf("want 3 deltas, got %d", len(batch))
	}
	te := batch[2]
	if !te.TurnEnd || !te.IsDone || te.Content != "" {
		t.Errorf("turn-end delta wrong: %+v", te)
	}
	if te.MessageID == "" || te.MessageID == batch[0].MessageID {
		t.Error("turn-end must carry its own fresh MessageID")
	}
	if end != "" {
		t.Errorf("no in-flight id expected after a turn end, got %q", end)
	}
}

// TestRotateOutputBatch_TurnEndClosesHalfOpenMessage pins the defensive close:
// a turn_end arriving while a message is mid-stream (no final yet) finalizes
// it first so it isn't lost past the boundary.
func TestRotateOutputBatch_TurnEndClosesHalfOpenMessage(t *testing.T) {
	batch, end := rotateOutputBatch([]outputRec{
		{Type: "chunk", Text: "orphan"}, {Type: "turn_end"},
	}, "j", "")
	if len(batch) != 3 {
		t.Fatalf("want chunk + synthesized final + turn-end, got %d: %+v", len(batch), batch)
	}
	if !batch[1].IsDone || batch[1].MessageID != batch[0].MessageID || batch[1].TurnEnd {
		t.Errorf("half-open message should be closed before the boundary: %+v", batch[1])
	}
	if !batch[2].TurnEnd {
		t.Errorf("boundary should follow the synthesized final: %+v", batch[2])
	}
	if end != "" {
		t.Errorf("no carry-over expected, got %q", end)
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
