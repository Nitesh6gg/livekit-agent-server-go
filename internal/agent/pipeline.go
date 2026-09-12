// Package agent orchestrates the real-time conversation pipeline: caller audio
// -> Sarvam STT -> Gemini -> Sarvam TTS -> agent audio.
//
// Barge-in is driven by the local TEN VAD when one is supplied, because
// Sarvam's server-side START_SPEECH fires on background conversation and lets
// room noise hijack a turn. Sarvam's signals remain the fallback when local VAD
// is unavailable, and its END_SPEECH still marks the end of a caller turn.
package agent

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go2market/go-agent-worker/internal/audio"
	"github.com/go2market/go-agent-worker/internal/livekit"
	"github.com/go2market/go-agent-worker/internal/llm"
	"github.com/go2market/go-agent-worker/internal/metrics"
	"github.com/go2market/go-agent-worker/internal/stt"
	"github.com/go2market/go-agent-worker/internal/tts"
)

// Pipeline wires the providers together for one call.
type Pipeline struct {
	stt     *stt.Client
	llm     *llm.Client
	tts     *tts.Client
	session *livekit.Session
	history *History

	// vad, when non-nil, owns barge-in decisions instead of Sarvam's VAD.
	vad *audio.Detector

	// vadProven turns true on the local VAD's first event. Until then Sarvam's
	// START_SPEECH is still honoured: a local VAD that loads but never fires
	// would otherwise leave the call with no barge-in path at all, and the
	// agent talks straight over the caller.
	vadProven atomic.Bool

	mu         sync.Mutex
	cancelTurn context.CancelFunc
	turnGen    uint64

	// curGen is the only generation whose TTS audio playbackLoop will write to
	// the room. Set on every startTurn; cleared (0, matching no real
	// generation) on every barge-in. Sarvam's TTS connection is call-scoped and
	// has no "stop synthesizing" message (see internal/tts), so audio left over
	// from a cancelled turn keeps arriving after a new turn begins — this is
	// what actually discards it, checked at the moment of playback rather than
	// once at interrupt time. Matches LiveKit Agents' SpeechHandle, which
	// filters stale audio the same way instead of reconnecting the TTS
	// provider on every interruption.
	curGen atomic.Uint64

	// allow gates whether decoded TTS audio is forwarded to the room at all,
	// independent of generation — false for the brief window between a
	// barge-in and the next turn actually starting.
	allow atomic.Bool

	// ttsSampleRate is the rate of the PCM handed to WritePlayback, used to
	// convert written sample counts into playout duration.
	ttsSampleRate int

	// lastSarvamSpeechEnd is the speech-end reference used for latency metrics,
	// in unix nanos. It is the *local* time Sarvam's END_SPEECH was received.
	//
	// The ordering argument is what makes it safe, and it is unchanged from
	// ADR-017: Sarvam's own END_SPEECH always precedes its own matching
	// transcript on the same ordered connection, processed by this one
	// goroutine, so there is no correlation to guess at. That is what fixed
	// ADR-016's two opposite-direction bugs (a stale value from a previous
	// utterance, then a backfill from a newer one).
	//
	// What changed is the *clock*, not the ordering. ADR-017 took this from
	// Sarvam's `occured_at`, on the theory that a provider timestamp is
	// authoritative. A live call disproved that twice over: the two clocks are
	// ~3.87s apart, which inflated every speech-end figure by that much, and
	// even corrected, `occured_at` is Sarvam's detection time rather than the
	// acoustic end of speech. Local receipt is on the same clock as everything
	// it is subtracted from, which is the property that actually matters.
	lastSarvamSpeechEnd atomic.Int64

	// skewWarned makes the clock-skew report fire once per call rather than
	// once per utterance: it is a property of the connection, not the turn.
	skewWarned atomic.Bool

	// curTurn is the turn currently being timed. First-audio is observed from
	// playbackLoop, which has no other way to reach it.
	curTurn atomic.Pointer[metrics.Turn]

	// playoutEndsAt is when queued agent audio will finish being heard, in unix
	// nanos.
	//
	// Tracking *when audio was written* is not good enough: TTS delivers a
	// 13-second reply in about two seconds, so a write timestamp goes stale
	// while most of the reply is still queued and audible — and the agent then
	// believes it is silent during exactly the long replies worth interrupting.
	// Accumulating duration instead keeps this accurate.
	playoutEndsAt atomic.Int64
}

