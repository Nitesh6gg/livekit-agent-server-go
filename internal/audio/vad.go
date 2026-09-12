// Package audio provides local voice activity detection and speech
// enhancement for the agent pipeline.
//
// Both run in-process through sherpa-onnx (CGO), which bundles its own
// onnxruntime — nothing is dlopened and no separate runtime is installed.
//
// Detection uses TEN VAD rather than the provider's server-side VAD for
// barge-in: Sarvam's START_SPEECH fires on background conversation, which lets
// room noise hijack a turn, and it carries a network round trip that local
// inference does not.
//
// A caveat worth knowing when reading the logs: sherpa's TEN VAD does not
// compute the model's pitch feature, substituting zero. Measured against the
// official TEN VAD on its own test audio, that costs ~14% extra false speech
// frames. See docs/DECISIONS.md ADR-018.
package audio

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// TEN VAD runs at 16 kHz only, and is fed exactly windowSamples per inference.
// sherpa supports a window of 160 or 256 samples; 256 (16 ms) is its default
// and matches GTCRN's frame shift exactly, so the two need no re-buffering
// between them.
const (
	vadSampleRate = 16000
	windowSamples = 256
)

// segmentBufferSeconds sizes sherpa's internal speech-segment buffer. We drain
// it every frame (see Run) and never read a segment, so this only has to be
// large enough to never be the thing that fails first.
const segmentBufferSeconds = 30

// EventKind classifies a detector event.
type EventKind int

const (
	// EventSpeechStarted fires once speech has been sustained for MinSpeech.
	EventSpeechStarted EventKind = iota
	// EventSpeechStopped fires after Hangover of continuous silence.
	EventSpeechStopped
)

// String implements fmt.Stringer so events read clearly in logs.
func (k EventKind) String() string {
	switch k {
	case EventSpeechStarted:
		return "SPEECH_START"
	case EventSpeechStopped:
		return "SPEECH_END"
	}
	return "UNKNOWN"
}

// Event is one detector state change.
type Event struct{ Kind EventKind }

// DetectorConfig configures a Detector.
type DetectorConfig struct {
	// ModelPath is the vendored ten-vad.onnx. It must be sherpa's re-export:
	// the model published by TEN itself carries no metadata and is rejected.
	ModelPath string
	// SampleRate must be 16000.
	SampleRate int
	// Threshold is the speech probability (0..1) above which a frame counts as
	// speech. 0.5 is sherpa's default; higher is more conservative.
	Threshold float64
	// MinSpeech is how long speech must persist before EventSpeechStarted is
	// emitted. This is the guard against a cough or a door closing
	// interrupting the agent.
	MinSpeech time.Duration
	// Hangover is the silence required before EventSpeechStopped is emitted.
	Hangover time.Duration
}

// Detector runs TEN VAD over submitted PCM and emits speech events.
//
// Submit never blocks: it is called from the LiveKit decode goroutine, and
// inference happens on the Detector's own goroutine.
type Detector struct {
	vad      *sherpa.VoiceActivityDetector
	frameDur time.Duration

	in     chan []int16
	events chan Event

	// pending accumulates submitted samples until a full window is available.
	pending []int16
	// frame is the reusable float32 conversion buffer handed to sherpa.
	frame []float32

	// speaking is read by Speaking() from other goroutines, so it is atomic
	// even though it is only written from Run.
	speaking atomic.Bool

	dropped uint64
}

