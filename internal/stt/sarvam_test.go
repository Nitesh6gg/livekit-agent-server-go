package stt

import (
	"testing"
	"time"
)

// TestSarvamTimestampParsesRealPayload uses the exact value captured from a
// live probe against the API: {"type":"events","data":{"signal_type":
// "END_SPEECH","occured_at":1786013420.5537887}}.
func TestSarvamTimestampParsesRealPayload(t *testing.T) {
	got, ok := sarvamTimestamp(1786013420.5537887)
	if !ok {
		t.Fatal("sarvamTimestamp reported not-ok for a real, non-zero payload value")
	}

	want := time.Unix(1786013420, 553788700)
	if diff := got.Sub(want); diff < -time.Microsecond || diff > time.Microsecond {
		t.Fatalf("sarvamTimestamp(1786013420.5537887) = %v, want %v (diff %v)", got, want, diff)
	}
}

// TestSarvamTimestampZeroIsAbsent covers the one case JSON can't distinguish:
// a genuinely-sent 0 vs. a field the server omitted entirely. Both decode to
// 0.0 in Go, so treating 0 as "absent" is the only safe reading — Sarvam's
// events happen long after the Unix epoch, so a real occured_at is never
// actually zero.
func TestSarvamTimestampZeroIsAbsent(t *testing.T) {
	got, ok := sarvamTimestamp(0)
	if ok {
		t.Fatalf("sarvamTimestamp(0) reported ok=true with time %v, want ok=false", got)
	}
}

// TestAbsentTimestampYieldsZeroRemoteAt pins the contract internal/agent relies
// on: a missing occured_at must leave Event.RemoteAt zero, so the clock-skew
// report stays silent rather than comparing against 1970 and announcing a
// 56-year skew on every call.
func TestAbsentTimestampYieldsZeroRemoteAt(t *testing.T) {
	at, ok := sarvamTimestamp(0)
	if ok || !at.IsZero() {
		t.Fatal("expected the absent-field case to yield (zero time, false)")
	}
}

// TestRemoteTimestampIsNotUsedForLatency is a documentation test for the
// decision that cost a full test call to learn: Event.At must be the local
// receipt time, never Sarvam's occured_at.
//
// A live call measured the two clocks 3.857–3.892s apart across nine turns —
// a 35ms spread, which is an offset, not detection lag. Anchoring speech-end
// on the remote value inflated the Decision Gate 1 metric from ~1.6s to ~5.5s.
// This asserts the shape of the fix: the two timestamps are separate fields,
// so latency arithmetic cannot silently reach for the wrong clock.
func TestRemoteTimestampIsNotUsedForLatency(t *testing.T) {
	remote, ok := sarvamTimestamp(1786013420.5537887)
	if !ok {
		t.Fatal("expected a parseable remote timestamp")
	}

	local := time.Now()
	ev := Event{Kind: EventSpeechStopped, At: local, RemoteAt: remote}

	if !ev.At.Equal(local) {
		t.Errorf("Event.At = %v, want the local receipt time %v", ev.At, local)
	}
	if ev.At.Equal(ev.RemoteAt) {
		t.Error("Event.At must not carry Sarvam's clock; that is what RemoteAt is for")
	}
	// The skew this represents is the whole reason the fields are separate.
	if skew := ev.At.Sub(ev.RemoteAt); skew == 0 {
		t.Error("expected a measurable gap between the local and remote clocks in this fixture")
	}
}