// playoutGrace bridges brief gaps between TTS chunks arriving for the same
// reply, so playout is not declared finished a moment before more audio lands.
const playoutGrace = 200 * time.Millisecond

// minTranscriptRunes drops transcript fragments too short to be a real
// utterance.
const minTranscriptRunes = 2

// New builds a pipeline. The clients must already be connected. vad may be nil,
// in which case barge-in falls back to Sarvam's server-side VAD. ttsSampleRate
// must match the rate of the PCM the TTS client emits.
func New(s *stt.Client, l *llm.Client, t *tts.Client, sess *livekit.Session, vad *audio.Detector, ttsSampleRate int, systemPrompt string) *Pipeline {
	return &Pipeline{
		stt:           s,
		llm:           l,
		tts:           t,
		session:       sess,
		vad:           vad,
		ttsSampleRate: ttsSampleRate,
		history:       NewHistory(systemPrompt),
	}
}

// Run drives the pipeline until ctx is cancelled or the STT stream ends.
func (p *Pipeline) Run(ctx context.Context) {
	go p.playbackLoop(ctx)

	// Local VAD events are consumed on their own goroutine. They are the only
	// barge-in trigger when present, so they do not need to be ordered against
	// the transcript stream — unlike Sarvam's signals, which do (see below).
	if p.vad != nil {
		go p.vadLoop(ctx)
		log.Print("agent: barge-in via Sarvam VAD until local TEN VAD produces its first event")
	} else {
		log.Print("agent: barge-in driven by Sarvam server-side VAD (no local VAD)")
	}

	// STT events arrive on one ordered channel on purpose. Selecting over
	// separate VAD and transcript channels reorders them, letting a
	// START_SPEECH cancel the very turn its own transcript just started — which
	// wedges the agent permanently, since every later turn is then killed by
	// the previous utterance's signal.
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-p.stt.Events():
			if !ok {
				return
			}
			switch ev.Kind {
			case stt.EventSpeechStarted:
				// Sarvam fires this on background conversation, so local VAD
				// takes over — but only once it has shown it actually produces
				// events. Until then this is the only barge-in path.
				if p.vad == nil || !p.vadProven.Load() {
					p.onBargeIn()
				}
			case stt.EventSpeechStopped:
				// Always the metrics reference, regardless of which source
				// drives barge-in: Sarvam's own END_SPEECH always precedes its
				// own matching transcript on this same ordered connection, so
				// there is nothing to correlate — see lastSarvamSpeechEnd.
				p.lastSarvamSpeechEnd.Store(ev.At.UnixNano())
				p.reportClockSkew(ev)
				p.onSpeechStopped()
			case stt.EventTranscript:
				text := strings.TrimSpace(ev.Text)
				if len([]rune(text)) < minTranscriptRunes {
					if text != "" {
						log.Printf("agent: ignoring short transcript %q", text)
					}
					continue
				}
				p.startTurn(ctx, text)
			}
		}
	}
}

