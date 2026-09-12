// Package metrics records the per-turn timings Decision Gate 1 turns on, and
// exposes them for scraping.
//
// speechEnd (the reference point for endpointLag and responseLatency) is the
// local receipt time of Sarvam's END_SPEECH — see docs/DECISIONS.md ADR-017's
// amendment. An earlier version of this package also emitted a "hangover
// adjusted" figure, subtracting the local VAD's configured hangover on the
// theory that SPEECH_END fires a hangover after the caller actually stopped
// talking. That was correct back when speechEnd came from the *local* VAD,
// whose hangover really did delay it — but once speechEnd moved to Sarvam's
// own END_SPEECH, the subtraction was removing a number that has nothing to do
// with Sarvam's endpointing delay, silently discounting the reported latency
// by however long VAD_HANGOVER_MS happened to be set to. Confirmed in a live
// call: it understated the caller's wait by over a second on one turn. Removed
// rather than recalculated, because there is no local measurement that safely
// substitutes — correlating Sarvam's speech-end against a separately clocked
// local VAD signal is the exact mistake ADR-016 made twice already.
//
// endpointLag (Sarvam's own speech-end-to-transcript lag) is still reported,
// but only as its own number — not folded into any "corrected" total. If it
// is large on a turn, that turn's total is genuinely slow, not misleadingly
// slow.
//
// FirstAudioOut is when PCM is handed to LiveKit, not when it is audible.
// Queueing and network sit downstream, so this is a floor on mouth-to-ear
// latency, not a measurement of it.
//
// See docs/METRICS.md.
package metrics

import (
	"log"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// latencyBuckets span the range a voice turn plausibly occupies, with
// resolution around the 800ms target and the 1500ms hard fail.
var latencyBuckets = []float64{
	0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.65, 0.8, 1.0, 1.25, 1.5, 2.0, 3.0, 5.0,
}

// interruptionBuckets are far finer: this path is in-process and should be
// sub-millisecond.
var interruptionBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25,
}

var (
	endpointLag = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agent_endpoint_lag_seconds",
		Help:    "Speech end to transcript delivered.",
		Buckets: latencyBuckets,
	})
	llmTTFT = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agent_llm_ttft_seconds",
		Help:    "Transcript to first LLM token.",
		Buckets: latencyBuckets,
	})
	ttsTTFB = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agent_tts_ttfb_seconds",
		Help:    "First sentence sent to TTS, to first audio handed to LiveKit.",
		Buckets: latencyBuckets,
	})
	responseLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agent_response_latency_seconds",
		Help:    "Speech end to first agent audio handed to LiveKit. The Decision Gate 1 number.",
		Buckets: latencyBuckets,
	})
	interruptionLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agent_interruption_latency_seconds",
		Help:    "Barge-in decision to playout queue flushed, in-process only.",
		Buckets: interruptionBuckets,
	})

	turnsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agent_turns_total",
		Help: "Turns that produced audio.",
	})
	turnsCancelledTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agent_turns_cancelled_total",
		Help: "Turns abandoned before producing audio.",
	})
	interruptionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agent_interruptions_total",
		Help: "Barge-ins that actually interrupted agent speech.",
	})
)

// MustRegister adds the agent collectors to reg. Go runtime and process
// collectors are the caller's responsibility.
func MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(
		endpointLag, llmTTFT, ttsTTFB,
		responseLatency, interruptionLatency,
		turnsTotal, turnsCancelledTotal, interruptionsTotal,
	)
}

// ObserveInterruption records a completed barge-in.
func ObserveInterruption(d time.Duration) {
	interruptionsTotal.Inc()
	interruptionLatency.Observe(d.Seconds())
}

// Turn accumulates one turn's milestones. All methods are safe for concurrent
// use and idempotent: only the first call for a milestone counts, since the
// pipeline records some of them from more than one goroutine.
type Turn struct {
	gen uint64

	mu        sync.Mutex
	speechEnd time.Time // zero if no speech-end event preceded this turn
	transcript,
	llmFirst,
	ttsFirst,
	audioOut time.Time
	closed bool
}

// NewTurn starts timing a turn. speechEnd may be zero, in which case the
// speech-end-relative figures are omitted.
func NewTurn(gen uint64, speechEnd time.Time) *Turn {
	return &Turn{
		gen:        gen,
		speechEnd:  speechEnd,
		transcript: time.Now(),
	}
}

// LLMFirstToken marks the first token off the LLM stream.
func (t *Turn) LLMFirstToken() { t.mark(&t.llmFirst) }

// TTSFirstSend marks the first sentence handed to TTS.
func (t *Turn) TTSFirstSend() { t.mark(&t.ttsFirst) }

func (t *Turn) mark(field *time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && field.IsZero() {
		*field = time.Now()
	}
}

// FirstAudioOut marks the first audio handed to LiveKit and closes the turn,
// recording it. Later calls do nothing.
func (t *Turn) FirstAudioOut() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed || !t.audioOut.IsZero() {
		t.mu.Unlock()
		return
	}
	t.audioOut = time.Now()
	t.closed = true
	gen, speechEnd, transcript := t.gen, t.speechEnd, t.transcript
	llmFirst, ttsFirst, audioOut := t.llmFirst, t.ttsFirst, t.audioOut
	t.mu.Unlock()

	turnsTotal.Inc()

	fields := []any{gen}
	format := "metrics: turn=%d"

	if !llmFirst.IsZero() {
		d := llmFirst.Sub(transcript)
		llmTTFT.Observe(d.Seconds())
		format += " llm_ttft_ms=%d"
		fields = append(fields, d.Milliseconds())
	}
	if !ttsFirst.IsZero() {
		d := audioOut.Sub(ttsFirst)
		ttsTTFB.Observe(d.Seconds())
		format += " tts_ttfb_ms=%d"
		fields = append(fields, d.Milliseconds())
	}
	if !speechEnd.IsZero() {
		lag := transcript.Sub(speechEnd)
		endpointLag.Observe(lag.Seconds())
		format += " endpoint_ms=%d"
		fields = append(fields, lag.Milliseconds())

		total := audioOut.Sub(speechEnd)
		responseLatency.Observe(total.Seconds())
		format += " total_ms=%d"
		fields = append(fields, total.Milliseconds())
	}
	log.Printf(format, fields...)
}

// Cancel closes the turn without recording latencies, for turns abandoned
// before they produced audio. Counting these separately keeps interrupted turns
// from silently improving the latency percentiles.
func (t *Turn) Cancel() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	turnsCancelledTotal.Inc()
}