// NewDetector loads the model and prepares a detector. The caller must Close it.
func NewDetector(cfg DetectorConfig) (*Detector, error) {
	if cfg.SampleRate != vadSampleRate {
		// Silero accepted 8 kHz too; TEN VAD does not. Failing here beats
		// sherpa aborting the process from C++ on a rate it cannot serve.
		return nil, fmt.Errorf("tenvad: unsupported sample rate %d (want %d)", cfg.SampleRate, vadSampleRate)
	}
	if cfg.ModelPath == "" {
		return nil, fmt.Errorf("tenvad: no model path configured")
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.5
	}
	if cfg.MinSpeech <= 0 {
		cfg.MinSpeech = 200 * time.Millisecond
	}
	if cfg.Hangover <= 0 {
		cfg.Hangover = 550 * time.Millisecond
	}

	var c sherpa.VadModelConfig
	c.TenVad.Model = cfg.ModelPath
	c.TenVad.Threshold = float32(cfg.Threshold)
	c.TenVad.MinSpeechDuration = float32(cfg.MinSpeech.Seconds())
	c.TenVad.MinSilenceDuration = float32(cfg.Hangover.Seconds())
	c.TenVad.WindowSize = windowSamples
	// sherpa raises its own threshold to 0.9 for segments longer than this, to
	// stop a single segment growing without bound. It has no bearing on
	// barge-in, which only reads the speech/silence edge.
	c.TenVad.MaxSpeechDuration = 20
	c.SampleRate = vadSampleRate
	c.NumThreads = 1
	c.Provider = "cpu"

	vad := sherpa.NewVoiceActivityDetector(&c, segmentBufferSeconds)
	if vad == nil {
		// sherpa reports the reason on stderr and returns nil; a bad model path
		// is by far the most common cause.
		return nil, fmt.Errorf("tenvad: failed to create detector (model %q)", cfg.ModelPath)
	}

	return &Detector{
		vad:      vad,
		frameDur: time.Duration(windowSamples) * time.Second / time.Duration(vadSampleRate),
		in:       make(chan []int16, 64),
		events:   make(chan Event, 16),
		frame:    make([]float32, windowSamples),
	}, nil
}

// Events yields speech start/stop transitions.
func (d *Detector) Events() <-chan Event { return d.events }

// Submit queues PCM for detection. It never blocks; on a full queue the chunk
// is dropped, because the caller runs on the SDK's decode goroutine.
func (d *Detector) Submit(pcm []int16) {
	select {
	case d.in <- pcm:
	default:
		d.dropped++
		if d.dropped%100 == 1 {
			log.Printf("vad: detector queue full, dropped %d chunk(s)", d.dropped)
		}
	}
}

// Run drives detection until ctx is cancelled. Inference is confined to this
// goroutine, which is also why the sherpa handle needs no locking.
func (d *Detector) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pcm := <-d.in:
			d.pending = append(d.pending, pcm...)
			for len(d.pending) >= windowSamples {
				ev, ok := d.step(d.pending[:windowSamples])
				d.pending = d.pending[windowSamples:]
				if !ok {
					continue
				}
				select {
				case d.events <- ev:
				case <-ctx.Done():
					return
				}
			}
			// Keep the backing array from growing without bound.
			if len(d.pending) == 0 && cap(d.pending) > 4*windowSamples {
				d.pending = nil
			}
		}
	}
}

// step runs one window through the detector, returning an event when the
// speech state changes.
func (d *Detector) step(window []int16) (Event, bool) {
	for i, s := range window {
		d.frame[i] = float32(s) / 32768
	}
	d.vad.AcceptWaveform(d.frame)

	// sherpa buffers every completed speech segment for callers that want the
	// audio back. Barge-in only needs the speech/silence edge, so discard them
	// — left undrained this grows for the whole length of the call.
	for !d.vad.IsEmpty() {
		d.vad.Pop()
	}

	now := d.vad.IsSpeech()
	if now == d.speaking.Load() {
		return Event{}, false
	}
	d.speaking.Store(now)
	if now {
		return Event{Kind: EventSpeechStarted}, true
	}
	return Event{Kind: EventSpeechStopped}, true
}

// Speaking reports whether the detector currently considers the caller to be
// speaking.
func (d *Detector) Speaking() bool { return d.speaking.Load() }

// Close releases the detector. Safe to call more than once.
func (d *Detector) Close() error {
	if d.vad != nil {
		sherpa.DeleteVoiceActivityDetector(d.vad)
		d.vad = nil
	}
	return nil
}