// vadLoop turns local speech detection into barge-in.
func (p *Pipeline) vadLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-p.vad.Events():
			if !ok {
				return
			}
			log.Printf("vad: %s", ev.Kind)
			if p.vadProven.CompareAndSwap(false, true) {
				log.Print("agent: local VAD confirmed live — Sarvam START_SPEECH no longer used for barge-in")
			}
			// Speech-end for metrics purposes always comes from Sarvam's own
			// timestamp (see lastSarvamSpeechEnd), not from local VAD — local
			// VAD leads Sarvam by ~200ms, which made it look like the better
			// reference, but a local timestamp still has to be correlated
			// against Sarvam's independently-paced transcript delivery, and
			// that correlation was the actual source of both prior bugs.
			if ev.Kind == audio.EventSpeechStarted {
				p.onBargeIn()
			}
		}
	}
}

// playbackGapWarn is the threshold for logging a gap between consecutive
// audio writes within the same reply. Sentences within a reply stream as
// separate Speak calls (see handleTurn), so some gap is normal; this flags one
// large enough to be audible as a stutter.
const playbackGapWarn = 200 * time.Millisecond

// playbackLoop forwards decoded TTS audio to the room while playback is
// allowed and the chunk belongs to the current generation.
func (p *Pipeline) playbackLoop(ctx context.Context) {
	var streakGen, streakCount uint64
	var streakStart time.Time
	var lastWriteAt time.Time
	var lastWriteGen uint64

	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-p.tts.Audio():
			if !ok {
				return
			}
			if !p.allow.Load() {
				continue
			}
			if chunk.Gen != p.curGen.Load() {
				// Leftover audio from an interrupted turn, delivered after the
				// fact — Sarvam kept synthesizing it regardless of our own
				// cancellation. Dropping it here, not just at interrupt time,
				// is what actually closes the leak. Tracked per-gen so a run of
				// drops for the same mistagged generation reports as one
				// event with its span and count, not one log line per chunk.
				if chunk.Gen != streakGen || streakStart.IsZero() {
					streakGen, streakStart, streakCount = chunk.Gen, time.Now(), 0
				}
				streakCount++
				continue
			}
			if !streakStart.IsZero() {
				log.Printf("agent: resumed gen=%d after dropping %d stale gen=%d chunk(s) over %s",
					chunk.Gen, streakCount, streakGen, time.Since(streakStart))
				streakStart = time.Time{}
			}
			if !lastWriteAt.IsZero() && lastWriteGen == chunk.Gen {
				if gap := time.Since(lastWriteAt); gap > playbackGapWarn {
					log.Printf("agent: playback gap of %s within turn gen=%d — likely audible as a stutter", gap, chunk.Gen)
				}
			}
			p.extendPlayout(len(chunk.PCM))
			// Closes out the turn's latency measurement; idempotent, so
			// only the first chunk of a reply counts.
			p.curTurn.Load().FirstAudioOut()
			if err := p.session.WritePlayback(chunk.PCM); err != nil {
				log.Printf("playback write error: %v", err)
			}
			lastWriteAt, lastWriteGen = time.Now(), chunk.Gen
		}
	}
}

// onBargeIn cancels the active turn and flushes queued agent audio immediately.
func (p *Pipeline) onBargeIn() {
	start := time.Now()
	if !p.isSpeaking() {
		// Nothing to interrupt. Muting here would silence the *next* reply for
		// no reason, which is what happens when background chatter trips the
		// provider's VAD while the agent is idle.
		log.Print("agent: barge-in ignored — agent not speaking")
		return
	}

	p.allow.Store(false)
	// No real generation is valid until the next startTurn. Generation
	// filtering (playbackLoop, checked per chunk) is the always-on safety net
	// for stray audio Sarvam keeps streaming after a cancelled turn. But it
	// depends on Sarvam's "final" event to know when a stale generation is
	// actually done, and that event has been observed taking up to 9.5s in a
	// live call — long enough to misattribute a fresh reply's real audio as
	// stale and drop it (see docs/DECISIONS.md ADR-017). Reaching this point
	// means the agent genuinely was speaking, so disconnect now: matches
	// pipecat's `should_reconnect = bot_speaking or tts_started` policy,
	// removing the ambiguity at the source instead of racing it.
	p.curGen.Store(0)
	p.tts.Disconnect()
	p.mu.Lock()
	if p.cancelTurn != nil {
		p.cancelTurn()
		p.cancelTurn = nil
	}
	p.mu.Unlock()

	// Drop the room's playout queue and opportunistically clear whatever TTS
	// audio is already buffered — a courtesy that frees channel capacity
	// promptly; the generation check above is what guarantees correctness for
	// anything that arrives afterward.
	p.session.FlushPlayback()
	p.drainAudio()
	p.playoutEndsAt.Store(0)

	// An abandoned turn must not be recorded as a latency sample, or
	// interruptions would quietly improve the percentiles.
	if t := p.curTurn.Swap(nil); t != nil {
		t.Cancel()
	}
	metrics.ObserveInterruption(time.Since(start))
	log.Print("agent: barge-in — interrupted agent speech")
}

