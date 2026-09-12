// Package stt streams audio to Sarvam's speech-to-text WebSocket and emits
// transcripts plus the server-side VAD events (START_SPEECH / END_SPEECH) used
// for turn detection and barge-in.
//
// Transcripts and VAD signals are delivered on a *single ordered channel*
// (Events). They must not be split across separate channels: a consumer
// selecting over two channels sees them in arbitrary order, and a START_SPEECH
// processed after its own transcript cancels the turn that transcript just
// started. pipecat's Sarvam client routes both through one ordered handler for
// the same reason.
//
// Protocol reference: the production pipecat Sarvam STT client
// (api/patches/pipecat_sarvam_stt.py in the voice-ai-agent repo).
package stt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const sarvamSTTBaseURL = "wss://api.sarvam.ai/speech-to-text/ws"

// There is deliberately no keepalive here. Sarvam's STT socket rejects any
// message without an "audio" field — a {"type":"ping"} keepalive is answered
// with `Invalid request: 'audio' must not be None.` and an immediate close, so
// pinging an idle socket destroys it rather than preserving it. Verified
// directly against the API: an idle connection survives 75s+ untouched.

// EventKind classifies a stream event.
type EventKind int

const (
	// EventSpeechStarted is Sarvam's START_SPEECH VAD signal (barge-in).
	EventSpeechStarted EventKind = iota
	// EventSpeechStopped is Sarvam's END_SPEECH VAD signal.
	EventSpeechStopped
	// EventTranscript carries a finalized user utterance in Text.
	EventTranscript
)

// String implements fmt.Stringer so events read clearly in logs.
func (k EventKind) String() string {
	switch k {
	case EventSpeechStarted:
		return "START_SPEECH"
	case EventSpeechStopped:
		return "END_SPEECH"
	case EventTranscript:
		return "TRANSCRIPT"
	}
	return "UNKNOWN"
}

// Event is one item from the STT stream, delivered in the order Sarvam sent it.
type Event struct {
	Kind EventKind
	Text string // set for EventTranscript

	// At is when this event was received, on the local clock. It is the
	// reference for latency arithmetic, because everything it is subtracted
	// from is also local.
	//
	// Set for EventSpeechStarted/EventSpeechStopped. A transcript always
	// arrives after its own utterance's END_SPEECH on this same ordered
	// connection, so this timestamp is guaranteed to belong to the utterance
	// that produced the next transcript — no correlation against a separately
	// clocked signal is needed.
	At time.Time

	// RemoteAt is Sarvam's own "occured_at" for this event, on Sarvam's clock.
	// Zero when the field is absent.
	//
	// It is deliberately *not* used for latency maths. A live call measured the
	// gap between the two clocks at 3.857–3.892s across nine turns — a 35ms
	// spread over 90 seconds, which is a clock offset, not detection lag — and
	// every speech-end-anchored figure was inflated by that ~3.87s. Worse, once
	// the offset is removed, occured_at lands within ~35ms of local receipt and
	// ~400ms *after* the acoustic end of speech: it is Sarvam's detection
	// timestamp, roughly "when I sent this", not when the caller stopped
	// talking. It is kept because comparing it against At is what makes clock
	// skew visible instead of silently corrupting the numbers.
	// See docs/DECISIONS.md ADR-017.
	RemoteAt time.Time
}

// Config configures the Sarvam STT client.
type Config struct {
	APIKey     string
	Model      string // e.g. "saaras:v3"
	Language   string // e.g. "hi-IN" or "unknown"
	SampleRate int    // e.g. 16000
	Mode       string // e.g. "transcribe"

	// VAD sensitivity (saaras:v3). Defaults are deliberately less trigger-happy
	// than Sarvam's, because background conversation firing START_SPEECH lets
	// room noise hijack the turn. Zero values are omitted from the request.
	HighVADSensitivity      bool
	PositiveSpeechThreshold float64 // raise to require stronger speech
	NegativeSpeechThreshold float64
	MinSpeechFrames         int // raise to require sustained speech
}

