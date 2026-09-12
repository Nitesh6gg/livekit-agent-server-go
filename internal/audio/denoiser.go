package audio

import (
	"fmt"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// denoiseSampleRate is the only rate GTCRN is trained for.
const denoiseSampleRate = 16000

// Denoiser runs streaming GTCRN speech enhancement over caller audio.
//
// What it does and does not do is worth being precise about, because the two
// are easy to confuse. GTCRN suppresses *noise* — fans, traffic, hum, keyboard
// clatter. It is trained to preserve speech, so a second person talking across
// the room is not removed: their voice is speech, and the model has no way to
// know it is not the caller's. Rejecting an interfering talker is a different
// problem (target-speaker extraction or diarization), and this is not that.
// See docs/DECISIONS.md ADR-018.
//
// Process is called synchronously from the LiveKit decode goroutine so that the
// denoised stream reaches STT in order and without a second queue. GTCRN is
// cheap enough for that — roughly 0.07 real-time, about 1 ms of work per 16 ms
// frame — but it is not free, and it is the reason enhancement is opt-in.
//
// The model carries exactly one frame of algorithmic delay, measured rather
// than assumed: the first Process call that completes a frame returns nothing,
// and every frame after it returns a full one. Output therefore trails input by
// one frame — 256 samples, 16 ms at 16 kHz — for the life of the call. That
// delay lands on the STT path when enhancement is on.
//
// A Denoiser is not safe for concurrent use.
type Denoiser struct {
	sd    *sherpa.OnlineSpeechDenoiser
	shift int

	// pending accumulates submitted samples until a full frame is available.
	pending []int16
	// frame is the reusable float32 conversion buffer handed to sherpa.
	frame []float32
}

// NewDenoiser loads the GTCRN model. The caller must Close it.
func NewDenoiser(modelPath string, sampleRate int) (*Denoiser, error) {
	if sampleRate != denoiseSampleRate {
		return nil, fmt.Errorf("gtcrn: unsupported sample rate %d (want %d)", sampleRate, denoiseSampleRate)
	}
	if modelPath == "" {
		return nil, fmt.Errorf("gtcrn: no model path configured")
	}

	var c sherpa.OnlineSpeechDenoiserConfig
	c.Model.Gtcrn.Model = modelPath
	c.Model.NumThreads = 1
	c.Model.Provider = "cpu"

	sd := sherpa.NewOnlineSpeechDenoiser(&c)
	if sd == nil {
		return nil, fmt.Errorf("gtcrn: failed to create denoiser (model %q)", modelPath)
	}

	shift := sd.FrameShiftInSamples()
	if shift <= 0 {
		sherpa.DeleteOnlineSpeechDenoiser(sd)
		return nil, fmt.Errorf("gtcrn: model reported invalid frame shift %d", shift)
	}

	return &Denoiser{
		sd:    sd,
		shift: shift,
		frame: make([]float32, shift),
	}, nil
}

// FrameShift is the number of samples GTCRN consumes per inference. Audio is
// buffered to a multiple of it, so this is also the granularity at which
// Process can return anything.
func (d *Denoiser) FrameShift() int { return d.shift }

// Process denoises pcm, returning whatever whole frames are ready.
//
// The result is freshly allocated on every call. That is deliberate: callers
// hand this straight to the VAD, which consumes it on another goroutine, so a
// reused buffer would be overwritten underneath the detector. The allocation is
// negligible next to the inference that produced it.
//
// The result is frequently shorter than the input, and empty until a full frame
// has accumulated.
func (d *Denoiser) Process(pcm []int16) []int16 {
	d.pending = append(d.pending, pcm...)

	var out []int16
	for len(d.pending) >= d.shift {
		for i, s := range d.pending[:d.shift] {
			d.frame[i] = float32(s) / 32768
		}
		d.pending = d.pending[d.shift:]

		enhanced := d.sd.Run(d.frame, denoiseSampleRate)
		if enhanced == nil {
			continue
		}
		if out == nil {
			out = make([]int16, 0, len(enhanced.Samples))
		}
		for _, s := range enhanced.Samples {
			out = append(out, clampToInt16(s))
		}
	}

	if len(d.pending) == 0 && cap(d.pending) > 4*d.shift {
		d.pending = nil
	}
	return out
}

// clampToInt16 converts a normalised sample, saturating rather than wrapping.
// Enhancement can push a sample past full scale, and wrapping turns that into
// an audible click.
func clampToInt16(s float32) int16 {
	v := s * 32768
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// Close releases the denoiser. Safe to call more than once.
func (d *Denoiser) Close() error {
	if d.sd != nil {
		sherpa.DeleteOnlineSpeechDenoiser(d.sd)
		d.sd = nil
	}
	return nil
}