// extendPlayout advances the projected end of playout by the duration the given
// number of samples will take to be heard.
func (p *Pipeline) extendPlayout(samples int) {
	if samples <= 0 || p.ttsSampleRate <= 0 {
		return
	}
	d := int64(time.Duration(samples) * time.Second / time.Duration(p.ttsSampleRate))
	now := time.Now().UnixNano()
	for {
		cur := p.playoutEndsAt.Load()
		// Queued audio plays after whatever is already pending; if the queue has
		// drained, it starts now.
		base := cur
		if base < now {
			base = now
		}
		if p.playoutEndsAt.CompareAndSwap(cur, base+d) {
			return
		}
	}
}

// isSpeaking reports whether the agent is mid-reply, counting both an active
// LLM stream and audio still playing out after it finished.
func (p *Pipeline) isSpeaking() bool {
	p.mu.Lock()
	turnActive := p.cancelTurn != nil
	p.mu.Unlock()
	if turnActive {
		return true
	}
	end := p.playoutEndsAt.Load()
	return end != 0 && time.Now().UnixNano() < end+int64(playoutGrace)
}

// skewReportThreshold is how far apart the two clocks must be before it is
// worth saying so. Sarvam's timestamp is its detection moment, so a small
// positive gap is normal and expected; this is set well above that.
const skewReportThreshold = 250 * time.Millisecond

// reportClockSkew logs the gap between Sarvam's clock and ours, once per call.
//
// This exists because its absence cost a whole test call: with speech-end
// anchored on Sarvam's `occured_at`, a ~3.87s clock offset silently inflated
// every latency figure, and the only reason it was caught at all is that
// endpoint_ms sat at ~4000ms while its own components swung by 600ms. Nothing
// reported the skew, so nothing could have flagged it earlier. It is no longer
// load-bearing — latency maths is local-only now — but a drifting provider
// clock is worth seeing rather than inferring.
func (p *Pipeline) reportClockSkew(ev stt.Event) {
	if ev.RemoteAt.IsZero() || p.skewWarned.Load() {
		return
	}
	skew := ev.At.Sub(ev.RemoteAt)
	if skew < 0 {
		skew = -skew
	}
	if skew < skewReportThreshold {
		return
	}
	if p.skewWarned.CompareAndSwap(false, true) {
		log.Printf("stt: Sarvam clock is %s from ours (its occured_at=%s, received %s) — "+
			"latency metrics are local-clock only, so this is informational",
			ev.At.Sub(ev.RemoteAt), ev.RemoteAt.Format(time.RFC3339Nano), ev.At.Format(time.RFC3339Nano))
	}
}

// onSpeechStopped notes the end of a caller utterance.
//
// It deliberately does not touch the playback gate or the audio buffer. An
// earlier version drained queued TTS audio here, which discarded the remainder
// of a reply that was still playing. Playback is re-enabled by startTurn, and
// onBargeIn now only mutes when there is genuinely something to interrupt, so
// no recovery step is needed.
func (p *Pipeline) onSpeechStopped() {
	log.Print("agent: caller speech ended")
}