// Client is a streaming Sarvam STT connection. SendPCM is expected to be called
// from a single goroutine (the audio ingress) and never blocks: if the socket is
// down it drops the frame and redials in the background.
type Client struct {
	cfg Config
	ctx context.Context

	connMu sync.RWMutex
	conn   *websocket.Conn

	writeMu      sync.Mutex
	reconnecting atomic.Bool

	// events is never closed by a per-connection read loop; consumers exit via
	// their own context so a dropped socket cannot tear down the pipeline.
	events chan Event
}

// New creates an unconnected client.
func New(cfg Config) *Client {
	if cfg.Mode == "" {
		cfg.Mode = "transcribe"
	}
	return &Client{
		cfg:    cfg,
		events: make(chan Event, 64),
	}
}

// Events yields transcripts and VAD signals in the order Sarvam sent them.
func (c *Client) Events() <-chan Event { return c.events }

// Connect dials the Sarvam STT WebSocket with server-side VAD enabled and starts
// the receive + keepalive loops.
func (c *Client) Connect(ctx context.Context) error {
	c.ctx = ctx
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.setConn(conn)
	log.Printf("stt: connected (model=%s lang=%s rate=%d mode=%s)",
		c.cfg.Model, c.cfg.Language, c.cfg.SampleRate, c.cfg.Mode)

	go c.receiveLoop(ctx, conn)
	return nil
}

func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	q := url.Values{}
	q.Set("model", c.cfg.Model)
	q.Set("language-code", c.cfg.Language)
	q.Set("mode", c.cfg.Mode)
	q.Set("sample_rate", fmt.Sprintf("%d", c.cfg.SampleRate))
	// Enable Sarvam's built-in VAD so we receive START_SPEECH / END_SPEECH
	// events for turn detection and barge-in. A local energy gate upstream
	// (internal/audio) keeps most room noise from reaching this point at all.
	q.Set("vad_signals", "true")
	q.Set("high_vad_sensitivity", strconv.FormatBool(c.cfg.HighVADSensitivity))
	if c.cfg.PositiveSpeechThreshold > 0 {
		q.Set("positive_speech_threshold", strconv.FormatFloat(c.cfg.PositiveSpeechThreshold, 'f', 2, 64))
	}
	if c.cfg.NegativeSpeechThreshold > 0 {
		q.Set("negative_speech_threshold", strconv.FormatFloat(c.cfg.NegativeSpeechThreshold, 'f', 2, 64))
	}
	if c.cfg.MinSpeechFrames > 0 {
		q.Set("min_speech_frames", strconv.Itoa(c.cfg.MinSpeechFrames))
	}

	endpoint := sarvamSTTBaseURL + "?" + q.Encode()
	header := http.Header{}
	header.Set("Api-Subscription-Key", c.cfg.APIKey)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, header)
	if err != nil {
		return nil, fmt.Errorf("sarvam stt dial: %w", err)
	}
	return conn, nil
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
}

func (c *Client) getConn() *websocket.Conn {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.conn
}

// invalidate clears conn if it is still the active one, so the next send
// triggers a redial.
func (c *Client) invalidate(conn *websocket.Conn) {
	c.connMu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.connMu.Unlock()
	conn.Close()
}

// triggerReconnect redials in the background. Reconnection is lazy (driven by
// send traffic, as pipecat does) rather than a standing retry loop, so a
// permanently rejected config costs nothing while idle instead of hammering the
// API and exhausting the rate limit.
func (c *Client) triggerReconnect() {
	if !c.reconnecting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.reconnecting.Store(false)
		if c.ctx == nil || c.ctx.Err() != nil {
			return
		}
		conn, err := c.dial(c.ctx)
		if err != nil {
			log.Printf("sarvam stt reconnect failed: %v", err)
			return
		}
		c.setConn(conn)
		log.Print("sarvam stt reconnected")
		go c.receiveLoop(c.ctx, conn)
	}()
}

