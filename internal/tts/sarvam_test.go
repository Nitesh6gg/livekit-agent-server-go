package tts

import "testing"

// These test the generation-queue bookkeeping in isolation, without a real
// connection: push/pop/peek/reset operate on private state only. See
// docs/DECISIONS.md for why this exists — it is what stops audio from an
// interrupted turn leaking into the next one on Sarvam's call-scoped,
// non-cancellable TTS connection.

func TestPeekGenFallsBackBeforeAnySpeak(t *testing.T) {
	c := &Client{}
	if got := c.peekGen(); got != 0 {
		t.Fatalf("peekGen() on a fresh client = %d, want 0", got)
	}
}

func TestPeekGenReturnsFrontOfQueue(t *testing.T) {
	c := &Client{}
	c.pushGen(5)
	c.pushGen(6)
	if got := c.peekGen(); got != 5 {
		t.Fatalf("peekGen() = %d, want 5 (oldest pending)", got)
	}
}

func TestPopGenAdvancesTheQueueInOrder(t *testing.T) {
	c := &Client{}
	c.pushGen(5)
	c.pushGen(6)

	gen, ok := c.popGen()
	if !ok || gen != 5 {
		t.Fatalf("first popGen() = (%d, %v), want (5, true)", gen, ok)
	}
	if got := c.peekGen(); got != 6 {
		t.Fatalf("peekGen() after popping 5 = %d, want 6", got)
	}

	gen, ok = c.popGen()
	if !ok || gen != 6 {
		t.Fatalf("second popGen() = (%d, %v), want (6, true)", gen, ok)
	}
}

func TestPopGenOnEmptyQueueReturnsFalse(t *testing.T) {
	c := &Client{}
	if _, ok := c.popGen(); ok {
		t.Fatal("popGen() on an empty queue reported ok=true")
	}
}

// TestPeekGenFallsBackToLastGenBetweenFinalAndNextSpeak reproduces the gap
// between a "final" event popping the last pending generation and the next
// Speak call pushing a new one — audio should still tag as the most recent
// real generation instead of an ambiguous zero.
func TestPeekGenFallsBackToLastGenBetweenFinalAndNextSpeak(t *testing.T) {
	c := &Client{}
	c.pushGen(7)
	c.popGen() // simulates the "final" event draining the queue

	if got := c.peekGen(); got != 7 {
		t.Fatalf("peekGen() after the queue drains = %d, want 7 (lastGen fallback)", got)
	}
}

// TestResetGensDropsPendingButKeepsLastGen matches what a reconnect needs:
// "final" events owed by the old connection will never arrive on the new one,
// but a chunk that slips in before the next Speak call should still land on a
// real generation rather than falling back to 0.
func TestResetGensDropsPendingButKeepsLastGen(t *testing.T) {
	c := &Client{}
	c.pushGen(3)
	c.pushGen(4)

	c.resetGens()

	if _, ok := c.popGen(); ok {
		t.Fatal("popGen() found a pending generation after resetGens")
	}
	if got := c.peekGen(); got != 4 {
		t.Fatalf("peekGen() after reset = %d, want 4 (lastGen preserved)", got)
	}
}

// TestGenerationFilteringScenario exercises the exact sequence that produced
// the bug this queue fixes: a turn is interrupted almost immediately after its
// first Speak call, and the next turn starts before Sarvam has finished
// streaming the abandoned turn's audio.
func TestGenerationFilteringScenario(t *testing.T) {
	c := &Client{}

	// Turn 5 speaks; its audio starts arriving, tagged 5.
	c.pushGen(5)
	if got := c.peekGen(); got != 5 {
		t.Fatalf("turn 5 audio tagged %d, want 5", got)
	}

	// Barge-in: the pipeline invalidates gen 5 on its side (curGen -> 0), but
	// Sarvam has no cancel message, so more gen-5-tagged audio keeps arriving
	// client-side — this queue does not know or care about the interruption,
	// by design; filtering happens in the pipeline, not here.
	if got := c.peekGen(); got != 5 {
		t.Fatalf("stale audio still tags as %d, want 5 (queue is unaware of the pipeline's cancellation)", got)
	}

	// Turn 6 starts and speaks before Sarvam has sent gen 5's "final".
	c.pushGen(6)
	if got := c.peekGen(); got != 5 {
		t.Fatalf("audio while gen 5 is still outstanding tags as %d, want 5 (not yet turn 6's)", got)
	}

	// Sarvam finishes streaming turn 5's (abandoned) reply.
	gen, ok := c.popGen()
	if !ok || gen != 5 {
		t.Fatalf("popGen() = (%d, %v), want (5, true)", gen, ok)
	}

	// Only now does new audio correctly belong to turn 6.
	if got := c.peekGen(); got != 6 {
		t.Fatalf("audio after gen 5's final tags as %d, want 6", got)
	}
}
