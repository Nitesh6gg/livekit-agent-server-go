// Command agent is the entry point for the Go voice agent worker.
//
// Phase 1: join one LiveKit room and run a single Sarvam STT -> Gemini ->
// Sarvam TTS conversation loop with barge-in. See docs/ROADMAP.md.
package main

import (
	"context"
	"log"
	"net/http"
	"net/http/pprof"
	"os/signal"
	"syscall"
	"time"

	"github.com/go2market/go-agent-worker/config"
	"github.com/go2market/go-agent-worker/internal/agent"
	"github.com/go2market/go-agent-worker/internal/audio"
	"github.com/go2market/go-agent-worker/internal/livekit"
	"github.com/go2market/go-agent-worker/internal/llm"
	"github.com/go2market/go-agent-worker/internal/metrics"
	"github.com/go2market/go-agent-worker/internal/stt"
	"github.com/go2market/go-agent-worker/internal/tts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const systemPrompt = "You are a helpful, concise voice assistant speaking Hindi. " +
	"Keep replies short and conversational, suitable for being spoken aloud."

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go serveOps(cfg.HTTPAddr, cfg.PprofEnabled)

	// STT: connect first so we can forward caller audio as soon as it arrives.
	sttClient := stt.New(stt.Config{
		APIKey:                  cfg.SarvamAPIKey,
		Model:                   cfg.STTModel,
		Language:                cfg.STTLanguage,
		SampleRate:              cfg.STTSampleRate,
		HighVADSensitivity:      cfg.STTHighVADSensitivity,
		PositiveSpeechThreshold: cfg.STTPositiveSpeechThreshold,
		NegativeSpeechThreshold: cfg.STTNegativeSpeechThreshold,
		MinSpeechFrames:         cfg.STTMinSpeechFrames,
	})
	if err := sttClient.Connect(ctx); err != nil {
		log.Fatalf("stt connect: %v", err)
	}
	defer sttClient.Close()

	// TTS.
	ttsClient := tts.New(tts.Config{
		APIKey:     cfg.SarvamAPIKey,
		Model:      cfg.TTSModel,
		Voice:      cfg.TTSVoice,
		Language:   cfg.STTLanguage,
		SampleRate: cfg.TTSSampleRate,
	})
	if err := ttsClient.Connect(ctx); err != nil {
		log.Fatalf("tts connect: %v", err)
	}
	defer ttsClient.Close()

	// LLM.
	llmClient, err := llm.New(ctx, llm.Config{APIKey: cfg.GeminiAPIKey, Model: cfg.GeminiModel})
	if err != nil {
		log.Fatalf("llm init: %v", err)
	}

	// Local VAD drives barge-in. It does not gate what is sent to Sarvam: the
	// STT socket stays continuous, because dropping chunks breaks Sarvam's
	// endpointing (see docs/DECISIONS.md ADR-012).
	//
	// Failure to load is not fatal — the pipeline falls back to Sarvam's
	// server-side VAD, which is what runs on hosts missing the model.
	var vad *audio.Detector
	if cfg.VADEnabled {
		vad, err = audio.NewDetector(audio.DetectorConfig{
			ModelPath:  cfg.VADModelPath,
			SampleRate: cfg.STTSampleRate,
			Threshold:  cfg.VADThreshold,
			MinSpeech:  cfg.VADMinSpeech,
			Hangover:   cfg.VADHangover,
		})
		if err != nil {
			log.Printf("local VAD unavailable, falling back to Sarvam VAD: %v", err)
			vad = nil
		} else {
			defer vad.Close()
			go vad.Run(ctx)
			log.Printf("local VAD: ten-vad threshold=%.2f minSpeech=%s hangover=%s",
				cfg.VADThreshold, cfg.VADMinSpeech, cfg.VADHangover)
		}
	} else {
		log.Print("local VAD disabled by config; using Sarvam server-side VAD")
	}

	// Speech enhancement, when enabled, sits in front of everything else: both
	// the VAD and Sarvam see denoised audio.
	//
	// Failure to load is not fatal either. Enhancement is an improvement to the
	// input, not a requirement for the call to work, and refusing to start a
	// call over it would be the wrong trade.
	var denoiser *audio.Denoiser
	if cfg.DenoiseEnabled {
		denoiser, err = audio.NewDenoiser(cfg.DenoiseModelPath, cfg.STTSampleRate)
		if err != nil {
			log.Printf("speech enhancement unavailable, continuing without it: %v", err)
			denoiser = nil
		} else {
			defer denoiser.Close()
			log.Printf("speech enhancement: gtcrn frameShift=%d samples (~%dms added latency)",
				denoiser.FrameShift(), denoiser.FrameShift()*1000/cfg.STTSampleRate)
		}
	} else {
		log.Print("speech enhancement disabled by config (GTCRN_ENABLED)")
	}

	// LiveKit: caller audio is forwarded into STT.
	session, err := livekit.Connect(ctx, livekit.Params{
		URL:           cfg.LiveKitURL,
		APIKey:        cfg.LiveKitAPIKey,
		APISecret:     cfg.LiveKitAPISecret,
		Room:          cfg.Room,
		Identity:      cfg.Identity,
		STTSampleRate: cfg.STTSampleRate,
		TTSSampleRate: cfg.TTSSampleRate,
		OnCallerAudio: func(pcm []int16) {
			if denoiser != nil {
				// Denoise inline rather than on its own goroutine: STT and the
				// VAD must see the same stream in the same order, and a second
				// queue here would be one more place for the two to drift
				// apart. Returns whole frames only, so it is empty until one
				// has accumulated.
				pcm = denoiser.Process(pcm)
				if len(pcm) == 0 {
					return
				}
			}
			// Every frame goes to Sarvam: the STT stream stays continuous.
			// Best-effort — drop on transient write errors rather than block
			// the SDK's decode goroutine.
			_ = sttClient.SendPCM(pcm)
			if vad != nil {
				// Non-blocking; inference runs on the detector's goroutine.
				vad.Submit(pcm)
			}
		},
	})
	if err != nil {
		log.Fatalf("livekit connect: %v", err)
	}
	defer session.Disconnect()

	log.Printf("agent joined room %q as %q; waiting for caller audio", cfg.Room, cfg.Identity)

	p := agent.New(sttClient, llmClient, ttsClient, session, vad, cfg.TTSSampleRate, systemPrompt)
	go p.Run(ctx)

	<-ctx.Done()
	log.Println("shutting down")
}

// serveOps exposes health, metrics and (optionally) pprof on the stdlib HTTP
// server. Prometheus is used for the metrics format and collectors only — there is
// still no web framework here (see docs/DECISIONS.md ADR-006).
func serveOps(addr string, enablePprof bool) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		// process_cpu_seconds_total and process_resident_memory_bytes are the
		// per-call CPU/RAM inputs METRICS.md needs; they read /proc, so they
		// report only on Linux.
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)
	metrics.MustRegister(reg)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	// Registered explicitly rather than by importing net/http/pprof for its side
	// effect: that import installs the handlers on http.DefaultServeMux, which this
	// server deliberately does not use. pprof.Index also serves the named profiles
	// (heap, goroutine, allocs, block, mutex, threadcreate) under the same prefix.
	if enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		log.Printf("pprof enabled at %s/debug/pprof/ — disable with PPROF_ENABLED=false", addr)
	}

	// No WriteTimeout on purpose: /debug/pprof/profile?seconds=N streams for N
	// seconds and a write deadline would truncate every profile longer than it.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("ops server: %v", err)
	}
}