// SendPCM sends a chunk of 16-bit little-endian mono PCM to Sarvam. It never
// blocks on a redial: callers run on the LiveKit decode goroutine.
func (c *Client) SendPCM(pcm []int16) error {
	conn := c.getConn()
	if conn == nil {
		c.triggerReconnect()
		return fmt.Errorf("stt not connected")
	}

	msg := audioMessage{}
	msg.Audio.Data = base64.StdEncoding.EncodeToString(pcm16ToBytes(pcm))
	msg.Audio.SampleRate = c.cfg.SampleRate
	msg.Audio.Encoding = "audio/wav"

	c.writeMu.Lock()
	err := conn.WriteJSON(msg)
	c.writeMu.Unlock()
	if err != nil {
		c.invalidate(conn)
		return err
	}
	return nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	conn := c.getConn()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

// receiveLoop owns one connection and exits when that connection dies; the next
// SendPCM redials.
func (c *Client) receiveLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		if ctx.Err() != nil {
			return
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			c.invalidate(conn)
			return
		}

		var m inboundMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Data.Error != "" {
			log.Printf("sarvam stt error: %s", m.Data.Error)
		}
		if m.Data.Message != "" {
			log.Printf("sarvam stt error: %s", m.Data.Message)
		}

		var ev Event
		switch m.Type {
		case "events":
			// Local receipt time is the reference; Sarvam's own timestamp rides
			// along only so the gap between the clocks can be reported.
			at := time.Now()
			remoteAt, _ := sarvamTimestamp(m.Data.OccuredAt)
			switch m.Data.SignalType {
			case "START_SPEECH":
				ev = Event{Kind: EventSpeechStarted, At: at, RemoteAt: remoteAt}
			case "END_SPEECH":
				ev = Event{Kind: EventSpeechStopped, At: at, RemoteAt: remoteAt}
			default:
				continue
			}
		case "data":
			if m.Data.Transcript == "" {
				continue
			}
			ev = Event{Kind: EventTranscript, Text: m.Data.Transcript}
		default:
			continue
		}

		if ev.Kind == EventTranscript {
			log.Printf("stt: %s %q", ev.Kind, ev.Text)
		} else {
			log.Printf("stt: %s", ev.Kind)
		}

		select {
		case c.events <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// pcm16ToBytes serializes int16 samples as little-endian bytes.
func pcm16ToBytes(pcm []int16) []byte {
	out := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		out[i*2] = byte(s)
		out[i*2+1] = byte(s >> 8)
	}
	return out
}

type audioMessage struct {
	Audio struct {
		Data       string `json:"data"`
		SampleRate int    `json:"sample_rate"`
		Encoding   string `json:"encoding"`
	} `json:"audio"`
}

type inboundMessage struct {
	Type string `json:"type"`
	Data struct {
		Transcript   string `json:"transcript"`
		LanguageCode string `json:"language_code"`
		SignalType   string `json:"signal_type"`
		Error        string `json:"error"`
		// Sarvam reports failures here, not in Error — reading only Error hid
		// every STT-side problem until it was found by probing the API.
		Message string `json:"message"`
		// OccuredAt is a fractional Unix timestamp on START_SPEECH/END_SPEECH
		// events, e.g. 1786013420.5537887 — Sarvam's own clock, not ours.
		// Sarvam's spelling, not a typo on our part.
		OccuredAt float64 `json:"occured_at"`
	} `json:"data"`
}

// sarvamTimestamp converts Sarvam's fractional-seconds Unix timestamp to a
// time.Time. ok is false for the zero value, which is indistinguishable from
// "field absent" in JSON and should fall back to a local timestamp rather than
// silently claiming 1970.
func sarvamTimestamp(unixSeconds float64) (t time.Time, ok bool) {
	if unixSeconds == 0 {
		return time.Time{}, false
	}
	sec := int64(unixSeconds)
	nsec := int64((unixSeconds - float64(sec)) * float64(time.Second))
	return time.Unix(sec, nsec), true
}
