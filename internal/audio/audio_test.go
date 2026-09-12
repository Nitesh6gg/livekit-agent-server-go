package audio

import (
	"context"
	"math"
	"testing"
	"time"
)

// modelPath / denoiseModelPath are the vendored models, relative to this
// package. Tests that need them skip when absent so `go test ./...` still works
// in a checkout without the model files.
const (
	modelPath        = "../../models/ten-vad.onnx"
	denoiseModelPath = "../../models/gtcrn_simple.onnx"
)

func TestNewDetectorRejectsNon16k(t *testing.T) {
	// Silero accepted 8kHz; TEN VAD does not. sherpa aborts the *process* from
	// C++ on a rate it cannot serve, so this must be caught in Go first.
	for _, rate := range []int{8000, 24000, 48000} {
		if _, err := NewDetector(DetectorConfig{ModelPath: modelPath, SampleRate: rate}); err == nil {
			t.Errorf("sample rate %d: want error, got nil", rate)
		}
	}
}

func TestNewDetectorRejectsEmptyModelPath(t *testing.T) {
	if _, err := NewDetector(DetectorConfig{SampleRate: 16000}); err == nil {
		t.Error("want error for empty model path, got nil")
	}
}

func TestNewDenoiserRejectsNon16k(t *testing.T) {
	if _, err := NewDenoiser(denoiseModelPath, 8000); err == nil {
		t.Error("want error for 8kHz, got nil")
	}
}

func TestEventKindString(t *testing.T) {
	if got := EventSpeechStarted.String(); got != "SPEECH_START" {
		t.Errorf("EventSpeechStarted = %q", got)
	}
	if got := EventSpeechStopped.String(); got != "SPEECH_END" {
		t.Errorf("EventSpeechStopped = %q", got)
	}
}

func TestClampToInt16Saturates(t *testing.T) {
	// Enhancement can push a sample past full scale. Wrapping would turn a loud
	// sample into a loud sample of the opposite sign, which is an audible click.
	cases := []struct {
		in   float32
		want int16
	}{
		{0, 0},
		{2.0, 32767},
		{-2.0, -32768},
		{1.5, 32767},
		{-1.5, -32768},
		{0.5, 16384},
	}
	for _, c := range cases {
		if got := clampToInt16(c.in); got != c.want {
			t.Errorf("clampToInt16(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func newTestDetector(t *testing.T) *Detector {
	t.Helper()
	d, err := NewDetector(DetectorConfig{
		ModelPath:  modelPath,
		SampleRate: 16000,
		Threshold:  0.5,
		MinSpeech:  200 * time.Millisecond,
		Hangover:   550 * time.Millisecond,
	})
	if err != nil {
		t.Skipf("ten-vad model unavailable: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestDetectorSilenceProducesNoSpeech(t *testing.T) {
	d := newTestDetector(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	silence := make([]int16, windowSamples)
	for i := 0; i < 100; i++ { // ~1.6s
		d.Submit(append([]int16(nil), silence...))
	}

	select {
	case ev := <-d.Events():
		t.Fatalf("silence produced %s", ev.Kind)
	case <-time.After(500 * time.Millisecond):
	}
	if d.Speaking() {
		t.Error("Speaking() true after silence")
	}
}

// TestDetectorDrainsSegmentBuffer guards the leak that sherpa's API invites:
// it buffers every completed speech segment for callers that want the audio,
// and we never read one. Undrained, that grows for the length of the call.
func TestDetectorDrainsSegmentBuffer(t *testing.T) {
	d := newTestDetector(t)

	// Alternate loud noise and silence so segments actually complete.
	noise := make([]int16, windowSamples)
	for i := range noise {
		noise[i] = int16(8000 * math.Sin(float64(i)*0.35))
	}
	silence := make([]int16, windowSamples)

	for cycle := 0; cycle < 8; cycle++ {
		for i := 0; i < 60; i++ {
			d.step(noise)
		}
		for i := 0; i < 60; i++ {
			d.step(silence)
		}
	}

	if !d.vad.IsEmpty() {
		t.Error("segment buffer not drained by step(); it will grow for the whole call")
	}
}

func TestDenoiserBuffersToFrameShift(t *testing.T) {
	dn, err := NewDenoiser(denoiseModelPath, 16000)
	if err != nil {
		t.Skipf("gtcrn model unavailable: %v", err)
	}
	defer dn.Close()

	shift := dn.FrameShift()
	if shift <= 0 {
		t.Fatalf("FrameShift() = %d", shift)
	}

	// A partial frame must produce nothing rather than a short read.
	if got := dn.Process(make([]int16, shift-1)); len(got) != 0 {
		t.Errorf("partial frame returned %d samples, want 0", len(got))
	}
	// Completing the first frame still produces nothing: GTCRN carries one
	// frame of algorithmic delay. Measured, not assumed — see the Denoiser doc
	// comment.
	if got := dn.Process(make([]int16, 1)); len(got) != 0 {
		t.Errorf("first frame returned %d samples, want 0 (one-frame delay)", len(got))
	}
	// From the second frame on, output is a full frame every time.
	for i := 0; i < 3; i++ {
		if got := dn.Process(make([]int16, shift)); len(got) != shift {
			t.Fatalf("frame %d returned %d samples, want %d", i+2, len(got), shift)
		}
	}
}

// TestDenoiserSteadyStateLagsByOneFrame pins the delay down as a number, so a
// model or sherpa upgrade that changes it fails here rather than silently
// shifting every latency figure the project reports.
func TestDenoiserSteadyStateLagsByOneFrame(t *testing.T) {
	dn, err := NewDenoiser(denoiseModelPath, 16000)
	if err != nil {
		t.Skipf("gtcrn model unavailable: %v", err)
	}
	defer dn.Close()

	shift := dn.FrameShift()
	frame := make([]int16, shift)
	const frames = 10

	var totalOut int
	for i := 0; i < frames; i++ {
		totalOut += len(dn.Process(frame))
	}

	wantIn := frames * shift
	if want := wantIn - shift; totalOut != want {
		t.Errorf("fed %d samples, got %d back; want %d (exactly one frame of delay)",
			wantIn, totalOut, want)
	}
}

// TestDenoiserResultNotAliased is the regression guard for a bug caught during
// implementation: Process handed its output to the VAD, which reads it on
// another goroutine, so a reused buffer would be overwritten mid-flight.
func TestDenoiserResultNotAliased(t *testing.T) {
	dn, err := NewDenoiser(denoiseModelPath, 16000)
	if err != nil {
		t.Skipf("gtcrn model unavailable: %v", err)
	}
	defer dn.Close()

	shift := dn.FrameShift()
	tone := make([]int16, shift)
	for i := range tone {
		tone[i] = int16(6000 * math.Sin(float64(i)*0.2))
	}

	first := dn.Process(tone)
	if len(first) == 0 {
		t.Skip("denoiser produced no output on first frame")
	}
	snapshot := append([]int16(nil), first...)

	for i := 0; i < 4; i++ {
		dn.Process(tone)
	}

	for i := range snapshot {
		if first[i] != snapshot[i] {
			t.Fatal("earlier Process result was mutated by a later call")
		}
	}
}
