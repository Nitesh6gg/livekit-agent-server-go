// Package config loads runtime configuration from the environment
// (see .env.example for the full list of variables).
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime settings for the agent worker.
type Config struct {
	// LiveKit media plane
	LiveKitURL       string
	LiveKitAPIKey    string
	LiveKitAPISecret string
	Room             string
	Identity         string

	// STT (Sarvam)
	SarvamAPIKey  string
	STTModel      string
	STTLanguage   string
	STTSampleRate int

	// STT VAD sensitivity (saaras:v3)
	STTHighVADSensitivity      bool
	STTPositiveSpeechThreshold float64
	STTNegativeSpeechThreshold float64
	STTMinSpeechFrames         int

	// Local VAD (TEN VAD via sherpa-onnx, see internal/audio). Drives barge-in
	// decisions instead of Sarvam's server-side VAD. It does not gate the audio
	// sent to Sarvam — the STT socket stays continuous — so it changes
	// interruption quality, not the STT bill.
	VADEnabled   bool
	VADModelPath string
	VADThreshold float64
	VADHangover  time.Duration
	VADMinSpeech time.Duration

	// Speech enhancement (GTCRN via sherpa-onnx). When enabled, caller audio is
	// denoised before it reaches both the VAD and Sarvam STT.
	//
	// Off by default, and deliberately so: it adds ~16ms to the STT path, and
	// altering what Sarvam hears is a change with real downside risk — ASR is
	// trained on unprocessed audio, and over-suppression costs recognition
	// accuracy. Turn it on as a measured experiment, not as a default.
	DenoiseEnabled   bool
	DenoiseModelPath string

	// TTS (Sarvam)
	TTSModel      string
	TTSVoice      string
	TTSSampleRate int

	// LLM (Gemini)
	GeminiAPIKey string
	GeminiModel  string

	// Runtime
	LogLevel string
	HTTPAddr string
}

// Load reads configuration from the environment, first loading a .env file
// from the current directory if one exists (real environment variables always
// win over .env values).
func Load() (*Config, error) {
	loadDotEnv(".env")

	c := &Config{
		LiveKitURL:       getenv("LIVEKIT_URL", "ws://localhost:7880"),
		LiveKitAPIKey:    os.Getenv("LIVEKIT_API_KEY"),
		LiveKitAPISecret: os.Getenv("LIVEKIT_API_SECRET"),
		Room:             getenv("LIVEKIT_ROOM", "agent-spike-1"),
		Identity:         getenv("LIVEKIT_IDENTITY", "go-agent-worker"),

		SarvamAPIKey:  os.Getenv("SARVAM_API_KEY"),
		STTModel:      getenv("SARVAM_STT_MODEL", "saaras:v4"),
		STTLanguage:   getenv("SARVAM_STT_LANGUAGE", "hi-IN"),
		STTSampleRate: getenvInt("AUDIO_SAMPLE_RATE", 16000),

		STTHighVADSensitivity:      getenvBool("STT_HIGH_VAD_SENSITIVITY", false),
		STTPositiveSpeechThreshold: getenvFloat("STT_POSITIVE_SPEECH_THRESHOLD", 0.7),
		STTNegativeSpeechThreshold: getenvFloat("STT_NEGATIVE_SPEECH_THRESHOLD", 0.5),
		STTMinSpeechFrames:         getenvInt("STT_MIN_SPEECH_FRAMES", 10),

		VADEnabled:   getenvBool("VAD_ENABLED", true),
		VADModelPath: getenv("VAD_MODEL_PATH", "models/ten-vad.onnx"),
		VADThreshold: getenvFloat("VAD_THRESHOLD", 0.5),
		VADHangover:  time.Duration(getenvInt("VAD_HANGOVER_MS", 550)) * time.Millisecond,
		VADMinSpeech: time.Duration(getenvInt("VAD_MIN_SPEECH_MS", 200)) * time.Millisecond,

		DenoiseEnabled:   getenvBool("GTCRN_ENABLED", false),
		DenoiseModelPath: getenv("GTCRN_MODEL_PATH", "models/gtcrn_simple.onnx"),

		TTSModel:      getenv("SARVAM_TTS_MODEL", "bulbul:v2"),
		TTSVoice:      getenv("SARVAM_TTS_VOICE", "anushka"),
		TTSSampleRate: getenvInt("SARVAM_TTS_SAMPLE_RATE", 24000),

		GeminiAPIKey: os.Getenv("GEMINI_API_KEY"),
		GeminiModel:  getenv("GEMINI_MODEL", "gemini-2.5-flash"),

		LogLevel: getenv("LOG_LEVEL", "info"),
		HTTPAddr: getenv("HTTP_ADDR", ":8080"),
	}

	var missing []string
	if c.LiveKitAPIKey == "" {
		missing = append(missing, "LIVEKIT_API_KEY")
	}
	if c.LiveKitAPISecret == "" {
		missing = append(missing, "LIVEKIT_API_SECRET")
	}
	if c.SarvamAPIKey == "" {
		missing = append(missing, "SARVAM_API_KEY")
	}
	if c.GeminiAPIKey == "" {
		missing = append(missing, "GEMINI_API_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getenvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// loadDotEnv parses a simple KEY=VALUE file and sets any variables that are not
// already present in the environment. Lines starting with '#' and blank lines
// are ignored. Surrounding quotes on values are stripped. Missing file is fine.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, val)
			}
		}
	}
}
