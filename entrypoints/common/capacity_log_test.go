package common

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestCapacitySpellLogsOncePerSpell pins the whole point of the pair: a job
// blocked behind a long agent container is re-offered every 10s, so a runner
// that logged every refusal would emit ~360 identical lines an hour for a
// single queued job. One line must mark the start of a saturated spell and one
// the end, however many refusals happen in between.
func TestCapacitySpellLogsOncePerSpell(t *testing.T) {
	capacityLogged.Store(false)

	if !enteringCapacitySpell() {
		t.Fatal("the first refusal must log; an operator has no other way to tell " +
			"'runner at capacity' apart from 'job never picked up'")
	}
	for i := 0; i < 100; i++ {
		if enteringCapacitySpell() {
			t.Fatalf("refusal %d logged again mid-spell; that is the noise this exists to stop", i+2)
		}
	}

	if !leavingCapacitySpell() {
		t.Fatal("recovery must log once so the spell has a visible end")
	}
	if leavingCapacitySpell() {
		t.Error("recovery logged twice; admissions after the first must stay quiet")
	}

	// A second spell is a genuinely new event and must be reported again,
	// otherwise the runner goes silent for the rest of its life after one
	// episode.
	if !enteringCapacitySpell() {
		t.Error("a later spell must log; the flag has to reset on recovery")
	}
}

// TestCapacitySpellIsRaceSafe covers the reason these are compare-and-swaps
// rather than a load followed by a store. Job workers run concurrently, and a
// read-then-write leaves a window where two of them both see "not saturated"
// and both conclude they were first -- producing exactly the duplicate line
// this change removes. Note that -race alone will NOT catch that: Load and
// Store are each atomic, so there is no data race to report, only a logic one.
// Hence the start barrier and the repeats: the goroutines are released
// together, which is what makes the window observable.
func TestCapacitySpellIsRaceSafe(t *testing.T) {
	const (
		workers  = 64
		attempts = 500
	)

	for attempt := 0; attempt < attempts; attempt++ {
		capacityLogged.Store(false)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var logged atomic.Int64

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if enteringCapacitySpell() {
					logged.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()

		if got := logged.Load(); got != 1 {
			t.Fatalf("attempt %d: %d of %d concurrent refusals logged, want exactly 1",
				attempt, got, workers)
		}
	}
}