// drainAudio opportunistically discards whatever TTS audio is already
// buffered. Not required for correctness — playbackLoop's generation check
// handles audio that arrives after this runs — but keeps the channel from
// carrying a backlog of chunks it will just end up dropping one by one.
func (p *Pipeline) drainAudio() {
	for {
		select {
		case <-p.tts.Audio():
		default:
			return
		}
	}
}

// startTurn cancels any in-flight turn and begins a new one for userText.
func (p *Pipeline) startTurn(ctx context.Context, userText string) {
	p.mu.Lock()
	if p.cancelTurn != nil {
		p.cancelTurn()
	}
	turnCtx, cancel := context.WithCancel(ctx)
	p.cancelTurn = cancel
	p.turnGen++
	gen := p.turnGen
	p.mu.Unlock()

	p.history.AddUser(userText)
	p.curGen.Store(gen)
	p.allow.Store(true)

	// Safe to read directly, no correlation needed: this transcript and the
	// EventSpeechStopped that set lastSarvamSpeechEnd both come through
	// p.stt.Events() on this same goroutine, in the order Sarvam sent them —
	// and Sarvam always sends END_SPEECH before the transcript it belongs to.
	var speechEnd time.Time
	if ns := p.lastSarvamSpeechEnd.Load(); ns != 0 {
		speechEnd = time.Unix(0, ns)
	}
	turn := metrics.NewTurn(gen, speechEnd)
	if prev := p.curTurn.Swap(turn); prev != nil {
		prev.Cancel() // superseded before it produced audio
	}
	log.Printf("agent: turn %d start — user: %q", gen, userText)

	go p.handleTurn(turnCtx, cancel, gen, turn)
}

// handleTurn streams the LLM reply, chunks it into sentences for TTS, and
// records the final reply in history.
func (p *Pipeline) handleTurn(turnCtx context.Context, cancel context.CancelFunc, gen uint64, turn *metrics.Turn) {
	// Clear cancelTurn when this turn ends so onSpeechStopped can tell an idle
	// agent from one that is mid-reply.
	defer func() {
		cancel()
		p.mu.Lock()
		if p.turnGen == gen {
			p.cancelTurn = nil
		}
		p.mu.Unlock()
	}()

	tokens, errc := p.llm.Stream(turnCtx, p.history.Snapshot())

	var reply, sentence strings.Builder
	flush := func() {
		s := strings.TrimSpace(sentence.String())
		sentence.Reset()
		if s == "" {
			return
		}
		turn.TTSFirstSend()
		log.Printf("agent: turn %d speak: %q", gen, s)
		if err := p.tts.Speak(s, gen); err != nil {
			log.Printf("tts speak error: %v", err)
		}
	}

	for {
		select {
		case <-turnCtx.Done():
			log.Printf("agent: turn %d cancelled", gen)
			return
		case err, ok := <-errc:
			if ok && err != nil {
				log.Printf("llm stream error: %v", err)
			}
			errc = nil // disable this case once drained/closed
		case tok, ok := <-tokens:
			if !ok {
				flush()
				if r := strings.TrimSpace(reply.String()); r != "" {
					p.history.AddModel(r)
					log.Printf("agent: turn %d complete — reply: %q", gen, r)
				}
				return
			}
			turn.LLMFirstToken()
			reply.WriteString(tok)
			sentence.WriteString(tok)
			if endsSentence(sentence.String()) {
				flush()
			}
		}
	}
}

// endsSentence reports whether the buffered text ends on a sentence boundary,
// including the Devanagari danda (।) used in Hindi.
func endsSentence(s string) bool {
	s = strings.TrimRight(s, " \t")
	if s == "" {
		return false
	}
	r := []rune(s)
	switch r[len(r)-1] {
	case '.', '!', '?', '\n', '।':
		return true
	}
	return false
}
